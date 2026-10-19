package otel

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// A trimmed OTLP/JSON logs payload in the exact shape Claude Code exports: one
// api_request (with int fields as JSON *numbers*), one tool_result (with int
// fields as quoted *strings*, per the OTLP spec). Both must parse.
const samplePayload = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[
  {"timeUnixNano":"1784844719000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"api_request"}},
    {"key":"session.id","value":{"stringValue":"sess-1"}},
    {"key":"model","value":{"stringValue":"claude-opus-4-8"}},
    {"key":"query_source","value":{"stringValue":"repl_main_thread"}},
    {"key":"effort","value":{"stringValue":"high"}},
    {"key":"duration_ms","value":{"intValue":8200}},
    {"key":"input_tokens","value":{"intValue":1200}},
    {"key":"output_tokens","value":{"intValue":1500}},
    {"key":"cost_usd","value":{"doubleValue":0.0421}}
  ]},
  {"timeUnixNano":"1784844720000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"api_request"}},
    {"key":"session.id","value":{"stringValue":"sess-1"}},
    {"key":"model","value":{"stringValue":"claude-haiku-4-5"}},
    {"key":"query_source","value":{"stringValue":"generate_session_title"}},
    {"key":"duration_ms","value":{"intValue":"1082"}},
    {"key":"output_tokens","value":{"intValue":"11"}},
    {"key":"cost_usd","value":{"doubleValue":0.000576}}
  ]},
  {"timeUnixNano":"1784844721000000000","attributes":[
    {"key":"event.name","value":{"stringValue":"tool_result"}},
    {"key":"tool_name","value":{"stringValue":"Bash"}},
    {"key":"success","value":{"stringValue":"true"}},
    {"key":"duration_ms","value":{"intValue":"340"}}
  ]}
]}]}]}`

func ingestSample(t *testing.T) *Store {
	t.Helper()
	var p otlpLogs
	if err := json.Unmarshal([]byte(samplePayload), &p); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	s := newStore()
	s.ingestLogs(p)
	return s
}

// The recent-replies table names the session and the reasoning effort behind
// each call, and the page has to say how far back its totals reach — so the
// effort attribute must survive ingest and the snapshot must carry the oldest
// timestamp it still holds.
func TestEffortAndSinceReachThePage(t *testing.T) {
	s := ingestSample(t)
	if got := s.turns[0].Effort; got != "high" {
		t.Errorf("turn0 Effort = %q, want \"high\"", got)
	}
	// Claude Code omits the attribute when no effort is set; that must read as
	// blank rather than as the previous call's value.
	if got := s.turns[1].Effort; got != "" {
		t.Errorf("turn1 Effort = %q, want empty", got)
	}
	snap := s.Snapshot()
	if snap.SinceTS != s.turns[0].TS {
		t.Errorf("SinceTS = %d, want the oldest retained call at %d", snap.SinceTS, s.turns[0].TS)
	}
	if len(snap.Recent) == 0 || snap.Recent[len(snap.Recent)-1].Session != "sess-1" {
		t.Errorf("recent replies lost the session id: %+v", snap.Recent)
	}
}

// The recent list shows the name a session goes by, which comes from the
// session controller rather than from the telemetry itself. An id we have no
// name for must come through blank so the page can fall back to the short id.
func TestRecentCarriesTheSessionName(t *testing.T) {
	s := ingestSample(t)
	s.names = map[string]string{"sess-1": "Health"}
	for _, r := range s.Snapshot().Recent {
		if r.SessionName != "Health" {
			t.Fatalf("recent reply for %q named %q, want \"Health\"", r.Session, r.SessionName)
		}
	}
	s.names = nil
	for _, r := range s.Snapshot().Recent {
		if r.SessionName != "" {
			t.Fatalf("unknown session named %q, want blank", r.SessionName)
		}
	}
}

// The controller answers with a list of sessions; we only want id and title,
// and entries missing either are no use to the page.
func TestSessionNamesFromController(t *testing.T) {
	body := `{"ok":true,"sessions":[
	  {"title":"Health","session_id":"aaa","live":true},
	  {"title":"","session_id":"bbb"},
	  {"title":"VPS","session_id":""}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	names, err := fetchSessionNames(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(names) != 1 || names["aaa"] != "Health" {
		t.Errorf("got %v, want just aaa->Health", names)
	}
}

// The controller only lists the last thirty sessions, so a name must survive a
// later listing that no longer mentions that session — while a rename in a
// listing that does mention it still takes effect.
func TestSessionNamesSurviveDroppingOffTheListing(t *testing.T) {
	s := newStore()
	s.mergeNames(map[string]string{"old": "Health", "cur": "Sessions"})
	s.mergeNames(map[string]string{"cur": "Deploy"})
	if s.names["old"] != "Health" {
		t.Errorf("dropped session lost its name: %v", s.names)
	}
	if s.names["cur"] != "Deploy" {
		t.Errorf("rename did not take: %v", s.names)
	}
}

func TestIngestParsesBothIntEncodings(t *testing.T) {
	s := ingestSample(t)
	if len(s.turns) != 2 {
		t.Fatalf("got %d turns, want 2", len(s.turns))
	}
	// First turn: ints as JSON numbers → 8200ms = 8.2s, 1500 out tokens.
	if got := s.turns[0].DurSec; got != 8.2 {
		t.Errorf("turn0 DurSec = %v, want 8.2", got)
	}
	if got := s.turns[0].OutTokens; got != 1500 {
		t.Errorf("turn0 OutTokens = %v, want 1500", got)
	}
	// Second turn: ints as quoted strings → still parse (1082ms, 11 out).
	if got := s.turns[1].DurSec; got != 1.082 {
		t.Errorf("turn1 DurSec = %v, want 1.082 (string-encoded int)", got)
	}
	if got := s.turns[1].OutTokens; got != 11 {
		t.Errorf("turn1 OutTokens = %v, want 11", got)
	}
	if len(s.tools) != 1 || s.tools[0].Tool != "Bash" || s.tools[0].DurSec != 0.34 {
		t.Errorf("tool_result not parsed: %+v", s.tools)
	}
	// Session count is derived from the retained turn window at Snapshot() time (no persistent
	// map): both sample turns are sess-1, so exactly one distinct session.
	if snap := s.Snapshot(); snap.Sessions != 1 {
		t.Errorf("Snapshot().Sessions = %d, want 1 (both sample turns are sess-1)", snap.Sessions)
	}
}

func TestSnapshotCountsDistinctSessionsFromWindow(t *testing.T) {
	s := newStore()
	s.addTurn(Turn{Session: "a"})
	s.addTurn(Turn{Session: "b"})
	s.addTurn(Turn{Session: "a"}) // repeat -> still distinct 2
	s.addTurn(Turn{Session: ""})  // blank session id is ignored
	if snap := s.Snapshot(); snap.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2 (a, b; blank ignored)", snap.Sessions)
	}
}

func TestBucketOf(t *testing.T) {
	cases := []struct {
		out  float64
		want string
	}{
		{0, "low"}, {11, "low"}, {128, "low"},
		{129, "medium"}, {500, "medium"}, {1024, "medium"},
		{1025, "high"}, {5000, "high"},
	}
	for _, c := range cases {
		if got := bucketOf(c.out); got != c.want {
			t.Errorf("bucketOf(%v) = %q, want %q", c.out, got, c.want)
		}
	}
}

func TestConsumptionWindows(t *testing.T) {
	s := newStore()
	now := time.Now().Unix()
	s.turns = []Turn{
		{TS: now - 60, InTokens: 100, OutTokens: 50, CacheRead: 10, CostUSD: 0.01},       // in both windows
		{TS: now - 6*3600, InTokens: 200, OutTokens: 80, CacheRead: 20, CostUSD: 0.02},   // 7d only (older than 5h)
		{TS: now - 8*24*3600, InTokens: 999, OutTokens: 999, CacheRead: 999, CostUSD: 9}, // outside both
	}
	w := map[string]ConsumptionWindow{}
	for _, x := range s.Snapshot().Windows {
		w[x.Label] = x
	}
	if w["5h"].Turns != 1 || w["5h"].Tokens != 160 || w["5h"].Cost != 0.01 {
		t.Errorf("5h window = %+v, want 1 turn, 160 tokens, $0.01", w["5h"])
	}
	if w["7d"].Turns != 2 || w["7d"].Tokens != 460 {
		t.Errorf("7d window = %+v, want 2 turns, 460 tokens", w["7d"])
	}
}

func TestSnapshotBucketsTheTurns(t *testing.T) {
	s := ingestSample(t)
	snap := s.Snapshot()
	if !snap.Enabled {
		t.Fatal("snapshot should be enabled after ingesting turns")
	}
	byName := map[string]LoadBucket{}
	for _, b := range snap.Buckets {
		byName[b.Name] = b
	}
	// 1500 out tokens → high; 11 out tokens → low; nothing in medium.
	if byName["high"].Count != 1 || byName["high"].AvgSec != 8.2 {
		t.Errorf("high bucket = %+v, want count 1 avg 8.2", byName["high"])
	}
	if byName["low"].Count != 1 || byName["low"].AvgSec != 1.082 {
		t.Errorf("low bucket = %+v, want count 1 avg 1.082", byName["low"])
	}
	if byName["medium"].Count != 0 {
		t.Errorf("medium bucket = %+v, want empty", byName["medium"])
	}
}

func TestRollArchive(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tel.jsonl"

	// Under the cap: left untouched.
	small := []byte(`{"ts":1}` + "\n" + `{"ts":2}` + "\n")
	if err := os.WriteFile(path, small, 0o644); err != nil {
		t.Fatal(err)
	}
	rollArchive(path)
	if got, _ := os.ReadFile(path); string(got) != string(small) {
		t.Fatalf("small file was modified: %q", got)
	}

	// Over the cap: rolled down to ~archiveKeepBytes, every surviving line still
	// parses (no partial first line), and the newest line is preserved.
	var buf bytes.Buffer
	i := 0
	for buf.Len() <= archiveMaxBytes+(1<<20) {
		fmt.Fprintf(&buf, `{"ts":%d,"pad":"%s"}`+"\n", i, strings.Repeat("x", 200))
		i++
	}
	lastTS := i - 1
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	rollArchive(path)

	rolled, _ := os.ReadFile(path)
	if len(rolled) > archiveKeepBytes {
		t.Fatalf("rolled file %d bytes > keep cap %d", len(rolled), archiveKeepBytes)
	}
	if len(rolled) == 0 || len(rolled) >= buf.Len() {
		t.Fatalf("expected a smaller non-empty file, got %d (orig %d)", len(rolled), buf.Len())
	}
	sc := bufio.NewScanner(bytes.NewReader(rolled))
	var lines, maxTS int
	for sc.Scan() {
		var rec struct {
			TS int `json:"ts"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("surviving line does not parse (partial line left?): %q", sc.Text())
		}
		lines++
		if rec.TS > maxTS {
			maxTS = rec.TS
		}
	}
	if lines == 0 {
		t.Fatal("no surviving lines")
	}
	if maxTS != lastTS {
		t.Fatalf("newest line lost: kept max ts %d, want %d", maxTS, lastTS)
	}
}
