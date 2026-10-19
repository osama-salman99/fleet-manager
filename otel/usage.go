package otel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// The quota figures can also be read without spending anything: Claude Code's
// own /usage screen asks this address for them, and it answers with how much of
// each window is used and when it resets. It is not a model call, so unlike the
// header probe it costs no tokens, which is what lets the page check once a
// minute instead of once every five.
//
// It is not a published interface, so everything here is written to fail soft:
// a failed read leaves the header probe's numbers standing, exactly as the page
// worked before this existed.
const anthropicUsage = "https://api.anthropic.com/api/oauth/usage"

// usageReading is one answer from the usage address. Utilization is a 0..1
// fraction, the same scale as the rate-limit headers.
type usageReading struct {
	Fetched int64
	FiveH   usageWindow
	SevenD  usageWindow
}

type usageWindow struct {
	Utilization float64
	Reset       int64 // unix secs; 0 when the reply gave no reset time
}

// errUsageThrottled means the usage address turned us away for asking too often.
var errUsageThrottled = errors.New("usage read throttled (HTTP 429)")

func readUsage(container string) (usageReading, error) {
	token, err := readToken(container)
	if err != nil {
		return usageReading{}, err
	}
	req, err := http.NewRequest("GET", anthropicUsage, nil)
	if err != nil {
		return usageReading{}, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return usageReading{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return usageReading{}, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return usageReading{}, errUsageThrottled
	}
	if resp.StatusCode != http.StatusOK {
		return usageReading{}, fmt.Errorf("usage read HTTP %d", resp.StatusCode)
	}
	return parseUsage(body)
}

// parseUsage reads the two windows the page shows out of a usage reply. The
// address reports percentages (70.0), the headers fractions (0.7); this returns
// fractions so both sources land on one scale.
func parseUsage(body []byte) (usageReading, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	var r struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return usageReading{}, fmt.Errorf("parse usage: %w", err)
	}
	conv := func(w *window) (usageWindow, bool) {
		if w == nil || w.Utilization == nil {
			return usageWindow{}, false
		}
		out := usageWindow{Utilization: *w.Utilization / 100}
		if w.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt); err == nil {
				out.Reset = t.Unix()
			}
		}
		return out, true
	}
	five, ok5 := conv(r.FiveHour)
	seven, ok7 := conv(r.SevenDay)
	if !ok5 || !ok7 {
		return usageReading{}, errors.New("usage reply has no 5-hour or weekly figure")
	}
	return usageReading{Fetched: time.Now().Unix(), FiveH: five, SevenD: seven}, nil
}

// withUsage lays a fresh usage reading over the last header probe. The numbers
// and reset times come from the reading, because it is the newer of the two.
// The labels — allowed, warning, over limit, and which window is binding — only
// the header probe knows, so they are kept, except where the newer numbers
// prove them out of date: a window at 100% is over its limit whatever the last
// probe said, and a window that has reset since the probe starts over as
// allowed rather than carrying the old window's warning or block into the new one.
func withUsage(hdr Limits, u usageReading) Limits {
	out := hdr
	out.OK, out.Err, out.Fetched = true, "", u.Fetched
	out.FiveH = mergeWindow(hdr.FiveH, u.FiveH)
	out.SevenD = mergeWindow(hdr.SevenD, u.SevenD)

	switch {
	case out.FiveH.Status == "rejected" || out.SevenD.Status == "rejected":
		out.Status = "rejected"
	case out.Status == "rejected" || out.Status == "":
		out.Status = "allowed"
	case out.FiveH.Status == "allowed" && out.SevenD.Status == "allowed":
		// Both windows came back clear, so an overall warning from the last probe
		// belonged to a window that has since reset.
		out.Status = "allowed"
	}
	if out.Representative == "" {
		out.Representative = "five_hour"
		if out.SevenD.Utilization > out.FiveH.Utilization {
			out.Representative = "seven_day"
		}
	}
	return out
}

func mergeWindow(hdr LimitWindow, u usageWindow) LimitWindow {
	out := LimitWindow{Utilization: u.Utilization, Reset: u.Reset, Status: hdr.Status}
	if out.Reset == 0 {
		out.Reset = hdr.Reset
	}
	// The two sources round the reset time differently (the usage address adds
	// a fraction of a second), so only a gap of more than a minute means the
	// window has actually rolled over.
	rolled := hdr.Reset != 0 && u.Reset != 0 && math.Abs(float64(u.Reset-hdr.Reset)) > 60
	// Use only climbs within a window, so an "over limit" from the probe holds
	// until the window resets — the usage figure is rounded to a whole percent
	// and can read 99% while requests are already being turned away.
	switch {
	case u.Utilization >= 1:
		out.Status = "rejected"
	case rolled, out.Status == "":
		out.Status = "allowed"
	}
	return out
}
