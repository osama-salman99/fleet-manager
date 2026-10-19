package otel

import (
	"sort"
	"time"
)

// Snapshot is the whole /claude page in one JSON object: the numbers behind
// "how long does a session's work take", cut a few ways.
type Snapshot struct {
	Enabled   bool                `json:"enabled"` // false until the first turn arrives
	Turns     int                 `json:"turns"`
	Sessions  int                 `json:"sessions"`
	TotalCost float64             `json:"total_cost"`
	TotalOut  float64             `json:"total_out_tokens"`
	Buckets   []LoadBucket        `json:"buckets"`
	Requests  RequestStats        `json:"requests"`
	ByModel   []ModelStat         `json:"by_model"`
	Tools     []ToolStat          `json:"tools"`
	Recent    []RecentReply       `json:"recent"`
	Windows   []ConsumptionWindow `json:"windows"`
	Limits    Limits              `json:"limits"` // real remaining quota from the rate-limit headers
	Updated   int64               `json:"updated"`
	// SinceTS is when the oldest call we still hold happened. Turns, Sessions,
	// TotalCost and TotalOut are all sums over that retained window rather than
	// over all time, so the page has to be able to say how far back it reaches.
	SinceTS int64 `json:"since_ts"`
}

// RecentReply is one call as the page lists it: the stored record plus the name
// its session goes by, which lives in the session controller rather than in the
// telemetry. Blank when we have no name for it — the page falls back to a short
// piece of the id.
type RecentReply struct {
	Turn
	SessionName string `json:"session_name,omitempty"`
}

// ConsumptionWindow sums the usage *our telemetry observed* over a trailing
// window (rolling last-5h and last-7d): tokens and cost the fleet ran through.
// It sits alongside Limits — the real remaining quota — as the "what did we
// spend it on" context; the authoritative "how much is left" comes from Limits.
type ConsumptionWindow struct {
	Label     string  `json:"label"` // "5h" | "7d"
	Turns     int     `json:"turns"`
	InTokens  float64 `json:"in_tokens"`
	OutTokens float64 `json:"out_tokens"`
	CacheRead float64 `json:"cache_read"`
	Tokens    float64 `json:"tokens"` // in + out + cache_read
	Cost      float64 `json:"cost"`
}

// LoadBucket is the metric this whole thread was after: an average session's
// performance in seconds under low / medium / high load — where load is how much
// work the task demanded, measured by the tokens it generated.
type LoadBucket struct {
	Name   string  `json:"name"`  // low | medium | high
	Range  string  `json:"range"` // human label for the token span
	Count  int     `json:"count"`
	AvgSec float64 `json:"avg_sec"`
	P50Sec float64 `json:"p50_sec"`
	P95Sec float64 `json:"p95_sec"`
	// TokPerSec is aggregate output throughput for the bucket (Σ out ÷ Σ duration).
	// It separates "slow" from "just big": flat across buckets = healthy, a drop
	// only at high load = a real regression to chase.
	TokPerSec float64 `json:"tok_per_sec"`
}

type ModelStat struct {
	Model  string  `json:"model"`
	Count  int     `json:"count"`
	AvgSec float64 `json:"avg_sec"`
	Cost   float64 `json:"cost"`
}

type ToolStat struct {
	Tool   string  `json:"tool"`
	Count  int     `json:"count"`
	AvgSec float64 `json:"avg_sec"`
}

// bucketOf grades a turn by how much it produced. Output tokens are the truest
// cheap proxy for "how much work the task required" — a one-line answer and a
// full refactor are worlds apart here, and it is the axis that moves duration.
func bucketOf(outTokens float64) string {
	switch {
	case outTokens <= 128:
		return "low"
	case outTokens <= 1024:
		return "medium"
	default:
		return "high"
	}
}

var bucketRange = map[string]string{
	// The unit is named once, in the section heading — repeating "output tokens"
	// on all three cards was noise.
	"low":    "≤128 tok",
	"medium": "129–1024 tok",
	"high":   ">1024 tok",
}

// Quota is the account's remaining quota on its own.
func (s *Store) Quota() Limits {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits
}

