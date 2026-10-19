// Package otel ingests Claude Code's own OpenTelemetry export — the real
// per-turn duration, tokens, and cost each session emits — and keeps it in a
// form the /claude page can render and a JSONL archive can be analysed from
// later. This is what the whole "the sessions feel slow" thread was reaching
// for: not a synthetic proxy, but Claude's actual numbers.
//
// Claude Code speaks OTLP/HTTP. We take the JSON encoding (OTEL_EXPORTER_OTLP_
// PROTOCOL=http/json) so there is no protobuf dependency to carry, and decode
// gzip ourselves because the exporter compresses and Go's server does not
// undo Content-Encoding on a request body (it does de-chunk transparently).
package otel

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- OTLP/JSON wire types (only the fields we read) ----

type otlpLogs struct {
	ResourceLogs []struct {
		ScopeLogs []struct {
			LogRecords []otlpRecord `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type otlpRecord struct {
	TimeUnixNano string     `json:"timeUnixNano"`
	Attributes   []otlpAttr `json:"attributes"`
}

type otlpAttr struct {
	Key   string   `json:"key"`
	Value otlpAnyV `json:"value"`
}

// otlpAnyV is an OTLP AnyValue. int64 fields are a string in the JSON spec but
// some exporters emit a bare number, so the numeric fields accept either.
type otlpAnyV struct {
	StringValue *string  `json:"stringValue"`
	IntValue    *flexNum `json:"intValue"`
	DoubleValue *flexNum `json:"doubleValue"`
	BoolValue   *bool    `json:"boolValue"`
}

// flexNum decodes from a JSON number *or* a quoted number, so "521" and 521
// both parse — OTLP/JSON uses the former for int64, the latter for double.
type flexNum float64

func (n *flexNum) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*n = flexNum(f)
	}
	return nil // lenient: a value we cannot parse is 0, not a dropped payload
}

func (v otlpAnyV) str() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return strconv.FormatFloat(float64(*v.IntValue), 'f', -1, 64)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(float64(*v.DoubleValue), 'f', -1, 64)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	}
	return ""
}

func (v otlpAnyV) f64() float64 {
	switch {
	case v.IntValue != nil:
		return float64(*v.IntValue)
	case v.DoubleValue != nil:
		return float64(*v.DoubleValue)
	case v.StringValue != nil:
		f, _ := strconv.ParseFloat(*v.StringValue, 64)
		return f
	}
	return 0
}

// ---- what we keep ----

// Turn is one api_request event: one inference call, with the two numbers that
// matter here — how long it took and how much work it produced.
type Turn struct {
	TS        int64   `json:"ts"`
	Session   string  `json:"session"`
	Model     string  `json:"model"`
	Source    string  `json:"source"` // query_source: main thread, subagent, title-gen, …
	DurSec    float64 `json:"dur_sec"`
	InTokens  float64 `json:"in_tokens"`
	OutTokens float64 `json:"out_tokens"`
	CacheRead float64 `json:"cache_read"`
	CostUSD   float64 `json:"cost_usd"`
	Speed     string  `json:"speed"`
	// Effort is the reasoning effort the session was running at (low/medium/
	// high/…). Claude Code only sends it when one is set, so it is blank for
	// anything recorded before we started reading it, and for calls that have
	// no effort of their own.
	Effort string `json:"effort"`
	// Prompt is the id of the request this call answered — every call, tool run
	// and subagent reply that one message from you sets off shares it. Grouping
	// on it gives the time a whole request takes, not just one reply. Blank for
	// anything recorded before we read it (2026-10-04).
	Prompt string `json:"prompt,omitempty"`
}

// ToolCall is one tool_result event.
type ToolCall struct {
	TS     int64   `json:"ts"`
	Tool   string  `json:"tool"`
	DurSec float64 `json:"dur_sec"`
	OK     bool    `json:"ok"`
}

const (
	// The archive is the record for "analyse later"; the windows are what the
	// page averages over. 2000 turns is a few days of normal use in memory.
	archiveFile = "/opt/vps-health/claude-telemetry.jsonl"
	turnWindow  = 2000
	toolWindow  = 2000
)

// Store holds the rolling windows plus the append handle to the archive.
type Store struct {
	mu     sync.Mutex
	turns  []Turn
	tools  []ToolCall
	file   *os.File
	limits Limits            // real rate-limit quota, refreshed by RunLimits
	hdr    Limits            // the last header probe on its own, which RunLimits merges usage reads onto
	names  map[string]string // session id -> the name it was given, from the session controller
}

// newStore is the in-memory store with no persistence — the seam tests use so
// they never touch /opt.
func newStore() *Store {
	return &Store{}
}

func NewStore() *Store {
	s := newStore()
	s.replay()
	s.open()
	return s
}

func attrMap(rec otlpRecord) map[string]otlpAnyV {
	m := make(map[string]otlpAnyV, len(rec.Attributes))
	for _, a := range rec.Attributes {
		m[a.Key] = a.Value
	}
	return m
}

// recordTS prefers the event's own timestamp; falls back to now if absent.
func recordTS(rec otlpRecord) int64 {
	if rec.TimeUnixNano != "" {
		if n, err := strconv.ParseInt(rec.TimeUnixNano, 10, 64); err == nil && n > 0 {
			return n / 1e9
		}
	}
	return time.Now().Unix()
}

// ingestLogs walks an OTLP logs payload and files each event we care about.
func (s *Store) ingestLogs(p otlpLogs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rl := range p.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, rec := range sl.LogRecords {
				a := attrMap(rec)
				switch a["event.name"].str() {
				case "api_request":
					t := Turn{
						TS:        recordTS(rec),
						Session:   a["session.id"].str(),
						Model:     a["model"].str(),
						Source:    a["query_source"].str(),
						DurSec:    a["duration_ms"].f64() / 1000,
						InTokens:  a["input_tokens"].f64(),
						OutTokens: a["output_tokens"].f64(),
						CacheRead: a["cache_read_tokens"].f64(),
						CostUSD:   a["cost_usd"].f64(),
						Speed:     a["speed"].str(),
						Effort:    a["effort"].str(),
						Prompt:    a["prompt.id"].str(),
					}
					s.addTurn(t)
					s.archive(t)
				case "tool_result":
					s.tools = appendCapped(s.tools, ToolCall{
						TS:     recordTS(rec),
						Tool:   a["tool_name"].str(),
						DurSec: a["duration_ms"].f64() / 1000,
						OK:     a["success"].str() == "true",
					}, toolWindow)
				}
			}
		}
	}
}

func (s *Store) addTurn(t Turn) {
	s.turns = appendCapped(s.turns, t, turnWindow)
	// The distinct-session count is derived from the retained window at Snapshot() time,
	// not tracked here -- that keeps it bounded and consistent with the window.
}

func appendCapped[T any](s []T, v T, cap int) []T {
	s = append(s, v)
	if len(s) > cap {
		s = s[len(s)-cap:]
	}
	return s
}

// ---- HTTP ingest ----

// Handler is the OTLP/HTTP receiver: /v1/logs carries the events we read,
// /v1/metrics we acknowledge and ignore (the events already carry per-turn
// tokens and cost). Both must answer 200 or the exporter retries forever.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		if body := readBody(r); body != nil {
			var p otlpLogs
			if err := json.Unmarshal(body, &p); err == nil {
				s.ingestLogs(p)
			}
		}
		otlpOK(w)
	})
	mux.HandleFunc("/v1/metrics", func(w http.ResponseWriter, r *http.Request) { otlpOK(w) })
	mux.HandleFunc("/v1/traces", func(w http.ResponseWriter, r *http.Request) { otlpOK(w) })
	return mux
}

func otlpOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("{}"))
}

// readBody returns the request body, gunzipped when the exporter compressed it.
// Go de-chunks transparently; Content-Encoding it leaves to us.
func readBody(r *http.Request) []byte {
	var rd io.Reader = r.Body
	if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil
		}
		defer gz.Close()
		rd = gz
	}
	b, err := io.ReadAll(io.LimitReader(rd, 16<<20))
	if err != nil {
		return nil
	}
	return b
}

// ---- persistence ----

// archive size caps. The archive is append-only (one line per turn) but only the
// last replayTail (512 KB) is ever read back, so anything older is dead weight that
// would otherwise grow without bound. Roll it to its tail once it passes the max.
const (
	archiveMaxBytes  = 8 << 20 // roll when the file grows past 8 MB
	archiveKeepBytes = 2 << 20 // keep the last 2 MB (>> the 512 KB replay tail)
)

// rollArchive caps claude-telemetry.jsonl so it can't grow unbounded: past
// archiveMaxBytes, rewrite it keeping only its last archiveKeepBytes, trimmed to a
// whole-line boundary. Called from open() at startup — and since vps-health
// redeploys often, that enforces the cap regularly with no background job. Losing
// the older lines is safe: replay() only ever seeds from the last 512 KB, and the
// live /claude windows are already capped in memory.
func rollArchive(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= archiveMaxBytes {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(-archiveKeepBytes, io.SeekEnd); err != nil {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[i+1:] // drop the partial first line the seek landed in
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil { // atomic replace
		os.Remove(tmp)
		log.Printf("otel: could not roll %s: %v", path, err)
	}
}

func (s *Store) open() {
	rollArchive(archiveFile)
	f, err := os.OpenFile(archiveFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("otel: cannot open %s (turns will not be archived): %v", archiveFile, err)
		return
	}
	s.file = f
}

func (s *Store) archive(t Turn) {
	if s.file == nil {
		return
	}
	if b, err := json.Marshal(t); err == nil {
		s.file.Write(append(b, '\n'))
	}
}

// replay seeds the window and the session set from the tail of the archive, so a
// redeploy — vps-health ships itself often — does not blank the page.
func (s *Store) replay() {
	f, err := os.Open(archiveFile)
	if err != nil {
		return
	}
	defer f.Close()
	const tail = 512 * 1024
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		f.Seek(-tail, io.SeekEnd)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var t Turn
		if json.Unmarshal(sc.Bytes(), &t) == nil && t.TS > 0 {
			s.addTurn(t) // a mid-row from the seek fails to parse and is skipped
		}
	}
}

// ---- query / snapshot for the page ----

func pctl(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)-1))
	return sorted[i]
}
