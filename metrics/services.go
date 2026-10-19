package metrics

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Group separates the things this VPS exists to run from the things that keep
// it running. The dashboard shows apps large and infrastructure small.
const (
	GroupApp   = "app"
	GroupInfra = "infra"
)

// ServiceTarget is what the caller wants watched. It is deliberately not tied
// to the deployer's Service type: infrastructure units are watched here without
// ever being deployable.
type ServiceTarget struct {
	Name      string
	Unit      string // empty = nothing to ask systemd about (e.g. a static site)
	HealthURL string // empty = no HTTP check
	PublicURL string // where a human can go and look at it
	Repo      string // owner/name on GitHub; empty for infra with no repo
	Group     string
	// A file whose first line is shown as this row's note, for a job that only
	// runs now and then: its unit being up says nothing about what it last found.
	NoteFile string
}

// ServiceStatus is one row of the dashboard: is it up, since when, how fat, and
// does it actually answer over HTTP.
type ServiceStatus struct {
	Name      string  `json:"name"`
	Unit      string  `json:"unit"`
	Group     string  `json:"group"`
	State     string  `json:"state"` // active / inactive / failed / activating / unknown / n/a
	Sub       string  `json:"sub"`   // running / exited / dead / …
	Active    bool    `json:"active"`
	Enabled   bool    `json:"enabled"` // systemd starts it at boot; false = switched off on purpose
	UptimeSec float64 `json:"uptime_sec"`
	MemMB     float64 `json:"mem_mb"`
	// Share of the whole VPS this is using, 0-100 across all four cores, so it
	// reads on the same scale as the CPU figure at the top of the page and the
	// services roughly sum to it. Derived from CPUNSec between two polls.
	CPUPct float64 `json:"cpu_pct"`
	// The cumulative counter CPUPct is derived from. The kernel has no "right
	// now" reading to offer, only this total since the cgroup was created, so the
	// collector keeps the previous poll and subtracts. Never rendered.
	CPUNSec uint64 `json:"-"`
	// The ceiling this thing dies at, 0 when it has none. Without it a memory
	// figure is just a number: 400 MB is comfortable for one service and the edge
	// of a throttle-and-thrash for another.
	MemMaxMB float64 `json:"mem_max_mb"`
	// Processes the kernel has killed in here for running out of memory, since it
	// started. Zero for everything, until the day it is not.
	OOMKills int `json:"oom_kills"`
	// How hard this thing is stalling on memory right now, graded calm/light/
	// medium/heavy. Memory used says how close to the edge it is; this says whether
	// it is already going over — the workspace read "slow" here while its memory
	// bar still looked fine. nil when there is no cgroup to read (a static site).
	MemPressure *Pressure `json:"mem_pressure,omitempty"`
	Restarts    int       `json:"restarts"`
	HasHealth   bool      `json:"has_health"`
	HTTPOk      bool      `json:"http_ok"`
	PublicURL   string    `json:"url"`
	Repo        string    `json:"repo,omitempty"` // owner/name on GitHub, for the source link
	Note        string    `json:"note,omitempty"` // first line of the target's NoteFile

	// What is running inside, for the things that are not one program. A unit is a
	// single service and its memory figure explains itself; the Coder workspace is
	// a machine full of processes, and "coder-workspace, 2.4 GB" — permanently the
	// largest number on the page — answers nothing on its own. Empty for units.
	Breakdown []ProcGroup `json:"breakdown,omitempty"`

	// A deploy is being installed right now. Stamped by the collector, which is
	// the only place that can see both systemd and the deployer; this package
	// probes the VPS and must not know that deploying is a thing.
	Deploying bool   `json:"deploying"`
	DeployTag string `json:"deploy_tag,omitempty"` // the version going in
}

// ProcGroup is one kind of process inside a container — all the Claude sessions,
// or the Gradle daemon — with the memory they hold between them.
type ProcGroup struct {
	Name  string  `json:"name"`
	MemMB float64 `json:"mem_mb"`
	Count int     `json:"count"` // how many processes, so "×3" can be shown
}

// Down is a definitive outage: systemd was asked, and it says the unit is not
// running, and nothing is expected to be putting it back. "unknown" (we could
// not ask), "activating" (it is on its way) and a unit in the middle of a deploy
// (someone is already restarting it) are deliberately not outages — acting on
// any of them means restarting a service that was never broken.
func (s ServiceStatus) Down() bool {
	return s.Unit != "" && !s.Deploying && (s.State == "inactive" || s.State == "failed")
}

// ReadServiceStatuses probes every target.
//
// The unit properties come from a single systemctl call rather than one per
// service: this process runs under a 128 MB cgroup, and forking a dozen
// systemctl children at once got some of them killed — which read back as
// "unknown", which looked exactly like an outage. HTTP checks stay concurrent;
// they are goroutines, not processes.
func ReadServiceStatuses(targets []ServiceTarget) []ServiceStatus {
	units := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Unit != "" {
			units = append(units, t.Unit)
		}
	}
	props := showUnits(units)
	bootUptime := uptimeSeconds()

	out := make([]ServiceStatus, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		out[i] = describe(t, props[t.Unit], bootUptime)
		if t.HealthURL == "" {
			continue
		}
		wg.Add(1)
		go func(i int, url string) {
			defer wg.Done()
			out[i].HTTPOk = checkHTTP(url)
		}(i, t.HealthURL)
	}
	wg.Wait()
	return out
}

