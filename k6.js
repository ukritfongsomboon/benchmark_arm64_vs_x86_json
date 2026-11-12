// One k6 run = complete comparison. For every endpoint, each encoder is
// tested in turn (never at the same time) against the same server process,
// then a side-by-side table is printed.
//
//   k6 run k6.js                                      # ARM: std vs neon (max throughput)
//   k6 run -e ENCODERS=std k6.js                      # x86 (no NEON)
//   k6 run -e ENCODERS=std,scalar,neon k6.js          # + scalar control (SIMD-only effect)
//   k6 run -e RATE=3000 -e DURATION=30 k6.js          # fixed arrival rate
//   k6 run -e VUS=64 k6.js                            # more concurrent connections
//
// Env: URL (http://localhost:8080) ENCODERS (std,neon) ENDPOINTS (small,medium,large,escape,transform)
//      MODE (max|rate) RATE (1000 req/s, MODE=rate only) VUS (32) MAX_VUS (2000) DURATION (20 s) WARMUP (5 s)
//      TEXT_KB (16, /escape payload) USERS (100, /transform payload) OUT (optional JSON file)
import http from 'k6/http';
import { check, fail } from 'k6';
import { Gauge } from 'k6/metrics';

const URL = (__ENV.URL || 'http://localhost:8080').replace(/\/$/, '');
const ENCODERS = (__ENV.ENCODERS || 'std,neon').split(',');
for (const e of ENCODERS) {
  if (!['std', 'scalar', 'neon'].includes(e)) throw new Error(`unknown encoder "${e}" (use std, scalar, neon)`);
}
const ENDPOINTS = (__ENV.ENDPOINTS || 'small,medium,large,escape,transform').split(',');
const MODE = __ENV.MODE || 'max';
const RATE = +(__ENV.RATE || 1000);
const VUS = +(__ENV.VUS || 32);
const MAX_VUS = +(__ENV.MAX_VUS || 2000);
const DUR = +(__ENV.DURATION || 20);
const WARM = +(__ENV.WARMUP || 5);
const GAP = 3; // seconds after each phase: graceful stop + CPU mark

// Request bodies (deterministic, identical for every encoder).
const words = 'lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor'.split(' ');
let text = '';
for (let i = 0; text.length < +(__ENV.TEXT_KB || 16) * 1024; i++) {
  text += words[(i * 7) % words.length] + (i % 14 === 0 ? ['"', '\n', '\\', '<', '&'][i % 5] : ' ');
}
const users = [];
for (let i = 0; i < +(__ENV.USERS || 100); i++) {
  users.push({ id: i + 1, name: ['Somchai', 'Suda', 'Anan', 'สมชาย'][i % 4], message: `Hello "World" #${i}\nARM64 NEON <ok> & done` });
}
const BODY = { escape: JSON.stringify({ text }), transform: JSON.stringify({ users }) };
const JSON_HDR = { headers: { 'Content-Type': 'application/json' } };

function request(ep, enc) {
  const path = `${URL}/${enc}/json/${ep}`;
  return BODY[ep] ? ['POST', path, BODY[ep], JSON_HDR] : ['GET', path, null, {}];
}

// Timeline: [warmup][measure][gap] per (endpoint, encoder), sequential.
const scenarios = {};
const thresholds = {};
const phases = [];
let t = 0;
const load = (extra) => (MODE === 'max'
  ? { executor: 'constant-vus', vus: VUS, ...extra }
  : { executor: 'constant-arrival-rate', rate: RATE, timeUnit: '1s', preAllocatedVUs: VUS, maxVUs: MAX_VUS, ...extra });
for (const ep of ENDPOINTS) {
  for (const enc of ENCODERS) {
    const id = `${ep}_${enc}`;
    phases.push({ id, ep, enc });
    scenarios[`warm_${id}`] = load({ exec: 'hit', env: { EP: ep, ENC: enc }, startTime: `${t}s`, duration: `${WARM}s`, gracefulStop: '1s' });
    scenarios[id] = load({ exec: 'hit', env: { EP: ep, ENC: enc }, startTime: `${t + WARM}s`, duration: `${DUR}s`, gracefulStop: '2s' });
    scenarios[`mark0_${id}`] = { executor: 'shared-iterations', vus: 1, iterations: 1, exec: 'mark', env: { MARK: `${id}_start` }, startTime: `${t + WARM}s`, maxDuration: '5s' };
    scenarios[`mark1_${id}`] = { executor: 'shared-iterations', vus: 1, iterations: 1, exec: 'mark', env: { MARK: `${id}_end` }, startTime: `${t + WARM + DUR + 2}s`, maxDuration: '5s' };
    // Declaring sub-metric thresholds makes k6 report per-phase values.
    thresholds[`http_req_duration{scenario:${id}}`] = ['max>=0'];
    thresholds[`http_reqs{scenario:${id}}`] = ['count>=0'];
    thresholds[`http_req_failed{scenario:${id}}`] = ['rate>=0'];
    thresholds[`dropped_iterations{scenario:${id}}`] = ['count>=0'];
    thresholds[`server_cpu_seconds{mark:${id}_start}`] = ['value>=0'];
    thresholds[`server_cpu_seconds{mark:${id}_end}`] = ['value>=0'];
    t += WARM + DUR + GAP;
  }
}

