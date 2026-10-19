package collector

import (
	"sync/atomic"
	"time"

	"github.com/osama-salman99/fleet-manager/deployer"
	"github.com/osama-salman99/fleet-manager/metrics"
)

type CPUMetric struct {
	Overall float64   `json:"overall"`
	Cores   []float64 `json:"cores"`
}

type MemMetric struct {
	TotalMB     float64 `json:"total_mb"`
	UsedMB      float64 `json:"used_mb"`
	CachedMB    float64 `json:"cached_mb"`
	BuffersMB   float64 `json:"buffers_mb"`
	FreeMB      float64 `json:"free_mb"`
	SwapTotalMB float64 `json:"swap_total_mb"`
	SwapUsedMB  float64 `json:"swap_used_mb"`
}

type Snapshot struct {
	Timestamp int64                    `json:"ts"`
	Host      metrics.HostInfo         `json:"host"`
	CPU       CPUMetric                `json:"cpu"`
	Mem       MemMetric                `json:"mem"`
	Disks     []metrics.DiskCapacity   `json:"disks"`
	Storage   metrics.StorageBreakdown `json:"storage"`
	Nets      []metrics.NetMetric      `json:"nets"`
	Services  []metrics.ServiceStatus  `json:"services"`
}

// infraServices keep the VPS running but are not deployed from GitHub, so the
// deployer has never heard of them. They are still the first thing you want to
// see when a site stops answering.
var infraServices = []metrics.ServiceTarget{
	{Name: "caddy", Unit: "caddy.service", Group: metrics.GroupInfra},
	{Name: "coder", Unit: "coder.service", PublicURL: "https://coder.osmosis.work", Group: metrics.GroupInfra},
	{Name: "coder-favicon-proxy", Unit: "coder-favicon-proxy.service", Group: metrics.GroupInfra},
	{Name: "build-watchdog", Unit: "build-watchdog.service", Group: metrics.GroupInfra},
	// Checks Hetzner hourly for a cheaper server at least as good as ours, at least
	// as close to Amsterdam. The timer is what stays up; the note says what it found.
	{Name: "hetzner-deals", Unit: "hetzner-watch.timer", Group: metrics.GroupInfra,
		NoteFile: "/var/lib/hetzner-watch/health-note.txt"},
}

var latest atomic.Value
var latestServices atomic.Value
var latestStorage atomic.Value

func Latest() (Snapshot, bool) {
	v := latest.Load()
	if v == nil {
		return Snapshot{}, false
	}
	return v.(Snapshot), true
}

// serviceTargets is the deployer's list of apps plus our infrastructure. Reading
// the apps from the deployer config means the dashboard cannot drift out of sync
// with what actually gets deployed — add a service there and it shows up here.
func serviceTargets() []metrics.ServiceTarget {
	apps := deployer.Get().Services
	targets := make([]metrics.ServiceTarget, 0, len(apps)+len(infraServices))
	for _, s := range apps {
		targets = append(targets, metrics.ServiceTarget{
			Name:      s.Name,
			Unit:      s.Unit,
			HealthURL: s.HealthURL,
			PublicURL: s.PublicURL,
			Repo:      s.Repo,
			Group:     metrics.GroupApp,
		})
	}
	return append(targets, infraServices...)
}

// markDeploying flags the services the deployer is installing, so the restart in
// the middle of a deploy is not read as an outage by the dashboard or acted on by
// the watchdog.
//
// A probe is not instant — it shells out to systemd and waits on HTTP — so a
// deploy that was running at either end of it could have restarted the unit while
// it was being read. Both ends count; requiring the deploy to still be running
// when the probe returns would let a short one land a red card anyway.
func markDeploying(statuses []metrics.ServiceStatus, atStart map[string]deployer.InFlight) {
	for i := range statuses {
		d, ok := deployer.Deploying(statuses[i].Name)
		if !ok {
			d, ok = atStart[statuses[i].Name]
		}
		if ok {
			statuses[i].Deploying = true
			statuses[i].DeployTag = d.Tag
		}
	}
}

