package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Limits is the account's *real* usage against its subscription rate limits,
// read straight off the anthropic-ratelimit-unified-* response headers — the
// same numbers `/status` shows. This is true remaining quota, not the self-set
// spend budget it replaced.
type Limits struct {
	OK             bool        `json:"ok"`             // was the last probe successful
	Fetched        int64       `json:"fetched"`        // unix secs of the last successful probe
	Err            string      `json:"err,omitempty"`  // last probe error, if any
	Status         string      `json:"status"`         // unified-status: allowed | allowed_warning | rejected
	Representative string      `json:"representative"` // binding window: five_hour | seven_day
	FiveH          LimitWindow `json:"five_h"`
	SevenD         LimitWindow `json:"seven_d"`
}

// LimitWindow is one rate-limit window: how much of it is used and when it resets.
type LimitWindow struct {
	Utilization float64 `json:"utilization"` // 0..1 fraction of the window consumed
	Status      string  `json:"status"`      // allowed | allowed_warning | rejected
	Reset       int64   `json:"reset"`       // unix secs when the window resets
}

const anthropicMessages = "https://api.anthropic.com/v1/messages"

// RunLimits keeps the account's real quota fresh from two sources. Every
// usageEvery it reads the free usage address (see usage.go); every probeEvery
// it also sends the 1-token header probe, which alone knows the warning and
// binding-window labels and takes over the numbers whenever the usage read
// fails. One loop drives both so they never race on the store.
//
// The token is read from the Coder container **read-only** — we never refresh
// it, because refreshing rotates the OAuth token and would log the running
// Claude Code out; we piggyback on Claude Code's own refresh instead. When both
// sources fail the last good numbers stay up, flagged stale.
func (s *Store) RunLimits(container string, usageEvery, probeEvery time.Duration) {
	if container == "" {
		log.Println("claude limits: no container set, probe disabled")
		return
	}
	var lastProbe, usagePausedUntil time.Time
	tick := func() {
		now := time.Now()
		probed, probeOK := false, false
		if now.Sub(lastProbe) >= probeEvery {
			lastProbe, probed = now, true
			lim, err := probeLimits(container)
			s.mu.Lock()
			if err == nil {
				s.hdr, probeOK = lim, true
			}
			s.mu.Unlock()
			if err != nil {
				log.Printf("claude limits probe failed: %v", err)
			} else {
				log.Printf("claude limits: 5h %.0f%% used, 7d %.0f%% used (%s)",
					lim.FiveH.Utilization*100, lim.SevenD.Utilization*100, lim.Status)
			}
		}

		var u usageReading
		uerr := errors.New("usage read paused after being throttled")
		if now.After(usagePausedUntil) {
			u, uerr = readUsage(container)
			if errors.Is(uerr, errUsageThrottled) {
				usagePausedUntil = now.Add(probeEvery)
			}
			if uerr != nil {
				log.Printf("claude usage read failed: %v", uerr)
			}
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case uerr == nil:
			s.limits = withUsage(s.hdr, u)
		case probeOK:
			s.limits = s.hdr
		case probed:
			// Both sources failed this round: keep the numbers, flag them stale.
			s.limits.OK = false
			s.limits.Err = "quota checks failing: " + uerr.Error()
		}
		// Otherwise only the usage read failed between probes; the numbers on
		// screen are still the newest there are and say how old they are.
	}
	tick()
	for range time.Tick(usageEvery) {
		tick()
	}
}

// probeLimits sends a 1-token request purely to read the rate-limit headers off
// the response. It costs a token; the numbers it returns are worth far more.
func probeLimits(container string) (Limits, error) {
	token, err := readToken(container)
	if err != nil {
		return Limits{}, err
	}
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,` +
		`"system":"You are Claude Code, Anthropic's official CLI for Claude.",` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest("POST", anthropicMessages, bytes.NewReader(body))
	if err != nil {
		return Limits{}, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("content-type", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return Limits{}, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// A rejected request (HTTP 429) still carries the unified rate-limit headers,
	// and that is precisely the moment the numbers matter most: the window is
	// full. Treating it as a failed probe froze the panel on the last good read —
	// showing quota left while the account was actually being turned away. So
	// salvage the headers whatever the status code, and count only a header-less
	// response as a real probe failure.
	if lim, ok := parseLimits(resp.Header); ok {
		return lim, nil
	}
	if resp.StatusCode != http.StatusOK {
		return Limits{}, fmt.Errorf("probe HTTP %d (no rate-limit headers)", resp.StatusCode)
	}
	return Limits{}, errors.New("no rate-limit headers in response")
}

// parseLimits reads the unified rate-limit headers off a response. ok is false
// when they are absent — the response carries no quota information at all.
func parseLimits(h http.Header) (Limits, bool) {
	get := h.Get
	if get("anthropic-ratelimit-unified-status") == "" {
		return Limits{}, false
	}
	return Limits{
		OK:             true,
		Fetched:        time.Now().Unix(),
		Status:         get("anthropic-ratelimit-unified-status"),
		Representative: get("anthropic-ratelimit-unified-representative-claim"),
		FiveH: LimitWindow{
			Utilization: parseF(get("anthropic-ratelimit-unified-5h-utilization")),
			Status:      get("anthropic-ratelimit-unified-5h-status"),
			Reset:       parseI(get("anthropic-ratelimit-unified-5h-reset")),
		},
		SevenD: LimitWindow{
			Utilization: parseF(get("anthropic-ratelimit-unified-7d-utilization")),
			Status:      get("anthropic-ratelimit-unified-7d-status"),
			Reset:       parseI(get("anthropic-ratelimit-unified-7d-reset")),
		},
	}, true
}

// readToken pulls the current OAuth access token out of the container's
// credentials file. It refuses an expired token rather than firing a doomed
// probe — when no Claude session has run recently there is nothing to refresh it.
func readToken(container string) (string, error) {
	// Bound the docker exec: it is the only subprocess here without a deadline,
	// and it runs on the serial RunLimits loop — a hung docker daemon would wedge
	// that goroutine forever and freeze the /claude limits panel on stale numbers.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, dockerBin(), "exec", container,
		"cat", "/home/coder/.claude/.credentials.json").Output()
	if err != nil {
		return "", fmt.Errorf("read creds: %w", err)
	}
	var c struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // unix millis
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(out, &c); err != nil {
		return "", fmt.Errorf("parse creds: %w", err)
	}
	if c.OAuth.AccessToken == "" {
		return "", errors.New("no access token in creds")
	}
	if c.OAuth.ExpiresAt > 0 && time.Now().UnixMilli() > c.OAuth.ExpiresAt {
		return "", errors.New("token expired (no recent Claude session to refresh it)")
	}
	return c.OAuth.AccessToken, nil
}

func dockerBin() string {
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	for _, c := range []string{"/usr/bin/docker", "/usr/local/bin/docker", "/snap/bin/docker"} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "docker"
}

func parseF(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
func parseI(s string) int64   { n, _ := strconv.ParseInt(s, 10, 64); return n }
