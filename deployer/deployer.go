// Package deployer pulls released artifacts from GitHub and installs them on
// this VPS: download -> atomic swap -> restart -> health-check -> roll back if
// the service does not come up.
package deployer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osama-salman99/fleet-manager/ntfy"
)

const (
	pollInterval = 10 * time.Minute // safety net for missed webhooks
	healthWait   = 60 * time.Second
	latestTTL    = 5 * time.Minute // how long a fetched release tag stays good
)

// configPath is a var, not a const, only so tests can point it at a temp file.
var configPath = "/opt/vps-health/deploy-config.json"

// Kind describes how an artifact is installed.
const (
	KindJar     = "jar"     // replace a file, restart the unit
	KindBinary  = "binary"  // same as jar, but the file is an executable
	KindTarball = "tarball" // unpack into a directory (static sites)
)

// Service is one deployable unit.
type Service struct {
	Name         string `json:"name"`
	Repo         string `json:"repo"` // owner/name
	Unit         string `json:"unit"` // systemd unit; empty = nothing to restart
	Kind         string `json:"kind"`
	Target       string `json:"target"`        // file path, or directory for tarball
	AssetPattern string `json:"asset_pattern"` // regex matched against asset names
	HealthURL    string `json:"health_url"`    // optional HTTP check after restart
	PublicURL    string `json:"public_url"`    // where a human reaches it; shown on the dashboard
	// HealthWaitSec is how long a service gets to come up before the deploy calls
	// it dead and rolls back. 0 = the default. A false "unhealthy" verdict is what
	// starts the whole rollback path, so a service with a slow first boot — roofz
	// builds Hibernate indexes over a 475k-row table — needs longer than 60s.
	HealthWaitSec int  `json:"health_wait_sec"`
	Enabled       bool `json:"enabled"`
}

// Config is the whole deployer configuration, edited from the config tab.
type Config struct {
	GitHubToken     string    `json:"github_token"`   // read-only; needed for private release assets
	WebhookSecret   string    `json:"webhook_secret"` // shared secret for GitHub webhooks
	WatchdogEnabled bool      `json:"watchdog_enabled"`
	Services        []Service `json:"services"`
}

var (
	mu  sync.RWMutex
	cfg Config
)

// OnSave runs after the config changes, so whatever the config controls can be
// brought in line with it. main wires this up; the deployer itself must not
// import the packages it would otherwise need to call.
var OnSave func(Config)

// Load reads config from disk, seeding defaults on first run.
func Load() {
	mu.Lock()
	defer mu.Unlock()

	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("deployer: no config at %s, seeding defaults", configPath)
		cfg = defaultConfig()
		saveLocked()
		return
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Do NOT silently overwrite an existing config with defaults — that loses the
		// GitHub token + webhook secret and forgets every service added since. Preserve
		// the bad file for recovery
		// and page loudly; run with defaults in memory so the deployer (the recovery
		// mechanism itself) still comes up. With the atomic saveLocked below, this branch
		// should now only be reachable from non-crash corruption (disk, manual edit).
		log.Printf("deployer: config at %s is corrupt (%v) — preserving as .corrupt, running with defaults", configPath, err)
		if rerr := os.Rename(configPath, configPath+".corrupt"); rerr != nil {
			log.Printf("deployer: could not preserve corrupt config: %v", rerr)
		}
		notify("Deploy config corrupt",
			fmt.Sprintf("%s failed to parse (%v); preserved as .corrupt and running with DEFAULTS "+
				"(token/secret blank) — restore it by hand", configPath, err),
			"config", "urgent")
		cfg = defaultConfig()
		saveLocked()
		return
	}
	if backfillLocked() {
		log.Print("deployer: filled in missing public URLs")
		saveLocked()
	}
	log.Printf("deployer: loaded %d services", len(cfg.Services))
}

// Get returns a copy of the current config.
func Get() Config {
	mu.RLock()
	defer mu.RUnlock()
	return cfg
}

// Put replaces the config and persists it. If it cannot be written to disk the
// previous config stays in effect, so what is running always matches what is saved.
func Put(c Config) error {
	mu.Lock()
	prev := cfg
	cfg = c
	if err := saveLocked(); err != nil {
		cfg = prev
		mu.Unlock()
		return err
	}
	mu.Unlock()

	// Outside the lock: the hook shells out to systemctl, and holding the config
	// lock while doing that would stall every page that reads it.
	if OnSave != nil {
		OnSave(c)
	}
	return nil
}

