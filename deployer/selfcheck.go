package deployer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------- health checks for the two services without a unit to watch ----------
//
// verify() waits for a unit to come up, which leaves two services unchecked: the
// static site has no unit at all, and vps-health cannot watch its own restart.
// Both now check the one thing a deploy promises — that the new version is the
// one being served — and put the previous version back when it is not.

// versionOf turns a release tag into the version the service reports:
// "v61-72b82dc" -> "v61". Every release tag in the fleet has this shape.
func versionOf(tag string) string {
	if i := strings.IndexByte(tag, '-'); i > 0 {
		return tag[:i]
	}
	return tag
}

// staticTries and staticGap bound the static-site check. Caddy serves the new tree
// the moment it is renamed into place, so this only has to ride out a blip.
var (
	staticTries = 5
	staticGap   = time.Second
)

// verifyStatic fetches the site's version file from Caddy on this VPS (never
// through Cloudflare, whose cache could answer with the old file) and requires a
// 200 that names the version just installed.
func verifyStatic(s Service, tag string) error {
	want := versionOf(tag)
	var last error
	for i := 0; i < staticTries; i++ {
		if i > 0 {
			time.Sleep(staticGap)
		}
		got, err := servedVersion(s.HealthURL)
		if err == nil && got == want {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("serving %q, expected %q", got, want)
		}
		last = err
	}
	return fmt.Errorf("%s health check %s: %w", s.Name, s.HealthURL, last)
}

// servedVersion GETs a {"version": "..."} document and returns the version.
func servedVersion(rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := originClient(rawURL).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&doc); err != nil {
		return "", fmt.Errorf("not a version document: %w", err)
	}
	return doc.Version, nil
}

// originClient reaches a public https URL at this VPS's own Caddy instead of
// through DNS (which points at Cloudflare). The certificate Caddy presents is a
// Cloudflare origin certificate that no public root signs, so it is not
// verified — acceptable only because the connection never leaves loopback.
// Local URLs (http://127.0.0.1:...) use a plain client.
func originClient(rawURL string) *http.Client {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || isLoopback(u.Hostname()) {
		return http.DefaultClient
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
		TLSClientConfig: &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true},
	}}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------- vps-health checking its own restart ----------
//
// The process that deploys vps-health is the one being replaced, so it cannot
// wait for the new binary. Instead, just before restarting, it starts a small
// independent checker as its own systemd unit. If the new binary has not answered
// with the new version by the deadline, the checker puts the previous binary
// back, restores the deployed-tag marker, leaves a note, and restarts the unit.
// The restored binary reads that note on start-up (CheckSelfRollback) and records
// the release as failed, so the poller backs off instead of redeploying it.

const selfCheckSeconds = 90

func selfRollbackNote(s Service) string {
	return filepath.Join(stateDir(s), ".self-rollback")
}

// selfCheckScript is the checker's shell script. Values are passed as positional
// arguments, never spliced into the script text.
const selfCheckScript = `
url="$1"; want="$2"; deadline="$3"; backup="$4"; target="$5"; tagfile="$6"; prevtag="$7"; note="$8"; unit="$9"; failed="${10}"
end=$(( $(date +%s) + deadline ))
while [ "$(date +%s)" -lt "$end" ]; do
  sleep 3
  if curl -fsS --max-time 3 "$url" 2>/dev/null | grep -q "\"version\":\"$want\""; then
    echo "self-check: $want is up"
    exit 0
  fi
done
echo "self-check: $want did not answer within ${deadline}s, restoring the previous binary"
[ -s "$backup" ] || { echo "self-check: backup $backup is missing, cannot roll back"; exit 1; }
cp -f "$backup" "$target.restore" && chmod 755 "$target.restore" && mv -f "$target.restore" "$target" || exit 1
[ -n "$prevtag" ] && printf '%s' "$prevtag" > "$tagfile"
printf '%s' "$failed" > "$note"
systemctl restart "$unit"
`

// startSelfCheck launches the checker. A failure to launch is logged and the
// deploy goes ahead: the checker is a safety net, not a gate.
var startSelfCheck = func(s Service, tag, backup string) {
	if backup == "" {
		log.Printf("deployer: %s %s: no previous binary to fall back on, self-check skipped", s.Name, tag)
		return
	}
	unit := fmt.Sprintf("vps-health-selfcheck-%d", time.Now().Unix())
	args := []string{"--unit=" + unit, "--collect", "--quiet", "/bin/sh", "-c", selfCheckScript, "selfcheck",
		selfHealthURL(s), versionOf(tag), fmt.Sprint(selfCheckSeconds), backup, s.Target,
		deployedTagPath(s), deployedTag(s), selfRollbackNote(s), s.Unit, tag}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		log.Printf("deployer: %s %s: could not start the self-check: %v: %s", s.Name, tag, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("deployer: %s %s: self-check %s watching the restart", s.Name, tag, unit)
}

func selfHealthURL(s Service) string {
	if s.HealthURL != "" {
		return s.HealthURL
	}
	return "http://127.0.0.1:8200/version"
}

// CheckSelfRollback runs at start-up. If the self-check had to restore this
// binary, it records the release that failed so the poller backs off from it,
// and pages.
func CheckSelfRollback() {
	for _, s := range Get().Services {
		if !isSelf(s) {
			continue
		}
		b, err := os.ReadFile(selfRollbackNote(s))
		if err != nil {
			return
		}
		os.Remove(selfRollbackNote(s))
		tag := strings.TrimSpace(string(b))
		if tag == "" {
			return
		}
		log.Printf("deployer: %s %s did not come up — the self-check restored the previous version", s.Name, tag)
		markFailed(s, tag, errors.New("the new version did not answer within 90 s, so the previous one was restored"))
		return
	}
}
