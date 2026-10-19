package deployer

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Decommission takes a service out of service on this host: it stops the unit,
// takes away its boot autostart, deletes the build the deployer installed (plus
// the bookkeeping that records what is installed), and switches the service off
// in the config so neither the poller nor the watchdog brings it back.
//
// It removes only what the deployer itself put there. The systemd unit and its
// drop-ins, the Caddy block and the service's own data directory were installed
// by hand and the deployer cannot recreate them, so they stay — which is exactly
// what makes this reversible: pressing Redeploy afterwards installs the latest
// release and starts the service again. Deleting those as well would be a
// teardown, and a teardown behind a button is one misclick from unrecoverable.
//
// The returned lines are what actually happened, in order, for the log and for
// the page that asked — a step that could not be done is reported rather than
// silently skipped, because "decommissioned" has to mean it.
func Decommission(name string) ([]string, error) {
	var svc Service
	var found bool
	for _, s := range Get().Services {
		if s.Name == name {
			svc, found = s, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("unknown service %q", name)
	}
	// This code runs inside vps-health: stopping that unit would kill the request
	// mid-flight and deleting the binary would leave nothing to start again.
	if isSelf(svc) {
		return nil, fmt.Errorf("refusing to decommission %s — it is the deployer running this request", svc.Name)
	}
	// Racing an install would delete files from underneath it and leave a
	// half-written build behind the .deployed-tag we just removed.
	if d, busy := Deploying(svc.Name); busy {
		return nil, fmt.Errorf("%s is being deployed right now (%s) — try again once it settles", svc.Name, d.Tag)
	}

	var steps []string
	note := func(format string, a ...any) { steps = append(steps, fmt.Sprintf(format, a...)) }

	// Stop, then disable. Disabling matters as much as stopping: an inactive unit
	// still gets restarted by the watchdog, while a *disabled* one is how this host
	// says "off on purpose" (see watchdog.go / unitShouldRun). Without it the
	// service would be back within 30 seconds.
	if svc.Unit != "" {
		for _, verb := range []string{"stop", "disable"} {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			out, err := exec.CommandContext(ctx, "systemctl", verb, svc.Unit).CombinedOutput()
			cancel()
			if err != nil {
				// Already stopped or already disabled is the end state we want, so
				// record what systemd said and carry on rather than abort half-done.
				note("systemctl %s %s: %v (%s)", verb, svc.Unit, err, strings.TrimSpace(string(out)))
				continue
			}
			note("systemctl %s %s", verb, svc.Unit)
		}
	}

	// Delete the installed build and any rollback copies sitting beside it. For a
	// tarball service the target IS the unpacked directory, so the served tree is
	// what was deployed.
	if err := removableTarget(svc.Target); err != nil {
		note("left %s in place: %v", svc.Target, err)
	} else if svc.Kind == KindTarball {
		if err := os.RemoveAll(svc.Target); err != nil {
			note("could not remove %s: %v", svc.Target, err)
		} else {
			note("removed %s", svc.Target)
		}
	} else {
		if err := os.Remove(svc.Target); err != nil && !os.IsNotExist(err) {
			note("could not remove %s: %v", svc.Target, err)
		} else if err == nil {
			note("removed %s", svc.Target)
		}
		for _, b := range backups(svc) {
			if os.Remove(b) == nil {
				note("removed %s", filepath.Base(b))
			}
		}
	}

	// Forget what was installed and any recorded failure, so a later Redeploy
	// starts from a clean slate instead of comparing against a build that is gone.
	for _, p := range []string{deployedTagPath(svc), failedTagPath(svc)} {
		if os.Remove(p) == nil {
			note("removed %s", filepath.Base(p))
		}
	}

	// Last, because it is the part that keeps the rest true: with Enabled off the
	// poller skips the service, the watchdog leaves it alone, and it drops out of
	// the public fleet feed instead of reading as "down".
	c := Get()
	for i := range c.Services {
		if c.Services[i].Name == svc.Name {
			c.Services[i].Enabled = false
		}
	}
	if err := Put(c); err != nil {
		return steps, fmt.Errorf("stopped and removed %s, but could not switch it off in the config: %w", svc.Name, err)
	}
	note("switched off in deploy-config")

	log.Printf("deployer: decommissioned %s — %s", svc.Name, strings.Join(steps, "; "))
	return steps, nil
}

// removableTarget refuses paths that are too shallow to be a service's own
// install location. Target comes from the config file, and a typo there ("/opt",
// or an empty string that would make this "/") is the difference between
// decommissioning one service and erasing the host.
func removableTarget(target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("no target path configured")
	}
	clean := filepath.Clean(target)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("target %q is not an absolute path", target)
	}
	if len(strings.Split(strings.Trim(clean, "/"), "/")) < 3 {
		return fmt.Errorf("target %q is too shallow to remove safely", clean)
	}
	return nil
}