func saveLocked() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	// Atomic write. A plain WriteFile truncates the target first, so a crash mid-write
	// (this VPS OOM-kills processes under memory pressure) leaves a truncated config —
	// and Load() then can't parse it. Write a temp file in the same dir, fsync, then
	// rename over the target (atomic on one filesystem), so the config is only ever
	// replaced whole. Same pattern as otel/ingest.go. 0600: it holds the token + secret.
	tmp := configPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, configPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// selfAssetPattern picks the release asset built for the architecture we are
// actually running on. vps-health deploys itself, so getting this wrong installs
// a binary the VPS cannot execute and takes the deployer down with it.
func selfAssetPattern() string {
	return `^vps-health-linux-` + runtime.GOARCH + `$`
}

func defaultConfig() Config {
	return Config{
		WatchdogEnabled: true,
		Services: []Service{
			{Name: "transport-service", Repo: "osama-salman99/transport-service", Unit: "transport-service.service",
				Kind: KindJar, Target: "/opt/transport-service/app.jar", AssetPattern: `transport-service-.*\.jar$`,
				HealthURL: "http://localhost:8140/", PublicURL: "https://transport.osmosis.work", Enabled: true},
			{Name: "vps-health", Repo: "osama-salman99/vps-health", Unit: "vps-health.service",
				Kind: KindBinary, Target: "/opt/vps-health/vps-health", AssetPattern: selfAssetPattern(),
				HealthURL: "http://127.0.0.1:8200/version", PublicURL: "https://health.osmosis.work", Enabled: true},
			{Name: "osmosis-index", Repo: "osama-salman99/osmosis-index", Unit: "",
				Kind: KindTarball, Target: "/var/www/osmosis", AssetPattern: `\.tar\.gz$`,
				HealthURL: "https://osmosis.work/version.json", PublicURL: "https://osmosis.work", Enabled: true},
		},
	}
}

// backfill fills in fields added after a config was written. Without it, adding
// a field would mean every existing install silently runs with it blank until
// someone types the value in by hand.
func backfillLocked() bool {
	defaults := defaultConfig().Services
	changed := false
	for i, s := range cfg.Services {
		for _, d := range defaults {
			if d.Name != s.Name {
				continue
			}
			if s.PublicURL == "" && d.PublicURL != "" {
				cfg.Services[i].PublicURL = d.PublicURL
				changed = true
			}
			if s.HealthWaitSec == 0 && d.HealthWaitSec != 0 {
				cfg.Services[i].HealthWaitSec = d.HealthWaitSec
				changed = true
			}
			break
		}
	}
	return changed
}

func findByRepo(repo string) (Service, bool) {
	mu.RLock()
	defer mu.RUnlock()
	for _, s := range cfg.Services {
		if strings.EqualFold(s.Repo, repo) {
			return s, true
		}
	}
	return Service{}, false
}

// stateDir is where the deployer keeps its markers for a service — next to the
// artifact it manages.
func stateDir(s Service) string {
	if s.Kind == KindTarball {
		return s.Target
	}
	return filepath.Dir(s.Target)
}

// deployedTagPath records what is currently installed, so the poller can tell
// whether a release is already live.
func deployedTagPath(s Service) string {
	return filepath.Join(stateDir(s), ".deployed-tag")
}

// deploySecPath records how long the last successful deploy took, from starting
// the install to live. The Overview sizes the deploy part of its progress bars
// from it, and keeping it on disk means a restart does not forget it.
func deploySecPath(s Service) string {
	return filepath.Join(stateDir(s), ".deploy-sec")
}

func lastDeploySec(s Service) float64 {
	b, err := os.ReadFile(deploySecPath(s))
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return f
}

