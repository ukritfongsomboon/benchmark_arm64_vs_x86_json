// go-neon-simple: one HTTP server, several JSON encoders, chosen by URL path.
//
//	/std/...     encoding/json (standard library)
//	/neon/...    hand-written encoder, string scan with ARM64 NEON (arm64 only)
//	/scalar/...  same hand-written encoder, string scan with a plain byte loop
//	             (optional control: neon vs scalar = effect of SIMD alone)
//
// Every encoder produces byte-identical output (see main_test.go).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

// ---------------------------------------------------------------- data ----

type Address struct {
	City     string `json:"city"`
	Country  string `json:"country"`
	Postcode string `json:"postcode"`
}

type Record struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	Email   string   `json:"email"`
	Message string   `json:"message"`
	Bio     string   `json:"bio"`
	Score   float64  `json:"score"`
	Active  bool     `json:"active"`
	Tags    []string `json:"tags"`
	Address Address  `json:"address"`
}

type RecordList struct {
	Count   int      `json:"count"`
	Records []Record `json:"records"`
}

type EscapeReq struct {
	Text string `json:"text"`
}

type EscapeResp struct {
	Text  string `json:"text"`
	Bytes int    `json:"bytes"`
}

type User struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

type TransformReq struct {
	Users []User `json:"users"`
}

type TransformResp struct {
	Users []User `json:"users"`
	Total int    `json:"total"`
}

// makeRecords builds a deterministic dataset: mostly plain ASCII with some
// quotes, newlines, HTML characters, Thai and emoji.
func makeRecords(n int) []Record {
	names := []string{"Somchai", "Suda", "Anan", "Kanya", "Niran", "Ploy", "Chai", "Mali", "Arthit", "Rattana"}
	thai := []string{"สมชาย", "สุดา", "อนันต์", "กัญญา", "นิรันดร์"}
	cities := []string{"Bangkok", "Chiang Mai", "Khon Kaen", "Phuket", "Hat Yai"}
	tags := []string{"arm64", "neon", "simd", "json", "golang", "benchmark", "http", "<beta>", "q&a"}
	bio := []string{
		"Backend engineer focused on high-throughput HTTP services and serialization performance. ",
		"Enjoys profiling hot paths, reading disassembly, and measuring before optimizing. ",
		"Previously worked on payment systems handling millions of requests per day. ",
		"Quote of the day: \"premature optimization is the root of all evil\". ",
		"Path example: C:\\Users\\bench\\data.json\n",
		"ทำงานด้านระบบหลังบ้านและการวัดประสิทธิภาพ ",
		"Loves 🍜 and ☕ on long benchmarking nights. ",
		"Notes:\n\t- keep p99 low\n\t- avoid allocations\n",
	}
	recs := make([]Record, n)
	for i := range recs {
		name := names[i%len(names)]
		if i%7 == 3 {
			name = thai[i%len(thai)]
		}
		var b strings.Builder
		for j := 0; j < 4; j++ {
			b.WriteString(bio[(i+j*3)%len(bio)])
		}
		msg := fmt.Sprintf("Hello \"%s\"! Order #%d is ready.\nTotal: %d.%02d THB", name, 10000+i, 100+i%900, i%100)
		if i%5 == 0 {
			msg += " <b>priority</b> & fast"
		}
		t := []string{tags[i%len(tags)], tags[(i*3+1)%len(tags)]}
		if i%4 == 0 {
			t = append(t, tags[(i*7+2)%len(tags)])
		}
		recs[i] = Record{
			ID: int64(i + 1), Name: name, Message: msg, Bio: b.String(),
			Email:   fmt.Sprintf("%s.%d@example.com", strings.ToLower(names[i%len(names)]), i+1),
			Score:   float64((i*7919)%100000) / 137.0,
			Active:  i%3 != 0,
			Tags:    t,
			Address: Address{City: cities[i%len(cities)], Country: "TH", Postcode: fmt.Sprintf("%05d", 10100+i%900)},
		}
	}
	return recs
}

// ------------------------------------------------------------ encoders ----

// codec encodes a response value. index == nil means encoding/json;
// otherwise the hand-written encoder uses index to find the next byte that
// needs escaping (NEON or scalar).
type codec struct {
	name  string
	index func(string) int
}

