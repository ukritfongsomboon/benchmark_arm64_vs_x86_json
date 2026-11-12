# go-neon-simple

เทียบความเร็ว JSON encoding ของ Go ระหว่าง **x86 standard**, **ARM standard** และ **ARM + NEON (SIMD)**
แบบง่ายที่สุด: server ตัวเดียว + k6 script ตัวเดียว รันจบแล้ว **k6 พิมพ์ตารางเทียบให้เลย**

ไม่ใช้ Docker, ไม่ใช้ cgo, ไม่มี dependency นอก standard library

---

## ไฟล์ทั้งหมด

| ไฟล์ | ทำอะไร |
|---|---|
| `main.go` | HTTP server + encoder ทุกแบบ (`std`, `neon`, `scalar`) |
| `neon_arm64.go` | ประกาศฟังก์ชัน NEON (คอมไพล์เฉพาะ ARM64) |
| `neon_arm64.s` | ตัวสแกน NEON เขียนด้วย Go assembly (คอมไพล์เฉพาะ ARM64) |
| `neon_other.go` | stub สำหรับ x86 (ไม่มี NEON) |
| `main_test.go` | ตรวจว่าทุก encoder ให้ผล **ตรงกับ `encoding/json` ทุก byte** |
| `k6.js` | ยิงโหลดและพิมพ์ตารางเทียบ |

---

## 3 รูปแบบที่วัด

