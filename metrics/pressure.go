package metrics

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pressure is how hard a service is stalling on memory — the kernel's PSI, the
// share of wall-time its tasks spent waiting on the resource instead of running.
//
// It is the metric that turned "the sessions feel slow" into a number. A cgroup
// pinned against its own memory limit spends its time in reclaim, not in your
// code, and it does that long before its memory bar looks full — the workspace
// was at 93% of its cap and thrashing while every host-level gauge read fine.
// Memory used tells you how close to the edge a thing is; pressure tells you
// whether it is already going over.
type Pressure struct {
	Level string  `json:"level"` // calm | light | medium | heavy
	Some  float64 `json:"some"`  // % of the last 60s ≥1 task waited on memory
	Full  float64 `json:"full"`  // % of the last 60s every task waited at once
}

// readPressure reads one PSI file out of a cgroup and grades it.
//
// This is the cheap way to measure stall, and it stays cheap at any poll rate:
// the kernel already keeps PSI as a moving average over 10/60/300s, so there is
// nothing to sample twice, difference, or remember between polls — one read of a
// two-line file and the number is right. That is the whole reason to prefer it
// to a rate-of-counter (memory.events, pgscan_direct), which would need a stored
// previous value per service and would grow noisier the faster it was sampled.
//
// nil when the file is absent — a static site has no cgroup, and some kernels do
// not expose per-controller PSI — so the caller leaves the metric off rather
// than drawing a confident "calm" over a reading it never got.
func readPressure(cgroupDir, file string) *Pressure {
	b, err := os.ReadFile(filepath.Join(cgroupDir, file))
	if err != nil {
		return nil
	}
	p := &Pressure{}
	// Two lines: "some avg10=.. avg60=.. avg300=.. total=..", then "full ..".
	// avg60 is the window to grade on: it more than covers the 10s service poll,
	// so a reading is never a slice of time the poll never looked at, and it does
	// not lurch a whole level on one 10-second blip the way avg10 would.
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "some":
			p.Some = psiAvg60(fields)
		case "full":
			p.Full = psiAvg60(fields)
		}
	}
	p.Level = gradePressure(p.Some, p.Full)
	return p
}

func psiAvg60(fields []string) float64 {
	for _, f := range fields {
		if rest, ok := strings.CutPrefix(f, "avg60="); ok {
			if v, err := strconv.ParseFloat(rest, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// gradePressure turns two PSI percentages into a word a person can act on.
//
// "some" is the everyday reading — the share of time at least one task was
// waiting on memory; a few percent is real but survivable. "full", where *every*
// runnable task stalled at once, is the one that means work actually stopped, so
// a little of it outranks a lot of "some" (and full can never exceed some, so it
// only ever escalates a grade, never invents one).
//
// The thresholds are deliberately round. PSI is noisy enough that finer numbers
// would be false precision, and the point of the grade is a glanceable
// calm→light→medium→heavy, not a percentage pretending to two digits.
func gradePressure(some, full float64) string {
	switch {
	case some < 1:
		return "calm"
	case some >= 40 || full >= 10:
		return "heavy"
	case some >= 10 || full >= 2:
		return "medium"
	default:
		return "light"
	}
}