func (c codec) encode(dst []byte, v any) ([]byte, error) {
	if c.index == nil {
		buf := bytes.NewBuffer(dst)
		err := json.NewEncoder(buf).Encode(v) // escapes HTML, appends '\n'
		return buf.Bytes(), err
	}
	var err error
	switch v := v.(type) {
	case *Record:
		dst, err = c.record(dst, v)
	case *RecordList:
		dst = append(dst, `{"count":`...)
		dst = strconv.AppendInt(dst, int64(v.Count), 10)
		dst = append(dst, `,"records":`...)
		if v.Records == nil {
			dst = append(dst, "null"...)
		} else {
			dst = append(dst, '[')
			for i := range v.Records {
				if i > 0 {
					dst = append(dst, ',')
				}
				if dst, err = c.record(dst, &v.Records[i]); err != nil {
					return dst, err
				}
			}
			dst = append(dst, ']')
		}
		dst = append(dst, '}')
	case *EscapeResp:
		dst = append(dst, `{"text":`...)
		dst = c.str(dst, v.Text)
		dst = append(dst, `,"bytes":`...)
		dst = strconv.AppendInt(dst, int64(v.Bytes), 10)
		dst = append(dst, '}')
	case *TransformResp:
		dst = append(dst, `{"users":`...)
		if v.Users == nil {
			dst = append(dst, "null"...)
		} else {
			dst = append(dst, '[')
			for i, u := range v.Users {
				if i > 0 {
					dst = append(dst, ',')
				}
				dst = append(dst, `{"id":`...)
				dst = strconv.AppendInt(dst, u.ID, 10)
				dst = append(dst, `,"name":`...)
				dst = c.str(dst, u.Name)
				dst = append(dst, `,"message":`...)
				dst = c.str(dst, u.Message)
				dst = append(dst, '}')
			}
			dst = append(dst, ']')
		}
		dst = append(dst, `,"total":`...)
		dst = strconv.AppendInt(dst, int64(v.Total), 10)
		dst = append(dst, '}')
	default:
		return dst, fmt.Errorf("unsupported type %T", v)
	}
	if err != nil {
		return dst, err
	}
	return append(dst, '\n'), nil
}

func (c codec) record(dst []byte, r *Record) ([]byte, error) {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, r.ID, 10)
	dst = append(dst, `,"name":`...)
	dst = c.str(dst, r.Name)
	dst = append(dst, `,"email":`...)
	dst = c.str(dst, r.Email)
	dst = append(dst, `,"message":`...)
	dst = c.str(dst, r.Message)
	dst = append(dst, `,"bio":`...)
	dst = c.str(dst, r.Bio)
	dst = append(dst, `,"score":`...)
	dst, err := appendFloat(dst, r.Score)
	if err != nil {
		return dst, err
	}
	dst = append(dst, `,"active":`...)
	dst = strconv.AppendBool(dst, r.Active)
	dst = append(dst, `,"tags":`...)
	if r.Tags == nil {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '[')
		for i, t := range r.Tags {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = c.str(dst, t)
		}
		dst = append(dst, ']')
	}
	dst = append(dst, `,"address":{"city":`...)
	dst = c.str(dst, r.Address.City)
	dst = append(dst, `,"country":`...)
	dst = c.str(dst, r.Address.Country)
	dst = append(dst, `,"postcode":`...)
	dst = c.str(dst, r.Address.Postcode)
	return append(dst, "}}"...), nil
}

// safe[b]: ASCII byte copied verbatim (encoding/json with HTML escaping).
var safe = func() (t [256]bool) {
	for b := 0x20; b < 0x80; b++ {
		t[b] = b != '"' && b != '\\' && b != '<' && b != '>' && b != '&'
	}
	return
}()

// indexScalar returns the index of the first byte that is not safe (or is
// >= 0x80), or len(s).
func indexScalar(s string) int {
	for i := 0; i < len(s); i++ {
		if !safe[s[i]] {
			return i
		}
	}
	return len(s)
}

const hexDigits = "0123456789abcdef"