// Snapshot builds the page view from the current windows.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := Snapshot{
		Enabled: len(s.turns) > 0,
		Turns:   len(s.turns),
		Updated: time.Now().Unix(),
		// Sessions is computed from the retained turn window below, not a persistent map.
	}
	if len(s.turns) > 0 {
		snap.SinceTS = s.turns[0].TS
	}

	// Group durations by load bucket, and tally totals + per-model as we go.
	byBucket := map[string][]float64{}
	bucketOut := map[string]float64{} // Σ output tokens per bucket, for throughput
	type macc struct {
		n    int
		sum  float64
		cost float64
	}
	models := map[string]*macc{}

	// Distinct sessions *within the retained turn window* -- computed here rather than kept in
	// a process-lifetime map, so the "sessions seen" count matches the 2000-turn window it's
	// shown beside (it used to keep climbing past the window) and can't grow without bound.
	seen := map[string]struct{}{}

	// Rolling consumption windows: accumulate any turn newer than each cutoff.
	now := time.Now().Unix()
	win5h := &ConsumptionWindow{Label: "5h"}
	win7d := &ConsumptionWindow{Label: "7d"}
	cut5h := now - 5*3600
	cut7d := now - 7*24*3600
	acc := func(w *ConsumptionWindow, t Turn) {
		w.Turns++
		w.InTokens += t.InTokens
		w.OutTokens += t.OutTokens
		w.CacheRead += t.CacheRead
		w.Tokens += t.InTokens + t.OutTokens + t.CacheRead
		w.Cost += t.CostUSD
	}

	for _, t := range s.turns {
		snap.TotalCost += t.CostUSD
		snap.TotalOut += t.OutTokens
		if t.Session != "" {
			seen[t.Session] = struct{}{}
		}

		if t.TS >= cut7d {
			acc(win7d, t)
			if t.TS >= cut5h {
				acc(win5h, t)
			}
		}

		b := bucketOf(t.OutTokens)
		byBucket[b] = append(byBucket[b], t.DurSec)
		bucketOut[b] += t.OutTokens

		m := models[t.Model]
		if m == nil {
			m = &macc{}
			models[t.Model] = m
		}
		m.n++
		m.sum += t.DurSec
		m.cost += t.CostUSD
	}
	snap.Sessions = len(seen)

	for _, name := range []string{"low", "medium", "high"} {
		d := byBucket[name]
		sort.Float64s(d)
		var tps float64
		if sumDur := sum(d); sumDur > 0 {
			tps = bucketOut[name] / sumDur
		}
		snap.Buckets = append(snap.Buckets, LoadBucket{
			Name:      name,
			Range:     bucketRange[name],
			Count:     len(d),
			AvgSec:    mean(d),
			P50Sec:    pctl(d, 50),
			P95Sec:    pctl(d, 95),
			TokPerSec: tps,
		})
	}

	for model, m := range models {
		snap.ByModel = append(snap.ByModel, ModelStat{
			Model: model, Count: m.n, AvgSec: m.sum / float64(m.n), Cost: m.cost,
		})
	}
	sort.Slice(snap.ByModel, func(i, j int) bool { return snap.ByModel[i].Count > snap.ByModel[j].Count })

	// Tools: average execution time, busiest first.
	type tacc struct {
		n   int
		sum float64
	}
	tools := map[string]*tacc{}
	for _, tc := range s.tools {
		t := tools[tc.Tool]
		if t == nil {
			t = &tacc{}
			tools[tc.Tool] = t
		}
		t.n++
		t.sum += tc.DurSec
	}
	for name, t := range tools {
		snap.Tools = append(snap.Tools, ToolStat{Tool: name, Count: t.n, AvgSec: t.sum / float64(t.n)})
	}
	sort.Slice(snap.Tools, func(i, j int) bool { return snap.Tools[i].Count > snap.Tools[j].Count })
	if len(snap.Tools) > 12 {
		snap.Tools = snap.Tools[:12]
	}

	snap.Requests = requestStats(s.turns, s.names, now)

	snap.Windows = []ConsumptionWindow{*win5h, *win7d}
	snap.Limits = s.limits

	// Recent replies, newest first, each carrying its session's name if we know it.
	n := len(s.turns)
	for i := n - 1; i >= 0 && i > n-25; i-- {
		snap.Recent = append(snap.Recent, RecentReply{
			Turn: s.turns[i], SessionName: s.names[s.turns[i].Session],
		})
	}
	return snap
}

func sum(d []float64) float64 {
	var s float64
	for _, v := range d {
		s += v
	}
	return s
}

func mean(d []float64) float64 {
	if len(d) == 0 {
		return 0
	}
	return sum(d) / float64(len(d))
}