export const options = {
  scenarios,
  thresholds,
  discardResponseBodies: true,
  summaryTrendStats: ['avg', 'med', 'p(95)', 'p(99)', 'max'],
};

const cpu = new Gauge('server_cpu_seconds');

export function setup() {
  const res = http.get(`${URL}/info`, { responseType: 'text' });
  if (res.status !== 200) fail(`cannot reach ${URL}/info (status ${res.status}) - is the server running?`);
  const info = res.json();
  if (ENCODERS.includes('neon') && !info.neon) {
    fail(`server (${info.goarch}) has no NEON: run with -e ENCODERS=std (or std,scalar)`);
  }
  return info;
}

export function hit() {
  const [method, url, body, params] = request(__ENV.EP, __ENV.ENC);
  const res = http.request(method, url, body, params);
  check(res, { 'status 200': (r) => r.status === 200 });
}

export function mark() {
  const res = http.get(`${URL}/cpu`, { responseType: 'text' });
  cpu.add(parseFloat(res.body), { mark: __ENV.MARK });
}

// ------------------------------------------------------------- summary ----

const val = (data, name) => (data.metrics[name] || {}).values || {};
const pad = (s, n) => String(s).padStart(n);
const f = (x, d = 2) => (x === undefined || x === null || Number.isNaN(x) ? '-' : x.toFixed(d));

export function handleSummary(data) {
  const info = data.setup_data || {};
  const rows = phases.map(({ id, ep, enc }) => {
    const d = val(data, `http_req_duration{scenario:${id}}`);
    const n = val(data, `http_reqs{scenario:${id}}`).count || 0;
    const cpuS = (val(data, `server_cpu_seconds{mark:${id}_end}`).value || 0) - (val(data, `server_cpu_seconds{mark:${id}_start}`).value || 0);
    return {
      endpoint: ep, encoder: enc, requests: n, rps: n / DUR,
      p50_ms: d.med, p95_ms: d['p(95)'], p99_ms: d['p(99)'],
      error_rate: val(data, `http_req_failed{scenario:${id}}`).rate || 0,
      dropped: val(data, `dropped_iterations{scenario:${id}}`).count || 0,
      server_cpu_cores: cpuS / (DUR + 2),
      cpu_ms_per_1k_req: n ? (cpuS * 1e6) / n : undefined,
    };
  });

  const L = [];
  L.push('');
  L.push(`  server: goarch=${info.goarch} neon=${info.neon} go=${info.go} gomaxprocs=${info.gomaxprocs}   url=${URL}`);
  L.push(`  mode=${MODE}${MODE === 'max' ? ` vus=${VUS}` : ` rate=${RATE}/s`} duration=${DUR}s warmup=${WARM}s`);
  L.push('');
  L.push(`  ${'endpoint'.padEnd(10)} ${'encoder'.padEnd(7)} ${pad('rps', 9)} ${pad('p50 ms', 8)} ${pad('p95 ms', 8)} ${pad('p99 ms', 8)} ${pad('err %', 6)} ${pad('dropped', 8)} ${pad('srv cores', 9)} ${pad('cpu ms/1k req', 13)}`);
  L.push('  ' + '-'.repeat(98));
  for (const ep of ENDPOINTS) {
    const rs = rows.filter((r) => r.endpoint === ep);
    for (const r of rs) {
      L.push(`  ${ep.padEnd(10)} ${r.encoder.padEnd(7)} ${pad(f(r.rps, 0), 9)} ${pad(f(r.p50_ms, 3), 8)} ${pad(f(r.p95_ms, 3), 8)} ${pad(f(r.p99_ms, 3), 8)} ${pad(f(r.error_rate * 100, 2), 6)} ${pad(r.dropped, 8)} ${pad(f(r.server_cpu_cores, 2), 9)} ${pad(f(r.cpu_ms_per_1k_req, 1), 13)}`);
    }
    const base = rs.find((r) => r.encoder === 'std');
    for (const r of rs) {
      if (!base || r === base) continue;
      L.push(`  ${''.padEnd(10)} ${('→ ' + r.encoder + ' vs std').padEnd(18)}  rps x${f(r.rps / base.rps, 2)}   p99 x${f(r.p99_ms / base.p99_ms, 2)}   cpu/req x${f(r.cpu_ms_per_1k_req / base.cpu_ms_per_1k_req, 2)}`);
    }
    L.push('');
  }
  L.push('  How to read: in MODE=rate every encoder gets the same load, so compare "cpu ms/1k req" (lower = better) and latency.');
  L.push('  In MODE=max compare "rps" (higher = better). err % or dropped > 0 means the server could not keep up.');
  L.push('  x < 1 for cpu/req and p99 is better; x > 1 for rps is better.');
  L.push('');

  const out = { stdout: L.join('\n') + '\n' };
  if (__ENV.OUT) out[__ENV.OUT] = JSON.stringify({ server: info, mode: MODE, rate: RATE, vus: VUS, duration_s: DUR, rows }, null, 2);
  return out;
}