func deployedTag(s Service) string {
	b, err := os.ReadFile(deployedTagPath(s))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// failedTagPath records a release that was tried and did not come up.
//
// Only success used to be written down, so the poller compared the newest tag
// against the last *good* one and re-deployed a broken release every 10 minutes,
// forever — each attempt another artifact swap, another chance to destroy the
// jar. Remembering the failure is what turns that loop into a single failure.
func failedTagPath(s Service) string {
	return filepath.Join(stateDir(s), ".failed-tag")
}

// pollRetryBackoff is how long the poller waits before each successive retry of a
// release that already used up its in-deploy retries. A transient cause — the VPS
// briefly out of memory, a dependency down for a few minutes — often clears on a
// timescale longer than the ~2 min of in-deploy retries but shorter than a human
// noticing, so the release gets a few more widely-spaced tries. Only once these are
// spent too is it left for the manual Retry button, so a genuinely broken build
// still cannot loop forever.
var pollRetryBackoff = []time.Duration{
	15 * time.Minute,
	1 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
}

// failure is the on-disk record of a release that did not come up: the tag, how
// many full deploy cycles have failed for it (each cycle is 1 + len(retryDelays)
// tries), and the earliest time the poller may try again.
type failure struct {
	Tag       string
	Attempts  int
	NextRetry time.Time // zero once the auto-retries are spent (permanent)
}

// permanent reports whether every auto-retry has been used, so the poller should
// leave this release alone until a human forces it.
func (f failure) permanent() bool { return f.Attempts > len(pollRetryBackoff) }

func readFailure(s Service) (failure, bool) {
	b, err := os.ReadFile(failedTagPath(s))
	if err != nil {
		return failure{}, false
	}
	fields := strings.Fields(strings.TrimSpace(string(b)))
	if len(fields) == 0 {
		return failure{}, false
	}
	rec := failure{Tag: fields[0]}
	if len(fields) >= 2 {
		rec.Attempts, _ = strconv.Atoi(fields[1])
	}
	if len(fields) >= 3 {
		if sec, err := strconv.ParseInt(fields[2], 10, 64); err == nil && sec > 0 {
			rec.NextRetry = time.Unix(sec, 0)
		}
	}
	return rec, true
}

func writeFailure(s Service, rec failure) {
	var next int64
	if !rec.NextRetry.IsZero() {
		next = rec.NextRetry.Unix()
	}
	_ = os.WriteFile(failedTagPath(s), []byte(fmt.Sprintf("%s %d %d", rec.Tag, rec.Attempts, next)), 0o644)
}

// failedTag is the tag currently recorded as failed, if any.
func failedTag(s Service) string {
	rec, ok := readFailure(s)
	if !ok {
		return ""
	}
	return rec.Tag
}

// recordFailure notes a first fresh failure of tag and schedules the first
// auto-retry. markFailed drives the real escalation; this is the simple entry
// point (and what the tests use).
func recordFailure(s Service, tag string) {
	writeFailure(s, failure{Tag: tag, Attempts: 1, NextRetry: time.Now().Add(pollRetryBackoff[0])})
}

// clearFailure forgets the failed tag. A successful deploy clears it, and so does
// the redeploy button — clicking it is a human explicitly saying "try again".
func clearFailure(s Service) {
	_ = os.Remove(failedTagPath(s))
}

// healthWaitFor is how long this service gets to come up before the deploy gives
// up on it.
func healthWaitFor(s Service) time.Duration {
	if s.HealthWaitSec > 0 {
		return time.Duration(s.HealthWaitSec) * time.Second
	}
	return healthWait
}

// deployedAt is when the current version was installed (the marker's mtime).
func deployedAt(s Service) string {
	fi, err := os.Stat(deployedTagPath(s))
	if err != nil {
		return ""
	}
	return fi.ModTime().UTC().Format(time.RFC3339)
}

// unitState reports the systemd state, or "n/a" for things with no unit (a
// static site has nothing to run).
func unitState(s Service) string {
	if s.Unit == "" {
		return "n/a"
	}
	st := isActive(s.Unit)
	if st == "" {
		return "unknown"
	}
	return st
}

// isActive returns systemd's active-state for one unit, bounded by a deadline so a
// wedged systemd/dbus can't hang the caller (a dashboard render, or verify() inside
// the fleet-serialized deploy path). "" means the query failed or timed out.
func isActive(unit string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out))
}