func describe(t ServiceTarget, props map[string]string, bootUptime float64) ServiceStatus {
	s := ServiceStatus{
		Name:      t.Name,
		Unit:      t.Unit,
		Group:     t.Group,
		State:     "n/a",
		PublicURL: t.PublicURL,
		Repo:      t.Repo,
		HasHealth: t.HealthURL != "",
		Note:      readNote(t.NoteFile),
	}
	if s.Group == "" {
		s.Group = GroupApp
	}
	if t.Unit == "" {
		return s
	}

	s.State = props["ActiveState"]
	s.Sub = props["SubState"]
	if s.State == "" {
		s.State = "unknown"
	}
	s.Active = s.State == "active"
	s.Enabled = unitShouldRun(props["UnitFileState"])
	s.Restarts, _ = strconv.Atoi(props["NRestarts"])

	// MemoryCurrent is "[not set]" for units without memory accounting, and a
	// huge sentinel on older systemd; both parse to junk.
	if mem, err := strconv.ParseUint(props["MemoryCurrent"], 10, 64); err == nil && mem < 1<<62 {
		s.MemMB = float64(mem) / 1024 / 1024
	}
	// MemoryMax is that same sentinel when the unit has no limit at all.
	if max, err := strconv.ParseUint(props["MemoryMax"], 10, 64); err == nil && max < 1<<62 {
		s.MemMaxMB = float64(max) / 1024 / 1024
	}
	// Same sentinel treatment as memory: "[not set]" on a unit with no accounting.
	// This is only the running total — the collector turns it into a rate.
	if cpu, err := strconv.ParseUint(props["CPUUsageNSec"], 10, 64); err == nil && cpu < 1<<62 {
		s.CPUNSec = cpu
	}
	// A unit's cgroup is a file read away, so the kill count and pressure are free.
	cgroup := filepath.Join("/sys/fs/cgroup/system.slice", t.Unit)
	s.OOMKills = cgroupOOMKills(cgroup)
	s.MemPressure = readPressure(cgroup, "memory.pressure")

	// systemd reports when the unit went active as microseconds on the same
	// monotonic clock as /proc/uptime, so the difference is its uptime — no
	// timezone parsing, no clock skew.
	if us, err := strconv.ParseUint(props["ActiveEnterTimestampMonotonic"], 10, 64); err == nil && us > 0 && s.Active {
		if up := bootUptime - float64(us)/1e6; up > 0 {
			s.UptimeSec = up
		}
	}
	return s
}

// unitShouldRun reports whether systemd would start this unit at boot. A unit
// that is disabled or masked is off because someone meant it to be off, and
// nothing here should drag it back up.
func unitShouldRun(fileState string) bool {
	switch fileState {
	case "enabled", "enabled-runtime", "static", "indirect", "generated", "alias":
		return true
	default: // disabled, masked, masked-runtime, bad, or unreadable
		return false
	}
}

// showUnits asks systemd about every unit in one call. Output is one block of
// key=value lines per unit, in the order requested, separated by a blank line.
func showUnits(units []string) map[string]map[string]string {
	result := make(map[string]map[string]string, len(units))
	if len(units) == 0 {
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	args := append([]string{"show"}, units...)
	args = append(args,
		"-p", "Id", // keyed on, so a block can never be attributed to the wrong unit
		"-p", "ActiveState",
		"-p", "SubState",
		"-p", "UnitFileState",
		"-p", "ActiveEnterTimestampMonotonic",
		"-p", "MemoryCurrent",
		"-p", "MemoryMax",
		"-p", "NRestarts",
		// Cumulative CPU time. It rides along in the call we already make, so
		// per-service CPU costs no extra process. Note there is nothing to switch
		// on first: under cgroup v2 accounting is implicit, and the matching
		// CPUAccounting property reads back as an empty string, which looks
		// exactly like "disabled" and is not.
		"-p", "CPUUsageNSec",
	)

	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		// Every service will read as "unknown" — say why once, rather than
		// leaving a dashboard full of question marks with no explanation.
		log.Printf("metrics: systemctl show failed: %v", err)
		return result
	}

	for _, block := range strings.Split(strings.TrimSpace(string(out)), "\n\n") {
		props := make(map[string]string, 7)
		for _, line := range strings.Split(block, "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
				props[k] = v
			}
		}
		if id := props["Id"]; id != "" {
			result[id] = props
		}
	}
	return result
}

func checkHTTP(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	// A login redirect or a 404 still means the app is alive and serving; only a
	// server-side failure (or no answer at all) counts as unhealthy.
	return resp.StatusCode < 500
}

// readNote returns the first line of a status file a job writes for the
// dashboard, cut to a length one row can hold. Missing or empty reads as "".
func readNote(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > 80 {
		line = string(r[:80])
	}
	return line
}