// str appends s as a JSON string, byte-identical to encoding/json.
// Fast path: c.index skips runs of safe bytes. Slow path (scalar, shared by
// all scanners): escapes, UTF-8 validation (invalid -> \ufffd), U+2028/2029.
// The slow path returns to the fast path after 16 plain ASCII bytes, so text
// dense in non-ASCII (e.g. Thai) does not re-enter the scanner after every word.
func (c codec) str(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start, i := 0, 0
	for i < len(s) {
		i += c.index(s[i:])
		run := 0
		for i < len(s) && run < 16 {
			b := s[i]
			if b < utf8.RuneSelf {
				if safe[b] {
					i++
					run++
					continue
				}
				dst = append(dst, s[start:i]...)
				switch b {
				case '"', '\\':
					dst = append(dst, '\\', b)
				case '\b':
					dst = append(dst, '\\', 'b')
				case '\f':
					dst = append(dst, '\\', 'f')
				case '\n':
					dst = append(dst, '\\', 'n')
				case '\r':
					dst = append(dst, '\\', 'r')
				case '\t':
					dst = append(dst, '\\', 't')
				default:
					dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
				}
				i++
				start, run = i, 0
				continue
			}
			run = 0
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				dst = append(dst, s[start:i]...)
				dst = append(dst, "\\ufffd"...)
				i++
				start = i
				continue
			}
			if r == '\u2028' || r == '\u2029' {
				dst = append(dst, s[start:i]...)
				dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
				i += size
				start = i
				continue
			}
			i += size
		}
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendFloat follows encoding/json's float64 formatting.
func appendFloat(dst []byte, f float64) ([]byte, error) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return dst, fmt.Errorf("json: unsupported value: %v", f)
	}
	format := byte('f')
	if a := math.Abs(f); a != 0 && (a < 1e-6 || a >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst, nil
}

func codecs() map[string]codec {
	m := map[string]codec{"std": {name: "std"}, "scalar": {name: "scalar", index: indexScalar}}
	if hasNEON {
		m["neon"] = codec{name: "neon", index: indexNEON}
	}
	return m
}

// -------------------------------------------------------------- server ----

var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 4096); return &b }}

func write(w http.ResponseWriter, c codec, v any) {
	bp := bufPool.Get().(*[]byte)
	b, err := c.encode((*bp)[:0], v)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
	if cap(b) <= 1<<20 {
		*bp = b[:0]
		bufPool.Put(bp)
	}
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

func newMux() *http.ServeMux {
	recs := makeRecords(1000)
	docs := map[string]any{
		"small":  &recs[0],
		"medium": &RecordList{Count: 100, Records: recs[:100]},
		"large":  &RecordList{Count: 1000, Records: recs},
	}
	cs := codecs()
	pick := func(w http.ResponseWriter, r *http.Request) (codec, bool) {
		c, ok := cs[r.PathValue("enc")]
		if !ok {
			http.Error(w, "unknown encoder (std | scalar | neon; neon only on arm64)", 404)
		}
		return c, ok
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(v); err != nil {
			http.Error(w, "invalid JSON body", 400)
			return false
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pong")) })
	mux.HandleFunc("GET /cpu", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(strconv.AppendFloat(nil, cpuSeconds(), 'f', 6, 64))
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"goarch": runtime.GOARCH, "neon": hasNEON,
			"go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0)})
	})
	mux.HandleFunc("GET /{enc}/json/{doc}", func(w http.ResponseWriter, r *http.Request) {
		if c, ok := pick(w, r); ok {
			if d, ok := docs[r.PathValue("doc")]; ok {
				write(w, c, d)
			} else {
				http.NotFound(w, r)
			}
		}
	})
	mux.HandleFunc("POST /{enc}/json/escape", func(w http.ResponseWriter, r *http.Request) {
		var req EscapeReq
		if c, ok := pick(w, r); ok && decode(w, r, &req) {
			write(w, c, &EscapeResp{Text: req.Text, Bytes: len(req.Text)})
		}
	})
	mux.HandleFunc("POST /{enc}/json/transform", func(w http.ResponseWriter, r *http.Request) {
		var req TransformReq
		if c, ok := pick(w, r); ok && decode(w, r, &req) {
			out := make([]User, len(req.Users))
			for i, u := range req.Users {
				out[i] = User{ID: u.ID, Name: strings.ToUpper(u.Name), Message: u.Message}
			}
			write(w, c, &TransformResp{Users: out, Total: len(out)})
		}
	})
	return mux
}

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	names := []string{}
	for n := range codecs() {
		names = append(names, n)
	}
	log.Printf("goarch=%s neon=%v go=%s gomaxprocs=%d encoders=%v addr=%s",
		runtime.GOARCH, hasNEON, runtime.Version(), runtime.GOMAXPROCS(0), names, addr)
	log.Fatal(http.ListenAndServe(addr, newMux()))
}
