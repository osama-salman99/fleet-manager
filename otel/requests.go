package otel

import "sort"

// RequestStats is time per request: one message you send, from the start of its
// first reply to the end of its last. The tools it ran and the waiting between
// replies count too, so this is the time you actually sit through — which the
// per-reply speed figures leave out entirely.
type RequestStats struct {
	Count    int     `json:"count"`
	P50Sec   float64 `json:"p50_sec"`
	P95Sec   float64 `json:"p95_sec"`
	P50Calls float64 `json:"p50_calls"`
	P95Calls float64 `json:"p95_calls"`
	// WritingShare is the part of all that time Claude spent writing replies in
	// the main conversation (Σ reply time ÷ Σ request time); the rest went on
	// tools, subagents and the gaps between them.
	WritingShare float64   `json:"writing_share"`
	Recent       []Request `json:"recent"`
	// BySession adds each session's finished requests up, most time first, so a
	// session that keeps you waiting stands out — and shows whether that time
	// went on Claude writing or on its tools.
	BySession []SessionTime `json:"by_session"`
}

// SessionTime is one session's requests, added up.
type SessionTime struct {
	Session     string  `json:"session"`
	SessionName string  `json:"session_name,omitempty"`
	Requests    int     `json:"requests"`
	TotalSec    float64 `json:"total_sec"`
	WritingSec  float64 `json:"writing_sec"`
	LongestSec  float64 `json:"longest_sec"`
}

// bySessionRows is how many sessions the page lists.
const bySessionRows = 6

// Request is one finished request as the page lists it.
type Request struct {
	TS          int64   `json:"ts"` // when its last reply ended
	Session     string  `json:"session"`
	SessionName string  `json:"session_name,omitempty"`
	DurSec      float64 `json:"dur_sec"`
	Calls       int     `json:"calls"`
	WritingSec  float64 `json:"writing_sec"`
	Effort      string  `json:"effort"`
}

// requestIdle is how long a request must have been quiet before it counts as
// finished. Anything newer may still be running a tool, and grading it now would
// record a request as shorter than it turns out to be.
const requestIdle = 5 * 60

// Background calls that share a request's id but run after you have your answer
// (the next-prompt suggestion, the session title, summaries). Counting them would
// stretch every request by a few seconds nobody waited for.
var backgroundSource = map[string]bool{
	"prompt_suggestion":      true,
	"generate_session_title": true,
	"agent_summary":          true,
	"away_summary":           true,
}

// mainSource is the conversation itself, as opposed to a subagent or a helper.
// Subagents run while the main thread waits, so only these count as writing.
func mainSource(src string) bool { return src == "repl_main_thread" || src == "sdk" }

func requestStats(turns []Turn, names map[string]string, now int64) RequestStats {
	type acc struct {
		start, end float64
		calls      int
		writing    float64
		session    string
		effort     string
	}
	byPrompt := map[string]*acc{}
	var order []string
	for i, t := range turns {
		if t.Prompt == "" || backgroundSource[t.Source] {
			continue
		}
		// The oldest request in the window has probably lost its first replies to
		// the cap, so it would read as shorter than it was.
		if i == 0 || turns[0].Prompt == t.Prompt {
			continue
		}
		// A call's timestamp is when it finished, so it started dur_sec earlier.
		end := float64(t.TS)
		start := end - t.DurSec
		a := byPrompt[t.Prompt]
		if a == nil {
			a = &acc{start: start, end: end, session: t.Session}
			byPrompt[t.Prompt] = a
			order = append(order, t.Prompt)
		}
		a.start = min(a.start, start)
		a.end = max(a.end, end)
		a.calls++
		if mainSource(t.Source) {
			a.writing += t.DurSec
			if t.Effort != "" {
				a.effort = t.Effort
			}
		}
	}

	var st RequestStats
	var durs, calls []float64
	var sumDur, sumWriting float64
	var done []Request
	perSession := map[string]*SessionTime{}
	for _, id := range order {
		a := byPrompt[id]
		if now-int64(a.end) < requestIdle {
			continue
		}
		d := a.end - a.start
		w := min(a.writing, d)
		durs = append(durs, d)
		calls = append(calls, float64(a.calls))
		sumDur += d
		sumWriting += w
		done = append(done, Request{
			TS: int64(a.end), Session: a.session, SessionName: names[a.session],
			DurSec: d, Calls: a.calls, WritingSec: w, Effort: a.effort,
		})
		ps := perSession[a.session]
		if ps == nil {
			ps = &SessionTime{Session: a.session, SessionName: names[a.session]}
			perSession[a.session] = ps
		}
		ps.Requests++
		ps.TotalSec += d
		ps.WritingSec += w
		ps.LongestSec = max(ps.LongestSec, d)
	}
	st.Count = len(durs)
	sort.Float64s(durs)
	sort.Float64s(calls)
	st.P50Sec, st.P95Sec = pctl(durs, 50), pctl(durs, 95)
	st.P50Calls, st.P95Calls = pctl(calls, 50), pctl(calls, 95)
	if sumDur > 0 {
		st.WritingShare = sumWriting / sumDur
	}
	sort.Slice(done, func(i, j int) bool { return done[i].TS > done[j].TS })
	if len(done) > 8 {
		done = done[:8]
	}
	st.Recent = done
	for _, ps := range perSession {
		st.BySession = append(st.BySession, *ps)
	}
	sort.Slice(st.BySession, func(i, j int) bool {
		a, b := st.BySession[i], st.BySession[j]
		if a.TotalSec != b.TotalSec {
			return a.TotalSec > b.TotalSec
		}
		return a.Session < b.Session
	})
	if len(st.BySession) > bySessionRows {
		st.BySession = st.BySession[:bySessionRows]
	}
	return st
}