// unitStates reads the active-state of many units in ONE `systemctl is-active`
// call. A page render must never fork one child per service: the config page
// polls /api/status every 15s, and with ~15 configured units that is ~15
// systemctl processes spawned at once inside vps-health's ~128 MB cgroup. The
// metrics package already learnt (services.go) that the kernel then OOM-kills
// some of those children, whose empty output reads back as "unknown" and looks
// exactly like an outage — periodic, self-inflicted, on the highest-blast-radius
// service. `is-active u1 u2 …` prints one state per line, in order, on stdout
// regardless of exit code; a missing/empty line maps to "unknown", matching the
// single-unit fallback. Static sites (empty Unit) are not queried — the caller
// maps those to "n/a".
func unitStates(units []string) map[string]string {
	m := make(map[string]string, len(units))
	if len(units) == 0 {
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", append([]string{"is-active"}, units...)...).Output()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, u := range units {
		st := ""
		if i < len(lines) {
			st = strings.TrimSpace(lines[i])
		}
		if st == "" {
			st = "unknown"
		}
		m[u] = st
	}
	return m
}

// stateFromMap resolves a service's unit state from a map built by unitStates,
// mirroring unitState's "n/a" for unit-less services and "unknown" for a unit we
// somehow have no reading for.
func stateFromMap(s Service, states map[string]string) string {
	if s.Unit == "" {
		return "n/a"
	}
	if st, ok := states[s.Unit]; ok {
		return st
	}
	return "unknown"
}

// retryDelays are the waits before each retry after the first attempt fails:
// immediately, then tripling — 0, 3s, 9s, 27s, 81s. A deploy therefore gets one
// initial try plus len(retryDelays) retries before the release is written off.
//
// The failure this is here for is transient: the memory-tight VPS occasionally
// can't start a second JVM while another is still warming up, or GitHub hiccups
// mid-download. A short backoff clears that without a human, and only a run that
// fails every one of these attempts is worth paging about — or pressing the
// Retry button for.
var retryDelays = []time.Duration{
	0,
	3 * time.Second,
	9 * time.Second,
	27 * time.Second,
	81 * time.Second,
}

// Deploy installs the given release tag of a service, retrying with a backoff if
// an attempt does not come up. It is the single entry point used by the webhook,
// the poller and the redeploy button.
func Deploy(s Service, tag string) error {
	// One deploy per service at a time. Two overlapping runs are what turned a
	// bad release into an outage: the second backed the *broken* jar up over the
	// first one's backup, so when both rolled back there was nothing good left.
	// Taken first, so a second request for the same service is refused at once
	// instead of queueing behind the first.
	if !lockService(s.Name) {
		return fmt.Errorf("%s: a deploy is already running", s.Name)
	}
	defer unlockService(s.Name)

	// Then the fleet-wide gate: only one deploy runs at a time across all services,
	// so a burst of webhooks (e.g. a crawler-core bump releasing several crawlers at
	// once) can't start several JVMs together and OOM the VPS. A deploy that finds
	// it taken waits its turn (bounded, see fleetWait) rather than being dropped.
	if !waitFleetDeploy(fleetWait) {
		return fmt.Errorf("%s: another service is deploying", s.Name)
	}
	defer unlockFleetDeploy()

	// An empty tag means "whatever is newest" (a manual redeploy). Resolve it up
	// front so the version we record is the version we actually installed.
	if tag == "" {
		rel := refreshLatest(s.Repo)
		if rel.Tag == "" {
			return fmt.Errorf("no release found for %s: %s", s.Repo, rel.Err)
		}
		tag = rel.Tag
	}
	log.Printf("deployer: %s -> %s", s.Name, tag)
	start := time.Now()

	// Published for the whole deploy, retries and rollbacks included: the restarts
	// they do look just as much like an outage as the one on the way in. The marker
	// is re-asserted before every attempt (below) so a long retry sequence — a slow
	// health check, times several tries — cannot outlive its TTL and let the
	// dashboard read a deploy-in-progress as a service that is down.
	defer endDeploy(s.Name)

	var err error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			delay := retryDelays[attempt-1]
			if delay > 0 {
				log.Printf("deployer: %s %s failed (%v) — retry %d/%d in %s", s.Name, tag, err, attempt, len(retryDelays), delay)
				time.Sleep(delay)
			} else {
				log.Printf("deployer: %s %s failed (%v) — retry %d/%d now", s.Name, tag, err, attempt, len(retryDelays))
			}
		}
		beginDeploy(s.Name, tag)

		if err = deployOnce(s, tag); err == nil {
			_ = os.WriteFile(deployedTagPath(s), []byte(tag), 0o644)
			_ = os.WriteFile(deploySecPath(s), []byte(strconv.FormatFloat(time.Since(start).Seconds(), 'f', 1, 64)), 0o644)
			clearFailure(s)
			markDeployed(s.Repo, tag)
			pruneBackups(s)
			// Successful deploys are routine — only failures are worth interrupting for.
			if attempt > 0 {
				log.Printf("deployer: %s %s deployed OK on retry %d", s.Name, tag, attempt)
			} else {
				log.Printf("deployer: %s %s deployed OK", s.Name, tag)
			}
			// Self-deploy: restart() staged the replacement detached (~2s) and
			// returned, so returning here would fire the deferred unlockFleetDeploy
			// and let pollOnce move straight on to the next out-of-date service —
			// starting its download+install into a process about to be SIGKILLed by
			// the self-restart. An interrupt between that service's Rename and its
			// restart is the exact backup-clobber outage artifact.go exists to
			// prevent. So hold the locks until systemd has replaced us; the process
			// dies mid-sleep in the normal case, and if the restart somehow never
			// lands we resume after a bounded wait rather than wedging forever.
			if isSelf(s) {
				log.Printf("deployer: holding deploy locks until the self-restart replaces this process")
				time.Sleep(selfRestartGrace)
			}
			return nil
		}
	}

	// Every attempt is spent. Remember the tag so the poller stops retrying it, and
	// page a human — once, now, not once per attempt.
	markFailed(s, tag, err)
	return err
}

