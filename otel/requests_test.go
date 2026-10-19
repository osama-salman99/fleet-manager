package otel

import (
	"math"
	"testing"
)

func TestRequestTimeSpansRepliesAndTools(t *testing.T) {
	now := int64(100000)
	turns := []Turn{
		{TS: 1000, Prompt: "old", Source: "repl_main_thread", DurSec: 5}, // first in the window: maybe cut short
		// Request A: two main replies with a 60 s tool run between, a subagent,
		// and a prompt suggestion afterwards that nobody waited for.
		{TS: 2010, Prompt: "a", Session: "s1", Source: "repl_main_thread", DurSec: 10, Effort: "max"},
		{TS: 2050, Prompt: "a", Session: "s1", Source: "agent:builtin:Explore", DurSec: 30},
		{TS: 2100, Prompt: "a", Session: "s1", Source: "repl_main_thread", DurSec: 20, Effort: "max"},
		{TS: 2160, Prompt: "a", Session: "s1", Source: "prompt_suggestion", DurSec: 2},
		// Request B: still going, too recent to grade.
		{TS: now - 30, Prompt: "b", Session: "s2", Source: "repl_main_thread", DurSec: 4},
		// Recorded before prompt ids were read.
		{TS: 3000, Source: "repl_main_thread", DurSec: 9},
	}
	st := requestStats(turns, map[string]string{"s1": "Health"}, now)
	if st.Count != 1 {
		t.Fatalf("count = %d, want 1 (only request a is finished and whole)", st.Count)
	}
	r := st.Recent[0]
	if r.DurSec != 100 || r.Calls != 3 || r.WritingSec != 30 || r.Effort != "max" || r.SessionName != "Health" {
		t.Fatalf("request a = %+v, want 100 s, 3 calls, 30 s writing, max, Health", r)
	}
	if math.Abs(st.WritingShare-0.3) > 1e-9 {
		t.Fatalf("writing share = %v, want 0.3", st.WritingShare)
	}
}

func TestRequestTimeAddsUpPerSession(t *testing.T) {
	now := int64(100000)
	turns := []Turn{
		{TS: 1000, Prompt: "old", Session: "s1", Source: "repl_main_thread", DurSec: 5},
		// s1: two requests, 100 s (40 s writing) and 50 s (50 s writing).
		{TS: 2040, Prompt: "a", Session: "s1", Source: "repl_main_thread", DurSec: 40},
		{TS: 2100, Prompt: "a", Session: "s1", Source: "repl_main_thread", DurSec: 0},
		{TS: 3050, Prompt: "b", Session: "s1", Source: "repl_main_thread", DurSec: 50},
		// s2: one request, 30 s.
		{TS: 4030, Prompt: "c", Session: "s2", Source: "repl_main_thread", DurSec: 30},
	}
	st := requestStats(turns, map[string]string{"s1": "Gmail"}, now)
	if len(st.BySession) != 2 {
		t.Fatalf("by session = %+v, want 2 sessions", st.BySession)
	}
	g := st.BySession[0]
	if g.SessionName != "Gmail" || g.Requests != 2 || g.TotalSec != 150 || g.WritingSec != 90 || g.LongestSec != 100 {
		t.Fatalf("first = %+v, want Gmail: 2 requests, 150 s, 90 s writing, longest 100 s", g)
	}
	if st.BySession[1].Session != "s2" || st.BySession[1].TotalSec != 30 {
		t.Fatalf("second = %+v, want s2 with 30 s", st.BySession[1])
	}
}

func TestPromptIDReachesTheTurn(t *testing.T) {
	s := newStore()
	s.ingestLogs(otlpLogs{ResourceLogs: []struct {
		ScopeLogs []struct {
			LogRecords []otlpRecord `json:"logRecords"`
		} `json:"scopeLogs"`
	}{{ScopeLogs: []struct {
		LogRecords []otlpRecord `json:"logRecords"`
	}{{LogRecords: []otlpRecord{{TimeUnixNano: "1784844719000000000", Attributes: []otlpAttr{
		{Key: "event.name", Value: otlpAnyV{StringValue: ptr("api_request")}},
		{Key: "prompt.id", Value: otlpAnyV{StringValue: ptr("p-1")}},
	}}}}}}}})
	if got := s.turns[0].Prompt; got != "p-1" {
		t.Fatalf("prompt = %q, want p-1", got)
	}
}

func ptr(s string) *string { return &s }
