package otel

import "testing"

// The shape the usage address actually answered with (trimmed): percentages,
// reset times with fractional seconds, and a pile of null windows we ignore.
const usageReply = `{"five_hour":{"utilization":70.0,"resets_at":"2026-09-30T20:40:00.176671+00:00",
"limit_dollars":null},"seven_day":{"utilization":37.0,"resets_at":"2026-10-03T05:00:00.176690+00:00"},
"seven_day_opus":null,"nimbus_quill":{"utilization":0.0,"resets_at":null},"extra_usage":{"is_enabled":false}}`

func TestParseUsage(t *testing.T) {
	u, err := parseUsage([]byte(usageReply))
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveH.Utilization != 0.70 || u.SevenD.Utilization != 0.37 {
		t.Errorf("utilization = %v / %v, want fractions 0.70 / 0.37", u.FiveH.Utilization, u.SevenD.Utilization)
	}
	if u.FiveH.Reset != 1790800800 || u.SevenD.Reset != 1791003600 {
		t.Errorf("reset = %d / %d", u.FiveH.Reset, u.SevenD.Reset)
	}
	if u.Fetched == 0 {
		t.Error("Fetched not stamped")
	}
}

func TestParseUsageMissingWindow(t *testing.T) {
	if _, err := parseUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":3}}`)); err == nil {
		t.Error("a reply without the 5-hour figure should not count as a reading")
	}
	if _, err := parseUsage([]byte(`<html>`)); err == nil {
		t.Error("a non-JSON reply should not count as a reading")
	}
}

func probeAt(fiveStatus string, fiveReset int64) Limits {
	return Limits{
		OK: true, Fetched: 100, Status: fiveStatus, Representative: "five_hour",
		FiveH:  LimitWindow{Utilization: 0.80, Status: fiveStatus, Reset: fiveReset},
		SevenD: LimitWindow{Utilization: 0.30, Status: "allowed", Reset: 5000},
	}
}

// Between probes the numbers come from the usage read, the labels from the probe.
func TestWithUsageKeepsProbeLabels(t *testing.T) {
	u := usageReading{Fetched: 200,
		FiveH:  usageWindow{Utilization: 0.83, Reset: 1000},
		SevenD: usageWindow{Utilization: 0.31, Reset: 5000}}
	got := withUsage(probeAt("allowed_warning", 1000), u)
	if got.FiveH.Utilization != 0.83 || got.SevenD.Utilization != 0.31 || got.Fetched != 200 {
		t.Errorf("numbers not taken from the usage read: %+v", got)
	}
	if got.FiveH.Status != "allowed_warning" || got.Status != "allowed_warning" || got.Representative != "five_hour" {
		t.Errorf("probe labels lost: %+v", got)
	}
	if !got.OK || got.Err != "" {
		t.Errorf("a good usage read should clear the stale flag: %+v", got)
	}
}

// A full window is over its limit even if the last probe, minutes ago, said warning.
func TestWithUsageFullWindowIsRejected(t *testing.T) {
	u := usageReading{Fetched: 200,
		FiveH:  usageWindow{Utilization: 1, Reset: 1000},
		SevenD: usageWindow{Utilization: 0.31, Reset: 5000}}
	got := withUsage(probeAt("allowed_warning", 1000), u)
	if got.FiveH.Status != "rejected" || got.Status != "rejected" {
		t.Errorf("full window not shown as over limit: %+v", got)
	}
}

// Once the 5-hour window resets, the old window's warning must not carry over.
// The sub-second difference between the two sources' reset times is not a reset.
func TestWithUsageWindowRolledOver(t *testing.T) {
	u := usageReading{Fetched: 200,
		FiveH:  usageWindow{Utilization: 0.02, Reset: 1000 + 5*3600},
		SevenD: usageWindow{Utilization: 0.31, Reset: 5000}}
	got := withUsage(probeAt("allowed_warning", 1000), u)
	if got.FiveH.Status != "allowed" || got.Status != "allowed" {
		t.Errorf("reset window still flagged: %+v", got)
	}

	same := usageReading{Fetched: 200,
		FiveH:  usageWindow{Utilization: 0.85, Reset: 1001},
		SevenD: usageWindow{Utilization: 0.31, Reset: 5000}}
	if got := withUsage(probeAt("allowed_warning", 1000), same); got.FiveH.Status != "allowed_warning" {
		t.Errorf("a one-second reset difference was taken for a new window: %+v", got)
	}
}

// A blocked window stays blocked until it resets, even if the rounded figure says 99%.
func TestWithUsageRejectedHoldsUntilReset(t *testing.T) {
	u := usageReading{Fetched: 200,
		FiveH:  usageWindow{Utilization: 0.99, Reset: 1000},
		SevenD: usageWindow{Utilization: 0.31, Reset: 5000}}
	if got := withUsage(probeAt("rejected", 1000), u); got.FiveH.Status != "rejected" || got.Status != "rejected" {
		t.Errorf("block dropped before the window reset: %+v", got)
	}
	u.FiveH = usageWindow{Utilization: 0.01, Reset: 1000 + 5*3600}
	if got := withUsage(probeAt("rejected", 1000), u); got.FiveH.Status != "allowed" || got.Status != "allowed" {
		t.Errorf("block carried into the new window: %+v", got)
	}
}

// Before any header probe has succeeded the usage read still fills the panel.
func TestWithUsageWithoutProbe(t *testing.T) {
	u := usageReading{Fetched: 200,
		FiveH:  usageWindow{Utilization: 0.20, Reset: 1000},
		SevenD: usageWindow{Utilization: 0.40, Reset: 5000}}
	got := withUsage(Limits{}, u)
	if !got.OK || got.Status != "allowed" || got.FiveH.Status != "allowed" || got.Representative != "seven_day" {
		t.Errorf("usage-only reading incomplete: %+v", got)
	}
}
