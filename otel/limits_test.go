package otel

import (
	"net/http"
	"testing"
)

// A rejected request still reports the quota. This is the case that used to
// freeze the panel on stale numbers: the account was over the 5h limit and the
// dashboard kept showing the last good read (31% left) for an hour.
func TestParseLimitsSalvagesRejected(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	h.Set("anthropic-ratelimit-unified-5h-status", "rejected")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1785890400")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.52")
	h.Set("anthropic-ratelimit-unified-7d-status", "allowed")

	lim, ok := parseLimits(h)
	if !ok {
		t.Fatal("headers present but parseLimits reported none")
	}
	if !lim.OK {
		t.Error("salvaged limits should count as a successful probe, not a stale one")
	}
	if lim.Status != "rejected" || lim.FiveH.Status != "rejected" {
		t.Errorf("status not carried through: %q / %q", lim.Status, lim.FiveH.Status)
	}
	if lim.FiveH.Utilization != 1 {
		t.Errorf("5h utilization = %v, want 1 (window full)", lim.FiveH.Utilization)
	}
	if lim.FiveH.Reset != 1785890400 {
		t.Errorf("5h reset = %d, want 1785890400", lim.FiveH.Reset)
	}
	if lim.SevenD.Utilization != 0.52 {
		t.Errorf("7d utilization = %v, want 0.52", lim.SevenD.Utilization)
	}
	if lim.Fetched == 0 {
		t.Error("Fetched not stamped")
	}
}

// A response with no quota headers at all is the only real probe failure.
func TestParseLimitsRejectsHeaderless(t *testing.T) {
	if _, ok := parseLimits(http.Header{}); ok {
		t.Error("header-less response should not be treated as quota data")
	}
}
