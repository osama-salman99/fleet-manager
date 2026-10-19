package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Coder workspace container is the largest single consumer on this VPS — more
// than any crawler — and the only thing running on it with a memory limit of its
// own. It is where the Claude sessions live, and on 2026-07-12 it OOM-killed one
// of them against its own 1.5 GiB cap while the host still had RAM to spare.
//
// It is not a systemd unit, so nothing else in this package can see it: the
// dashboard's biggest number was the one thing missing from the dashboard.

// ContainerTarget is a docker container to watch. The name docker knows it by is
// not the name a human wants to read on a dashboard, so they are separate.
type ContainerTarget struct {
	Name      string // what to call it on the page
	Container string // what docker calls it
	PublicURL string
	Group     string
}

// Talk to docker over its unix socket rather than shelling out to the CLI: a
// `docker inspect` every 10s is a fork and ~30 MB of transient RSS on a VPS that
// has already been OOM-killed for less.
var dockerHTTP = &http.Client{
	Timeout: 4 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	},
}

type dockerInspect struct {
	ID    string `json:"Id"`
	State struct {
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
}

// ReadContainer reports a container as if it were a service, so it can sit on the
// dashboard next to everything else.
func ReadContainer(t ContainerTarget) ServiceStatus {
	s := ServiceStatus{
		// Not a systemd unit, but the dashboard keys "is there something to judge"
		// off this field — an empty one means "static site, nothing runs".
		Name:      t.Name,
		Unit:      "docker/" + t.Container,
		Group:     t.Group,
		PublicURL: t.PublicURL,
		State:     "unknown",
		Enabled:   true, // a container is not something systemd switches on and off
	}

	insp, err := inspect(t.Container)
	if err != nil {
		log.Printf("metrics: cannot inspect container %s: %v", t.Container, err)
		return s
	}

	s.State = "inactive"
	s.Sub = "exited"
	if insp.State.Running {
		s.State = "active"
		s.Sub = "running"
		s.Active = true
	}
	if started, err := time.Parse(time.RFC3339Nano, insp.State.StartedAt); err == nil && s.Active {
		s.UptimeSec = time.Since(started).Seconds()
	}

	// The cgroup carries the numbers that matter: what it is using, and how close
	// that is to the limit it dies at.
	scope := filepath.Join("/sys/fs/cgroup/system.slice", "docker-"+insp.ID+".scope")

	// Report what the container cannot give back, not memory.current.
	//
	// memory.current includes page cache, and this container does enough file I/O
	// — jars, Gradle caches, node_modules, transcripts — to hold 500+ MB of it.
	// That cache is dropped the moment anything needs the memory, so counting it
	// overstates the container by a third and makes a healthy VPS look full. Only
	// anon and slab can actually collide with the limit, which is why the /session
	// guard already gates on exactly these two. The card and the guard must not
	// disagree about how full the thing is.
	//
	// The systemd units do not need this treatment: their file cache is 0-8 MB, so
	// their MemoryCurrent is honest already.
	stat := readCgroupStat(scope)
	s.MemMB = float64(stat["anon"]+stat["slab"]) / 1024 / 1024
	if max, ok := readCgroupInt(scope, "memory.max"); ok {
		s.MemMaxMB = float64(max) / 1024 / 1024
	}
	s.OOMKills = cgroupOOMKills(scope)
	s.MemPressure = readPressure(scope, "memory.pressure")
	s.Breakdown = readBreakdown(scope, s.MemMB)

	// The container has no systemd unit to ask, so its cumulative CPU comes from
	// the same cgroup its memory does. cpu.stat counts microseconds; everything
	// downstream works in nanoseconds, like systemd's CPUUsageNSec.
	if usec, ok := readCgroupKV(scope, "cpu.stat")["usage_usec"]; ok {
		s.CPUNSec = usec * 1000
	}

	return s
}

// readBreakdown splits a container's memory across the processes inside it.
//
// The workspace sits at the top of the dashboard every single time, because it is
// three Claude sessions and whatever they have started — and one number for all of
// that says only "it is big", never "a Gradle daemon has been holding 700 MB doing
// nothing since lunch", which is the thing you actually came to find out.
//
// Two decisions make the parts add up to the whole they are drawn under:
//
// Anonymous PSS, not RSS. RSS counts a shared page once per process, and the
// Claude sessions share a large interpreter and its libraries — summing their RSS
// gives more memory than the container holds, which would be a breakdown that
// exceeds the thing it breaks down. PSS gives each sharer its fraction, so the
// sum is honest. And *anonymous* PSS because the card's figure is anon+slab, page
// cache deliberately excluded (see above): file-backed pages must be left out here
// too, or the segments would total more than the bar they sit in.
//
// Whatever the processes do not account for is the kernel's own memory (slab,
// stacks) plus anything that came and went between the two reads. It is named
// rather than hidden, because a remainder folded silently into the largest group
// is how a breakdown starts lying.
func readBreakdown(scope string, totalMB float64) []ProcGroup {
	pids := readCgroupProcs(scope)
	if len(pids) == 0 {
		return nil
	}

	groups := map[string]*ProcGroup{}
	var accounted float64
	for _, pid := range pids {
		argv := readCmdline(pid)
		if len(argv) == 0 {
			continue // exited between listing and reading, or has no command line
		}
		name := classifyProc(argv)
		g := groups[name]
		if g == nil {
			g = &ProcGroup{Name: name}
			groups[name] = g
		}
		mb := pssAnonMB(pid)
		g.MemMB += mb
		g.Count++
		accounted += mb
	}

	out := make([]ProcGroup, 0, len(groups)+1)
	for _, g := range groups {
		if g.MemMB >= 1 { // a sliver is a legend row nobody can see a segment for
			out = append(out, *g)
		}
	}
	if rest := totalMB - accounted; rest >= 1 {
		out = append(out, ProcGroup{Name: "Kernel & overhead", MemMB: rest})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MemMB > out[j].MemMB })
	return out
}

