package metrics

import (
	"runtime"
	"testing"
	"time"
)

// One core fully busy for the whole interval is one core's worth of the machine,
// whatever that machine happens to have. Asserting against runtime.NumCPU() rather
// than a hardcoded 25% keeps the test honest on a host with a different core count.
func TestCPUShareOfVPSOneBusyCore(t *testing.T) {
	got := CPUShareOfVPS(0, uint64(time.Second), time.Second)
	want := 100 / float64(runtime.NumCPU())
	if diff := got - want; diff > 0.01 || diff < -0.01 {
		t.Fatalf("one busy core = %.2f%%, want %.2f%%", got, want)
	}
}

// The case that actually bites in production: a unit restarts, systemd gives it a
// fresh cgroup, and its cumulative counter starts again from zero. The new reading
// is then *below* the previous one. Subtracting unsigned integers in that order
// wraps around to something astronomical, so without the guard this reports a wild
// spike on precisely the services that just came back — the ones you are most
// likely to be watching.
func TestCPUShareOfVPSCounterReset(t *testing.T) {
	if got := CPUShareOfVPS(5*uint64(time.Second), uint64(time.Second)/10, 10*time.Second); got != 0 {
		t.Fatalf("counter reset reported %.2f%%, want 0", got)
	}
}

// No time between polls means no rate to compute, and dividing by it would be an
// infinity rendered as a bar.
func TestCPUShareOfVPSZeroElapsed(t *testing.T) {
	if got := CPUShareOfVPS(0, uint64(time.Second), 0); got != 0 {
		t.Fatalf("zero elapsed reported %.2f%%, want 0", got)
	}
}

// The first poll after startup has no previous reading. The collector skips those
// rather than treating the whole lifetime total as one interval's work, which would
// show a long-running service pinned at 100% on the first paint after a restart.
func TestCPUShareOfVPSClampsToHundred(t *testing.T) {
	huge := uint64(time.Hour) * uint64(runtime.NumCPU()) * 10
	if got := CPUShareOfVPS(0, huge, time.Second); got != 100 {
		t.Fatalf("absurd delta reported %.2f%%, want it clamped to 100", got)
	}
}
