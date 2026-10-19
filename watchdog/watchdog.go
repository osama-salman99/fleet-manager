package watchdog

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/osama-salman99/fleet-manager/collector"
	"github.com/osama-salman99/fleet-manager/metrics"
	"github.com/osama-salman99/fleet-manager/ntfy"
)

const (
	restartAfter  = 30 * time.Second
	notifyAfter   = 60 * time.Second
	checkInterval = 10 * time.Second
)

type serviceState struct {
	downSince        time.Time
	restartAttempted bool
	notifiedDown     bool
}

func Run() {
	states := make(map[string]*serviceState)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		snap, ok := collector.Latest()
		if !ok {
			continue
		}
		for _, svc := range snap.Services {
			check(svc, states)
		}
	}
}

func check(svc metrics.ServiceStatus, states map[string]*serviceState) {
	// Three things that look like an outage but are not, and restarting or
	// paging for any of them is worse than doing nothing:
	//   - a static site has no unit to restart
	//   - "unknown" means we could not ask systemd, not that the unit is gone
	//   - a disabled unit is off because someone switched it off
	if svc.Unit == "" || svc.State == "unknown" || !svc.Enabled {
		delete(states, svc.Name)
		return
	}

	// A deploy is already restarting this unit on purpose. Stand back entirely —
	// and without touching the state, because a deploy is not a recovery: if the
	// service was down before it started, it is still down, and clearing the
	// timers here would page "recovered" for a service nobody has fixed yet.
	if svc.Deploying {
		return
	}

	st, ok := states[svc.Name]
	if !ok {
		st = &serviceState{}
		states[svc.Name] = st
	}

	// "activating" is on its way up; only a definitive stop counts as down.
	if !svc.Down() {
		if st.notifiedDown {
			log.Printf("watchdog: %s recovered, sending notification", svc.Name)
			ntfy.Send("watchdog", "Service Recovered", fmt.Sprintf("%s is back up", svc.Name), "white_check_mark", "default")
			st.notifiedDown = false
		}
		st.downSince = time.Time{}
		st.restartAttempted = false
		return
	}

	if st.downSince.IsZero() {
		st.downSince = time.Now()
		log.Printf("watchdog: %s went down", svc.Name)
		return
	}

	down := time.Since(st.downSince)

	if down >= restartAfter && !st.restartAttempted {
		log.Printf("watchdog: restarting %s (down %v)", svc.Name, down.Round(time.Second))
		restart(svc.Unit)
		st.restartAttempted = true
	}

	if down >= notifyAfter && !st.notifiedDown {
		log.Printf("watchdog: notifying %s down (down %v)", svc.Name, down.Round(time.Second))
		ntfy.Send("watchdog", "Service Down", fmt.Sprintf("%s has been down for over 1 minute", svc.Name), "rotating_light", "high")
		st.notifiedDown = true
	}
}

func restart(unit string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "systemctl", "restart", unit).Run(); err != nil {
		log.Printf("watchdog: failed to restart %s: %v", unit, err)
	}
}

// ntfy sending (with the ASCII-Title fold, and the deadline that keeps a hung
// POST from stalling this ticker loop) now lives in the shared ntfy package.
