package main

import (
	"bytes"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// Every encoder must produce exactly the same bytes as encoding/json.

func stdJSON(v any) string {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(v)
	return b.String()
}

func TestNEONCompiledOnARM64(t *testing.T) {
	if runtime.GOARCH == "arm64" && !hasNEON {
		t.Fatal("arm64 build without NEON")
	}
	t.Logf("GOARCH=%s hasNEON=%v encoders=%d", runtime.GOARCH, hasNEON, len(codecs()))
}

func checkStrings(t *testing.T, ss ...string) {
	t.Helper()
	for _, s := range ss {
		want := stdJSON(&EscapeResp{Text: s, Bytes: len(s)})
		for name, c := range codecs() {
			got, err := c.encode(nil, &EscapeResp{Text: s, Bytes: len(s)})
			if err != nil || string(got) != want {
				t.Fatalf("%s: input %q\n got %q\nwant %q", name, s, got, want)
			}
		}
	}
}

func TestStrings(t *testing.T) {
	checkStrings(t, "", "a", "hello world", `say "hi"`, `C:\path\`, "a\nb\r\tc", "\x00\x01\x1f\x7f",
		"<script>&</script>", "\u2028\u2029", "สวัสดีครับ ภาษาไทย", "😀🚀👩‍💻", "naïve 日本語",
		"a\xffb\xc3(\xe2\x82", "\xed\xa0\x80", strings.Repeat("plain ascii text ", 500),
		strings.Repeat("\"\\\n\t\x00<>&", 50))
}

// Escape-worthy byte at every position for lengths around the 16/64-byte
// vector boundaries (exercises the NEON loop, the overlapping tail load and
// the scalar short-string path).
func TestBoundaries(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 128, 129} {
		checkStrings(t, strings.Repeat("a", n))
		for p := 0; p < n; p++ {
			for _, sp := range []string{`"`, `\`, "\n", "\x00", "<", "&", "é", "\u2028", "\xff"} {
				checkStrings(t, strings.Repeat("a", p)+sp+strings.Repeat("b", max(0, n-p-1)))
			}
		}
	}
}

func TestIndexAllBytes(t *testing.T) {
	if !hasNEON {
		t.Skip("no NEON")
	}
	buf := make([]byte, 140)
	for n := 0; n <= len(buf); n++ {
		for p := 0; p < n; p++ {
			for v := 0; v < 256; v++ {
				for i := range buf[:n] {
					buf[i] = 'k'
				}
				buf[p] = byte(v)
				s := string(buf[:n])
				if a, b := indexNEON(s), indexScalar(s); a != b {
					t.Fatalf("len=%d pos=%d byte=%#x: neon=%d scalar=%d", n, p, v, a, b)
				}
			}
		}
	}
}

func TestRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	parts := []string{"a", " ", `"`, `\`, "\n", "\x00", "<", "&", "é", "ก", "😀", "\u2028", "\xff", "\xe2\x80"}
	for i := 0; i < 5000; i++ {
		var b strings.Builder
		for n := r.IntN(300); b.Len() < n; {
			if r.IntN(3) == 0 {
				b.WriteString(parts[r.IntN(len(parts))])
			} else {
				b.WriteByte(byte('a' + r.IntN(26)))
			}
		}
		checkStrings(t, b.String())
	}
}

// All HTTP endpoints: every encoder returns the same body as /std/.
func TestEndpointsIdentical(t *testing.T) {
	mux := newMux()
	reqs := []struct{ method, path, body string }{
		{"GET", "/json/small", ""},
		{"GET", "/json/medium", ""},
		{"GET", "/json/large", ""},
		{"POST", "/json/escape", `{"text":"Hello \"ARM64\"\nBenchmark <&> สวัสดี \u2028"}`},
		{"POST", "/json/transform", `{"users":[{"id":1,"name":"Somchai","message":"Hello \"World\""},{"id":2,"name":"Suda","message":"ARM64\nNEON"}]}`},
		{"POST", "/json/transform", `{}`},
	}
	for _, rq := range reqs {
		do := func(enc string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(rq.method, "/"+enc+rq.path, strings.NewReader(rq.body)))
			return w
		}
		want := do("std")
		if want.Code != 200 {
			t.Fatalf("std %s: %d", rq.path, want.Code)
		}
		for name := range codecs() {
			got := do(name)
			if got.Code != 200 || got.Body.String() != want.Body.String() {
				t.Fatalf("%s %s: status %d, body differs from std", name, rq.path, got.Code)
			}
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/std/json/transform", strings.NewReader(
		`{"users":[{"id":1,"name":"Somchai","message":"Hello \"World\""}]}`)))
	if want := `{"users":[{"id":1,"name":"SOMCHAI","message":"Hello \"World\""}],"total":1}` + "\n"; w.Body.String() != want {
		t.Fatalf("transform: %s", w.Body)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/avx/json/small", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown encoder: %d", w.Code)
	}
}

func BenchmarkLarge(b *testing.B) {
	recs := makeRecords(1000)
	doc := &RecordList{Count: 1000, Records: recs}
	for name, c := range codecs() {
		b.Run(name, func(b *testing.B) {
			buf, _ := c.encode(nil, doc)
			b.SetBytes(int64(len(buf)))
			b.ReportAllocs()
			for b.Loop() {
				buf, _ = c.encode(buf[:0], doc)
			}
		})
	}
}