// classifyProc names a process the way a person would. Order matters: the Gradle
// daemon is a java process, and it is the one worth calling out by name — it is
// the thing that quietly sits on half a gigabyte long after a build is done.
func classifyProc(argv []string) string {
	full := strings.Join(argv, " ")
	exe := filepath.Base(argv[0])
	switch {
	case strings.Contains(full, "GradleDaemon"), strings.Contains(full, "gradle-daemon-main"):
		return "Gradle daemon"
	case exe == "java":
		return "Java"
	case strings.HasPrefix(exe, "claude"):
		return "Claude sessions"
	case exe == "coder":
		return "Coder agent"
	case strings.HasPrefix(exe, "tmux"):
		return "tmux"
	case exe == "node":
		return "Node"
	default:
		return "Shells & tools"
	}
}

// readCgroupProcs lists the processes in a cgroup. These are host PIDs — this
// process runs on the host, outside the container's PID namespace — which is
// exactly what makes /proc readable for them.
func readCgroupProcs(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	pids := strings.Fields(string(b))
	// A runaway build could fill this cgroup with short-lived compiler processes,
	// and every entry costs a page-table walk below. The tail of that list is all
	// slivers anyway; it lands in the remainder.
	if len(pids) > 256 {
		pids = pids[:256]
	}
	return pids
}

func readCmdline(pid string) []string {
	b, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err != nil || len(b) == 0 {
		return nil
	}
	argv := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	if len(argv) == 0 || argv[0] == "" {
		return nil
	}
	return argv
}

// pssAnonMB is the process's fair share of the anonymous memory it holds: pages it
// owns outright, plus its slice of any it shares.
func pssAnonMB(pid string) float64 {
	b, err := os.ReadFile("/proc/" + pid + "/smaps_rollup")
	if err != nil {
		return 0 // died mid-read, or a kernel thread with nothing to roll up
	}
	for _, line := range strings.Split(string(b), "\n") {
		v, ok := strings.CutPrefix(line, "Pss_Anon:")
		if !ok {
			continue
		}
		kb, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 64)
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

func inspect(name string) (dockerInspect, error) {
	var d dockerInspect
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://docker/containers/"+name+"/json", nil)
	if err != nil {
		return d, err
	}
	resp, err := dockerHTTP.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return d, fmt.Errorf("docker returned %s", resp.Status)
	}
	return d, json.NewDecoder(resp.Body).Decode(&d)
}

// readCgroupInt reads one number out of a cgroup file. "max" — no limit — reads
// as absent rather than as a number, because a limit of "everything" is not a
// limit worth drawing a bar against.
func readCgroupInt(dir, file string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return 0, false
	}
	txt := strings.TrimSpace(string(b))
	if txt == "max" {
		return 0, false
	}
	v, err := strconv.ParseUint(txt, 10, 64)
	if err != nil || v >= 1<<62 { // systemd writes a huge sentinel for "no limit"
		return 0, false
	}
	return v, true
}

// readCgroupStat parses memory.stat into its key/value pairs.
func readCgroupStat(dir string) map[string]uint64 {
	return readCgroupKV(dir, "memory.stat")
}

// readCgroupKV parses any of the cgroup v2 "key value" files (memory.stat,
// cpu.stat, …) into a map.
func readCgroupKV(dir, file string) map[string]uint64 {
	out := map[string]uint64{}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			n, err := strconv.ParseUint(v, 10, 64)
			if err == nil {
				out[k] = n
			}
		}
	}
	return out
}

// cgroupOOMKills is how many processes the kernel has killed inside this cgroup
// for running it out of memory. It resets when the cgroup is recreated, so it
// means "since this thing started", which is the useful reading.
func cgroupOOMKills(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok && k == "oom_kill" {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}