// workspace is the Coder container. It is not a systemd unit, so systemd cannot
// see it — and it is the largest consumer on the VPS, the only thing with a
// memory limit of its own, and where every Claude session lives. Leaving it off
// the dashboard left the biggest number off the dashboard.
var workspace = metrics.ContainerTarget{
	Name:      "coder-workspace",
	Container: "coder-osmosis-osmosis-dot-work",
	PublicURL: "https://coder.osmosis.work",
	Group:     metrics.GroupApp,
}

// Services are polled on their own slower clock: each one costs a systemctl call
// and an HTTP request, and none of it changes second to second.
func runServices() {
	// CPU is a counter, not a level: the kernel reports time burned since each
	// cgroup was created, so a rate needs the previous poll to subtract from.
	// Keyed on unit, which is unique per row and stable across polls.
	prevCPU := map[string]uint64{}
	var prevAt time.Time

	update := func() {
		atStart := deployer.DeployingAll()
		statuses := metrics.ReadServiceStatuses(serviceTargets())
		markDeploying(statuses, atStart)
		statuses = append(statuses, metrics.ReadContainer(workspace))

		// Ten seconds between polls, deliberately: sampling CPU over a wider
		// window than the per-second overall figure smooths out the spikes that
		// make a per-service number jump around and mean nothing.
		now := time.Now()
		if !prevAt.IsZero() {
			elapsed := now.Sub(prevAt)
			for i := range statuses {
				if prev, ok := prevCPU[statuses[i].Unit]; ok {
					statuses[i].CPUPct = metrics.CPUShareOfVPS(prev, statuses[i].CPUNSec, elapsed)
				}
			}
		}
		prevCPU = make(map[string]uint64, len(statuses))
		for _, s := range statuses {
			if s.CPUNSec > 0 {
				prevCPU[s.Unit] = s.CPUNSec
			}
		}
		prevAt = now

		latestServices.Store(statuses)
	}
	update()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		update()
	}
}

// Storage is du'd on its own slow clock: a full scan of the container's overlay
// storage costs real I/O and the numbers barely move minute to minute, so it runs
// far less often than the per-second metrics and every snapshot reads the cached
// result.
func runStorage() {
	update := func() {
		if sb, err := metrics.ReadStorageBreakdown(); err == nil {
			latestStorage.Store(sb)
		}
	}
	update()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		update()
	}
}

func Run() {
	go runServices()
	go runStorage()

	prevCPU, _ := metrics.ReadCPU()
	prevNet, _ := metrics.ReadNetSamples()
	prevTime := time.Now()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		elapsed := now.Sub(prevTime).Seconds()

		currCPU, _ := metrics.ReadCPU()
		mem, _ := metrics.ReadMemory()
		disks, _ := metrics.ReadDiskCapacity()
		currNet, _ := metrics.ReadNetSamples()

		overall := metrics.CPUPercent(prevCPU.Overall, currCPU.Overall)
		cores := make([]float64, len(currCPU.Cores))
		for i, c := range currCPU.Cores {
			if i < len(prevCPU.Cores) {
				cores[i] = metrics.CPUPercent(prevCPU.Cores[i], c)
			}
		}

		var services []metrics.ServiceStatus
		if v := latestServices.Load(); v != nil {
			services = v.([]metrics.ServiceStatus)
		}

		var storage metrics.StorageBreakdown
		if v := latestStorage.Load(); v != nil {
			storage = v.(metrics.StorageBreakdown)
		}

		snap := Snapshot{
			Timestamp: now.Unix(),
			Host:      metrics.ReadHost(len(cores)),
			CPU: CPUMetric{
				Overall: overall,
				Cores:   cores,
			},
			Mem: MemMetric{
				TotalMB:     mem.TotalMB,
				UsedMB:      mem.UsedMB,
				CachedMB:    mem.CachedMB,
				BuffersMB:   mem.BuffersMB,
				FreeMB:      mem.FreeMB,
				SwapTotalMB: mem.SwapTotalMB,
				SwapUsedMB:  mem.SwapUsedMB,
			},
			Disks:    disks,
			Storage:  storage,
			Nets:     metrics.NetMetrics(prevNet, currNet, elapsed),
			Services: services,
		}

		latest.Store(snap)

		prevCPU = currCPU
		prevNet = currNet
		prevTime = now
	}
}