// deployOnce is one install attempt: stage the artifact, restart the unit and
// verify it. On failure it rolls the service back to the version that was running
// before, so the backoff and the next attempt see a healthy previous version
// rather than a down unit — but it neither records the failure nor pages anyone,
// because a later attempt may still succeed. Deploy owns that final verdict.
//
// It is a var so a test can drive Deploy's retry loop without a network or a real
// systemd unit behind it.
var deployOnce = func(s Service, tag string) error {
	// If an earlier run (or attempt) left the target missing, the unit is
	// restart-looping right now. Put the last good artifact back before anything else.
	selfHeal(s)

	// Stream onto the target's own (disk-backed) filesystem, not tmpfs /tmp: a ~60MB jar
	// sitting in RAM through install+restart adds memory pressure exactly when the new JVM
	// is warming up, and it makes the install a same-filesystem rename. The static-site
	// tarball is the exception — its stateDir is the Caddy-served web root, so keep that one
	// in the system temp dir rather than dropping a deploy-* file where it could be fetched.
	dlDir := ""
	if s.Kind != KindTarball {
		dlDir = stateDir(s)
	}
	asset, rel, tmp, err := fetchArtifact(s, tag, dlDir)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	// Integrity gate. Compare the download against the SHA-256 the release
	// published as an "<asset>.sha256" companion (see checksum.go). Fail-open
	// during rollout: a mismatch is the one signal that a root artifact was
	// tampered with, so it is alerted loudly — but until every repo emits a
	// checksum this must not block a deploy, or a legitimate release from a
	// not-yet-migrated repo would fail to install. Flip checksumEnforced once all
	// repos emit and a mismatch becomes a rollback like any other failed verify.
	switch st, want, got, cerr := verifyChecksum(rel, asset, tmp); {
	case st == checksumMismatch:
		log.Printf("deployer: %s %s CHECKSUM MISMATCH expected=%s got=%s", s.Name, tag, want, got)
		notify("Artifact checksum mismatch",
			fmt.Sprintf("%s %s: expected %s but downloaded artifact hashes to %s", s.Name, tag, want, got),
			tag, "urgent")
		if checksumEnforced {
			return fmt.Errorf("checksum mismatch: expected %s got %s", want, got)
		}
	case cerr != nil:
		// A checksum was published but could not be evaluated — a GitHub blip
		// fetching the tiny companion, not tampering. Note it; don't block.
		log.Printf("deployer: %s %s checksum unverified: %v", s.Name, tag, cerr)
	case st == checksumOK:
		log.Printf("deployer: %s %s checksum OK", s.Name, tag)
	}

	backup, err := install(s, tmp, tag)
	if err != nil {
		// install leaves the target untouched on failure, so the old version is
		// still running and there is nothing to roll back — just clean up.
		if backup != "" {
			os.RemoveAll(backup)
		}
		return fmt.Errorf("install: %w", err)
	}

	if isSelf(s) {
		startSelfCheck(s, tag, backup) // we cannot watch our own restart; see selfcheck.go
	}
	if err := restart(s); err != nil {
		rollbackAttempt(s, tag, backup, "restart failed")
		return fmt.Errorf("restart: %w", err)
	}

	if err := verify(s, tag); err != nil {
		rollbackAttempt(s, tag, backup, "unhealthy")
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

// rollbackAttempt restores the version a failed attempt replaced. A successful
// rollback is silent: whether the deploy as a whole has failed is not decided
// until the retries run out, and Deploy pages then. A *failed* rollback is never
// silent — it can leave the service with no working artifact right now, which is
// the one thing here that always needs a human immediately, retries or not.
func rollbackAttempt(s Service, tag, backup, why string) {
	if err := rollback(s, backup); err != nil {
		// The safety net tore. This is the alert that has to reach a human.
		log.Printf("deployer: ROLLBACK FAILED for %s: %v", s.Name, err)
		notify("ROLLBACK FAILED",
			fmt.Sprintf("%s %s: %s, and the rollback failed (%v). Check the service now.", s.Name, tag, why, err),
			"rotating_light", "max")
		return
	}
	log.Printf("deployer: %s %s %s — rolled back", s.Name, tag, why)
}

// markFailed records that a full deploy cycle (1 + len(retryDelays) tries) of tag
// has failed, and decides what happens next: schedule another auto-retry on the
// widening backoff, or — once those are spent — leave it for a human. It pages on
// the first failure and again on the final give-up, but stays quiet for the
// retries in between so a slow-to-recover service does not spam.
//
// It runs after the last attempt's rollback has restored the previous version, so
// for a static site — whose markers live *inside* the tree being swapped — the
// record lands in the tree that is actually live. Recording it earlier threw it
// away with the swapped-out tree, and the poller cheerfully retried the broken
// release ten minutes later.
func markFailed(s Service, tag string, err error) {
	attempts := 1
	if prev, ok := readFailure(s); ok && prev.Tag == tag {
		attempts = prev.Attempts + 1
	}
	cycleTries := len(retryDelays) + 1

	if attempts > len(pollRetryBackoff) {
		writeFailure(s, failure{Tag: tag, Attempts: attempts}) // zero NextRetry = permanent
		log.Printf("deployer: %s %s failed %d cycles — giving up (redeploy to force)", s.Name, tag, attempts)
		notify("Deploy failed",
			fmt.Sprintf("%s %s failed %d times over hours (last: %v). Auto-retries are spent — press Retry on the Deploy tab. The previous version is running.", s.Name, tag, attempts, err),
			"rotating_light", "max")
		return
	}

	next := time.Now().Add(pollRetryBackoff[attempts-1])
	writeFailure(s, failure{Tag: tag, Attempts: attempts, NextRetry: next})
	if attempts == 1 {
		log.Printf("deployer: %s %s failed after %d attempts — auto-retrying, next in %s", s.Name, tag, cycleTries, pollRetryBackoff[0])
		notify("Deploy failed",
			fmt.Sprintf("%s %s failed after %d attempts (%v). Auto-retrying with backoff; the previous version is running.", s.Name, tag, cycleTries, err),
			"rotating_light", "high")
		return
	}
	log.Printf("deployer: %s %s still failing (cycle %d) — next retry in %s", s.Name, tag, attempts, pollRetryBackoff[attempts-1])
}

// ---------- GitHub ----------

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"` // API URL, works for private repos with a token
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// Release lookups sit on the path that serves the config page, so a stalled
// connection to GitHub must not hang it.
var ghClient = &http.Client{Timeout: 10 * time.Second}

// downloadClient fetches release assets. It runs while holding the per-service
// deploy lock, so a stalled or black-holed transfer must eventually fail and
// release the lock rather than wedge that service's deploys until vps-health is
// restarted (they no-op with "already running" in the meantime). http.DefaultClient
// has no timeout at all. This one fails fast on a connection that opens but then
// goes silent (dial/TLS/response-header deadlines), while allowing a large jar the
// generous overall ceiling a legitimate transfer needs.
var downloadClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

// fetchRelease is a var so a test can serve a release without GitHub.
var fetchRelease = func(repo, tag string) (ghRelease, error) {
	var rel ghRelease
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	if tag == "" {
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if t := Get().GitHubToken; t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}

	resp, err := ghClient.Do(req)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return rel, fmt.Errorf("github %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return rel, json.NewDecoder(resp.Body).Decode(&rel)
}

// ---------- latest-release cache ----------

// An open config page asks for the newest tag of every service every 15s. Going
// to GitHub each time would burn the rate limit for an answer that changes a few
// times a day, so tags are cached and refetched at most once per latestTTL.
type latestRel struct {
	Tag     string // newest published tag; "" if we have never got one
	Err     string // why the last fetch failed, if it did
	Fetched time.Time
}

var (
	latestMu sync.Mutex
	latest   = map[string]latestRel{} // repo -> newest release
)

// latestFor returns the newest published tag of a repo, fetching it only when
// the cached copy has aged out.
func latestFor(repo string) latestRel {
	latestMu.Lock()
	prev, ok := latest[repo]
	latestMu.Unlock()

	if ok && time.Since(prev.Fetched) < latestTTL {
		return prev
	}
	return refreshLatest(repo)
}

// refreshLatest always asks GitHub. Use it where a stale answer would be wrong
// (deploying), rather than merely out of date (drawing the page).
func refreshLatest(repo string) latestRel {
	rel, err := fetchRelease(repo, "")

	latestMu.Lock()
	defer latestMu.Unlock()

	cur := latestRel{Tag: rel.TagName, Fetched: time.Now()}
	if err != nil {
		// A GitHub blip should not erase a tag we already know; report the error
		// alongside the last good answer instead of pretending we know nothing.
		cur.Tag = latest[repo].Tag
		cur.Err = err.Error()
	}
	latest[repo] = cur
	return cur
}

// markDeployed records a tag we just installed. It came from a real release, so
// it is at least as new as anything cached — without this, a service would look
// "outdated" for the rest of the TTL immediately after being updated.
func markDeployed(repo, tag string) {
	latestMu.Lock()
	defer latestMu.Unlock()
	if prev, ok := latest[repo]; ok && prev.Tag == tag {
		return
	}
	latest[repo] = latestRel{Tag: tag, Fetched: time.Now()}
}

// findAsset resolves the release for tag and returns the asset matching the
// service's AssetPattern, along with the full release so the caller can also find
// the artifact's checksum companion without a second GitHub round-trip.
func findAsset(s Service, tag string) (ghAsset, ghRelease, error) {
	rel, err := fetchRelease(s.Repo, tag)
	if err != nil {
		return ghAsset{}, ghRelease{}, err
	}
	re, err := regexp.Compile(s.AssetPattern)
	if err != nil {
		return ghAsset{}, rel, fmt.Errorf("bad asset_pattern %q: %w", s.AssetPattern, err)
	}
	for _, a := range rel.Assets {
		if re.MatchString(a.Name) {
			return a, rel, nil
		}
	}
	return ghAsset{}, rel, fmt.Errorf("no asset matching %q in %s", s.AssetPattern, rel.TagName)
}

// download streams a release asset to a temp file in dir (dir "" = the system temp dir).
func download(a ghAsset, dir string) (string, error) {
	req, err := http.NewRequest("GET", a.URL, nil)
	if err != nil {
		return "", err
	}
	// Asking for octet-stream makes the API return the binary, not JSON metadata.
	req.Header.Set("Accept", "application/octet-stream")
	if t := Get().GitHubToken; t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}

	resp, err := downloadClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("asset download: %s", resp.Status)
	}

	f, err := os.CreateTemp(dir, "deploy-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ---------- restart & verify ----------

// selfRestartGrace is how long Deploy holds its locks after a successful
// self-deploy, waiting for systemd to replace the process. It must comfortably
// exceed the detached restart's 2s delay plus the unit's stop time; the process
// normally dies well inside it, so the exact value only matters as a fail-safe
// ceiling if the restart never lands.
const selfRestartGrace = 30 * time.Second

func restart(s Service) error {
	if s.Unit == "" {
		return nil // static site: nothing to restart
	}

	// vps-health restarting itself would kill this process mid-request, so do it
	// detached and let systemd bring the new binary up.
	if s.Unit == "vps-health.service" {
		go func() {
			time.Sleep(2 * time.Second)
			// On success this process is replaced mid-restart, so we never return here.
			// If it does return, the restart failed (or systemd is wedged, caught by the
			// deadline) and we are still running the OLD binary — surface that, or the
			// deploy silently didn't land on the very service that is the deployer.
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "systemctl", "restart", s.Unit).CombinedOutput(); err != nil {
				log.Printf("deployer: SELF-RESTART FAILED: %v: %s", err, strings.TrimSpace(string(out)))
				notify("Self-restart failed",
					fmt.Sprintf("vps-health could not restart itself after a deploy (%v) — still running the old binary", err),
					"self-restart", "urgent")
			}
		}()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "restart", s.Unit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// healthPollEvery is how often verify looks at a restarting service. The health URL
// answers the moment the app is up, so a short gap means the deploy finishes within
// half a second of that instead of up to 3 s later. unitSettle is the least a service
// with no health URL must stay "active" before it counts as started.
var (
	healthPollEvery = 500 * time.Millisecond
	unitSettle      = 3 * time.Second
	unitActive      = isActive
)

// verify waits for the unit to be active and (optionally) for HTTP to answer.
// A static site has no unit, so it is checked by the version it serves instead.
func verify(s Service, tag string) error {
	if s.Kind == KindTarball && s.HealthURL != "" {
		return verifyStatic(s, tag)
	}
	if s.Unit == "" || isSelf(s) {
		return nil // nothing to check, or we are the thing restarting (startSelfCheck watches that)
	}

	wait := healthWaitFor(s)
	started := time.Now()
	deadline := started.Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(healthPollEvery)

		if unitActive(s.Unit) != "active" {
			continue
		}
		if s.HealthURL == "" {
			// Only systemd's word to go on, and a JVM that dies in start-up is still
			// "active" for its first seconds: give it the old 3 s before believing it.
			if time.Since(started) >= unitSettle {
				return nil
			}
			continue
		}
		if httpOK(s.HealthURL) {
			return nil
		}
	}
	return fmt.Errorf("%s did not become healthy within %s", s.Name, wait)
}

func httpOK(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// ---------- poller (safety net) ----------

// Poll catches releases whose webhook never arrived. It runs once at startup so
// a freshly booted VPS converges immediately instead of idling for an interval.
func Poll() {
	pollOnce()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		pollOnce()
	}
}

// skipLogged remembers which broken releases we have already complained about,
// so refusing to retry one does not write the same line to the journal every ten
// minutes for the rest of the release's life.
var skipLogged = map[string]string{} // service name -> tag we last logged

func pollOnce() {
	for _, s := range Get().Services {
		// Look up disabled services too: they do not deploy, but the config page
		// still shows how far behind they have drifted.
		rel := latestFor(s.Repo)
		if rel.Err != "" {
			log.Printf("deployer: cannot read releases for %s: %s", s.Name, rel.Err)
		}
		if !s.Enabled || rel.Tag == "" || rel.Tag == deployedTag(s) {
			continue
		}

		// A release that already failed is retried on a widening backoff, not every
		// tick, and eventually left for a human — so a transient failure recovers on
		// its own while a broken build cannot loop forever. A newer release, or a
		// human pressing Redeploy, resets this (the record is per-tag / cleared).
		if rec, ok := readFailure(s); ok && rec.Tag == rel.Tag {
			if rec.permanent() {
				if skipLogged[s.Name] != rel.Tag {
					log.Printf("deployer: %s %s failed %d cycles — not retrying (redeploy to force)", s.Name, rel.Tag, rec.Attempts)
					skipLogged[s.Name] = rel.Tag
				}
				continue
			}
			if time.Now().Before(rec.NextRetry) {
				continue // still waiting out the backoff
			}
			log.Printf("deployer: %s retrying failed %s (cycle %d)", s.Name, rel.Tag, rec.Attempts+1)
		} else {
			log.Printf("deployer: %s has %s, deployed %q", s.Name, rel.Tag, deployedTag(s))
		}

		if err := Deploy(s, rel.Tag); err != nil {
			log.Printf("deployer: %s failed: %v", s.Name, err)
		}
	}
}

// notify is a var so a test can watch what a failed deploy actually says —
// telling the truth here is half of this file's job. The send itself (and the
// ASCII-Title fold that keeps a non-ASCII Title from silently dropping the whole
// alert) lives in the shared ntfy package; delegating here also means a failed
// send now gets *logged* rather than silently swallowed, which matters most on
// exactly this path — a deploy-failure or rollback alert that vanishes is the
// worst one to lose.
var notify = func(title, message, tag, priority string) {
	ntfy.Send("deployer", title, message, tag, priority)
}