| # | รูปแบบ | รันบน | k6 ยิงที่ |
|---|---|---|---|
| 1 | x86 standard (`encoding/json`) | x86 VM | `/std/...` |
| 2 | ARM standard (`encoding/json`) | ARM VM | `/std/...` |
| 3 | ARM NEON (encoder ที่เขียนเอง + NEON) | ARM VM (เครื่องเดียวกับ #2) | `/neon/...` |

server ตัวเดียวมีทุก encoder เลือกด้วย path:

```
GET  /std/json/small     GET  /neon/json/small      (1 record, 596 B)
GET  /std/json/medium    GET  /neon/json/medium     (100 records, ~58 KB)
GET  /std/json/large     GET  /neon/json/large      (1,000 records, ~578 KB)
POST /std/json/escape    POST /neon/json/escape     {"text": "..."}
POST /std/json/transform POST /neon/json/transform  {"users": [...]}
GET  /ping   GET /info   GET /cpu
```

บน x86 จะไม่มี `/neon/...` (ตอบ 404)

> **(ไม่บังคับ)** `/scalar/...` = encoder ตัวเดียวกับ `neon` แต่ **ไม่ใช้ SIMD**
> ใช้แยกว่าความเร็วที่ได้มาจาก NEON เท่าไหร่ และจากการเขียน encoder แบบไม่ใช้ reflection เท่าไหร่
> (`neon` vs `std` = ผลรวมทั้งสองอย่าง, `neon` vs `scalar` = ผลของ SIMD อย่างเดียว)

---

## สิ่งที่ต้องมีบนเครื่อง

- **Go 1.25+** (หรือใช้ Docker build ให้ — ดูข้างล่าง)
- **k6** — https://grafana.com/docs/k6/latest/set-up/install-k6/ (หรือใช้ Docker image `grafana/k6`)
- `taskset` (มีใน Linux อยู่แล้ว, package `util-linux`)

ตรวจเครื่อง:

```bash
uname -m                              # aarch64 = ARM, x86_64 = x86
nproc                                 # จำนวน CPU
lscpu | grep -i 'thread(s) per core'  # 1 = ไม่มี hyperthreading
grep -m1 -o -w asimd /proc/cpuinfo    # ARM: ต้องเห็น asimd (= NEON)
```

---

## วิธีรัน (เครื่องเดียว: เป็นทั้ง server และ k6)

แบ่ง CPU ให้ server กับ k6 คนละชุด (ตัวอย่างเครื่อง 8 vCPU):

```
core 0-3 = server        core 4-7 = k6
```

### 1. Build และ test

```bash
git clone <repo-url> go-neon-simple && cd go-neon-simple
go test ./...
go build -o server .
```

ผลที่ต้องเห็น: `ok  go-neon-simple`

ถ้าไม่มี Go แต่มี Docker:

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.25.4-alpine sh -c 'go test ./... && go build -o server .'
```

หรือ build จาก Mac/PC แล้ว copy ไป:

```bash
GOOS=linux GOARCH=arm64 go build -o server-arm64 .     # ARM VM
GOOS=linux GOARCH=amd64 go build -o server-amd64 .     # x86 VM
scp server-arm64 k6.js user@ARM_VM:~/
```

### 2. เปิด server (terminal ที่ 1)

```bash
taskset -c 0-3 ./server
```

ผลที่ต้องเห็น (ARM):

```
goarch=arm64 neon=true go=go1.25.4 gomaxprocs=4 encoders=[std scalar neon] addr=:8080
```

`gomaxprocs=4` แปลว่า server ใช้ 4 core ตามที่ `taskset` กำหนด

### 3. ยิง k6 (terminal ที่ 2)

**ARM VM** (รูปแบบ #2 และ #3):

```bash
taskset -c 4-7 k6 run -e OUT=results-arm.json k6.js | tee results-arm.txt
```

**x86 VM** (รูปแบบ #1):

```bash
taskset -c 4-7 k6 run -e ENCODERS=std -e OUT=results-x86.json k6.js | tee results-x86.txt
```

ใช้เวลา: ARM ~5 นาที, x86 ~2.5 นาที

ถ้าไม่ได้ติดตั้ง k6 ใช้ Docker แทน:

```bash
docker run --rm -i --network host --cpuset-cpus 4-7 -v "$PWD":/w -w /w grafana/k6:1.3.0 run -e OUT=results-arm.json k6.js
```

### 4. ผลที่ได้

k6 จะพิมพ์ตารางแบบนี้ (ตัวอย่างจาก Apple M4 Max, server กับ k6 ไม่ได้แยก core — **ไม่ใช่ผลจริงบน server**):

```
  server: goarch=arm64 neon=true go=go1.25.4 gomaxprocs=14   url=http://localhost:8080
  mode=max vus=32 duration=8s warmup=2s

  endpoint   encoder       rps   p50 ms   p95 ms   p99 ms  err %  dropped srv cores cpu ms/1k req
  --------------------------------------------------------------------------------------------------
  large      std         13347    1.948    5.415    8.172   0.00        0      8.54         800.1
  large      neon        16874    1.592    3.751    6.606   0.00        0      6.70         496.0
             → neon vs std       rps x1.26   p99 x0.81   cpu/req x0.62

  medium     std         68415    0.393    0.853    1.598   0.00        0      5.85         106.8
  medium     neon        74316    0.339    0.783    1.654   0.00        0      4.78          80.4
             → neon vs std       rps x1.09   p99 x1.04   cpu/req x0.75
```

| คอลัมน์ | ความหมาย |
|---|---|
| `rps` | request ต่อวินาทีที่ทำได้จริง — **สูงกว่า = ดีกว่า** |
| `p50/p95/p99 ms` | latency — ต่ำกว่า = ดีกว่า |
| `err %`, `dropped` | ต้องเป็น 0 (ไม่งั้น server รับไม่ไหว) |
| `srv cores` | server ใช้ CPU กี่ core โดยเฉลี่ย |
| `cpu ms/1k req` | CPU ของ server ต่อ 1,000 request — **ต่ำกว่า = ดีกว่า** |
| `→ neon vs std` | `rps x1.26` = เร็วขึ้น 26%, `cpu/req x0.62` = ใช้ CPU น้อยลง 38% |

### 5. เทียบ x86 กับ ARM

เทียบแถว `std` ของไฟล์ `results-x86.txt` กับแถว `std` และ `neon` ของ `results-arm.txt` ที่ endpoint เดียวกัน

ต้องใช้ค่าเหมือนกันทั้งสองเครื่อง: จำนวน core ของ server (`taskset`), `VUS`, `DURATION`
และควรระบุในรายงาน: รุ่น CPU (`lscpu`), จำนวน physical core, ราคา VM

---

## ตั้งค่า k6

ส่งผ่าน `-e KEY=VALUE`:

| ตัวแปร | default | ความหมาย |
|---|---|---|
| `URL` | `http://localhost:8080` | ที่อยู่ server |
| `ENCODERS` | `std,neon` | encoder ที่จะเทียบ (x86 ใช้ `std`; เพิ่ม `scalar` เพื่อแยกผล SIMD) |
| `ENDPOINTS` | `small,medium,large,escape,transform` | endpoint ที่จะยิง |
| `MODE` | `max` | `max` = ยิงเต็มที่ด้วย `VUS` connection (เทียบ rps) / `rate` = ยิงคงที่ `RATE` req/s (เทียบ CPU/latency) |
| `VUS` | `32` | จำนวน connection พร้อมกัน |
| `RATE` | `1000` | req/s (เฉพาะ `MODE=rate`) |
| `DURATION` | `20` | วินาทีที่วัด ต่อ endpoint ต่อ encoder |
| `WARMUP` | `5` | วินาที warmup ก่อนวัด (ไม่นับผล) |
| `TEXT_KB` | `16` | ขนาดข้อความของ `/json/escape` (KB) |
| `USERS` | `100` | จำนวน user ของ `/json/transform` |
| `OUT` | — | เขียนผลเป็น JSON ลงไฟล์ |

ตัวอย่าง:

```bash
# เฉพาะ large, วัดนานขึ้น
taskset -c 4-7 k6 run -e ENDPOINTS=large -e DURATION=60 k6.js

# แยกผลของ SIMD (neon vs scalar)
taskset -c 4-7 k6 run -e ENCODERS=std,scalar,neon k6.js

# ยิงคงที่ 3000 req/s แล้วดู CPU ต่อ request
taskset -c 4-7 k6 run -e MODE=rate -e RATE=3000 k6.js
```

encoder ถูกยิง **ทีละตัว ต่อกัน** (ไม่ยิงพร้อมกัน) บน server process เดียวกัน: `warmup → วัด → พัก 3 วินาที` แล้วค่อยไป encoder ถัดไป

---

## ข้อควรรู้

- **ถ้า `srv cores` ของ server ยังไม่ถึงจำนวน core ที่ให้ แต่ rps ไม่ขึ้น** → k6 อาจเป็นคอขวด (ดู `top`) เพิ่ม `VUS` หรือให้ core กับ k6 มากขึ้น
- **x86 ที่มี hyperthreading** (`Thread(s) per core: 2`): ดู `lscpu -e=CPU,CORE` แล้วให้ server กับ k6 อยู่คนละ **physical core** เช่น ถ้า CPU 0 กับ 4 คือ core เดียวกัน ใช้ `taskset -c 0,1,4,5 ./server` และ `taskset -c 2,3,6,7 k6 run ...`
- **`neon` vs `std` รวม 2 อย่าง**: (1) encoder ที่เขียนเองไม่ใช้ reflection และ (2) NEON — ถ้าต้องการแยก ให้ใส่ `ENCODERS=std,scalar,neon` แล้วดู `neon vs scalar` vs `scalar vs std` (ในการทดสอบบนเครื่อง dev ส่วนใหญ่ของความเร็วมาจากข้อ 1, NEON ช่วยเพิ่มอีก ~5–25% ขึ้นกับข้อมูล)
- NEON ช่วยมากเมื่อข้อความยาวและเป็น ASCII ส่วนใหญ่ — ข้อความภาษาไทยหรือมีอักขระต้อง escape เยอะ แทบไม่ช่วย
- ทุก encoder ให้ผล **เหมือน `encoding/json` ทุก byte** (ตรวจด้วย `go test`) รวมถึงการ escape `<`, `>`, `&`, U+2028/2029 และ UTF-8 ที่ผิดรูปแบบ
- ตรวจว่าใช้ NEON จริง: `go tool objdump -s scanNEON ./server | grep -E 'VTBL|VLD1'` ต้องเห็นคำสั่ง vector
