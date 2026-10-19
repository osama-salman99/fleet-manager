package watchdog

import (
	"context"
	"log"
	"os/exec"
	"strings"
	"time"
)

// buildUnit is the CI build-failure watchdog: a separate unit that reproduces a
// broken master build and pushes a fix. It is not the same thing as the service
// watchdog in this package, which restarts units that fall over.
const buildUnit = "build-watchdog.service"

// SetBuildWatchdog makes the unit match the toggle on the config page.
//
// Until now the toggle was decoration — it was written to the config file and
// nothing ever read it, so turning it off left the watchdog running and turning
// it on did not start it.
func SetBuildWatchdog(enabled bool) {
	if enabled == buildWatchdogRunning() {
		return
	}

	verb := "disable"
	if enabled {
		verb = "enable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// --now also starts/stops it, so the toggle takes effect immediately instead
	// of at the next reboot.
	out, err := exec.CommandContext(ctx, "systemctl", verb, "--now", buildUnit).CombinedOutput()
	if err != nil {
		log.Printf("watchdog: could not %s %s: %v: %s", verb, buildUnit, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("watchdog: %sd %s", verb, buildUnit)
}

func buildWatchdogRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", buildUnit).Output()
	return strings.TrimSpace(string(out)) == "active"
}
