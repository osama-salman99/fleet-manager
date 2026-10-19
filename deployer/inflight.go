package deployer

import (
	"sync"
	"time"
)

// A deploy stops the unit, swaps the artifact and starts it again, so for a few
// seconds systemd reports exactly what a crash reports. Everything that reacts
// to a service being down — the dashboard's alarm, the watchdog's restart-and-
// page — has to be able to tell the two apart, so a running deploy publishes
// itself here for the rest of the process to read.
type InFlight struct {
	Tag   string    `json:"tag"`
	Since time.Time `json:"since"`
}

// A deploy that never finishes must not mute a service forever: download has no
// timeout, so a stalled GitHub connection could otherwise hide a real outage for
// as long as the process lives. Everything else in a deploy is bounded well
// inside this, so a marker older than it is a leak, not a slow deploy.
const inFlightTTL = 10 * time.Minute

var (
	inFlightMu sync.RWMutex
	inFlight   = map[string]InFlight{} // service name -> deploy currently running
)

// The map above publishes what is happening; the locks below decide what is
// *allowed* to happen. Keeping them apart matters: the in-flight map enforces
// nothing, and reading it as if it did is how two deploys of one service ran at
// the same time — the second one backing its jar up over the first one's backup,
// so when both failed there was nothing left to restore.
var (
	lockMu      sync.Mutex
	deployLocks = map[string]*sync.Mutex{} // service name -> deploy lock
)

func serviceLock(name string) *sync.Mutex {
	lockMu.Lock()
	defer lockMu.Unlock()
	l, ok := deployLocks[name]
	if !ok {
		l = &sync.Mutex{}
		deployLocks[name] = l
	}
	return l
}

// The per-service lock above stops one service deploying over itself; this stops
// the whole fleet deploying at once. The VPS is memory-tight enough that two JVMs
// starting together OOM-kill one of them — the reason pushes have to be staggered
// by hand — and a crawler-core bump publishes several crawler releases within
// seconds, each arriving as its own webhook and starting its own deploy goroutine.
// The sequential poller is already safe; this closes the concurrent-webhook path.
//
// A deploy that finds the gate taken WAITS for it, up to fleetWait. Before
// 2026-10-14 it was skipped instead and left for the 10-minute poll, so any release
// that landed while another service was deploying sat "waiting to deploy" for up to
// 10 minutes (home-app 8 min, transport-service 7.5 min that week). The wait is
// bounded because a download has no timeout: a stalled one must not hold every
// other release forever — past fleetWait the deploy is skipped as before and the
// poller converges. A burst still deploys one JVM at a time, just back to back.
var fleetDeployMu sync.Mutex

// fleetWait is how long a deploy waits for another service's deploy to finish.
// A normal deploy takes 2-20 s; even with every retry one is under 3 minutes.
var fleetWait = 10 * time.Minute

func lockFleetDeploy() bool { return fleetDeployMu.TryLock() }

// waitFleetDeploy takes the fleet gate, waiting up to max for it to come free.
func waitFleetDeploy(max time.Duration) bool {
	deadline := time.Now().Add(max)
	for !fleetDeployMu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return true
}

func unlockFleetDeploy() { fleetDeployMu.Unlock() }

// lockService takes a service's deploy lock, or reports that a deploy is already
// running for it.
//
// It does not queue. A download has no timeout, so waiting would pile the poller,
// the webhook and every redeploy click up behind a stalled connection. Skipping
// is safe because the poller comes back every 10 minutes and re-compares tags, so
// a deploy dropped here converges on the next tick.
func lockService(name string) bool { return serviceLock(name).TryLock() }

func unlockService(name string) { serviceLock(name).Unlock() }

// Deploying reports the deploy running for a service, if there is one.
func Deploying(name string) (InFlight, bool) {
	inFlightMu.RLock()
	defer inFlightMu.RUnlock()
	d, ok := inFlight[name]
	if !ok || time.Since(d.Since) > inFlightTTL {
		return InFlight{}, false
	}
	return d, true
}

// DeployingAll is every deploy running right now, keyed by service name.
func DeployingAll() map[string]InFlight {
	inFlightMu.RLock()
	defer inFlightMu.RUnlock()
	out := make(map[string]InFlight, len(inFlight))
	for name, d := range inFlight {
		if time.Since(d.Since) <= inFlightTTL {
			out[name] = d
		}
	}
	return out
}

func beginDeploy(name, tag string) {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	inFlight[name] = InFlight{Tag: tag, Since: time.Now()}
}

func endDeploy(name string) {
	inFlightMu.Lock()
	defer inFlightMu.Unlock()
	delete(inFlight, name)
}
