package deployer

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// ---------- waking the build runner ----------
//
// The build runner on this VPS (/opt/build-runner/dispatch.py) used to find queued
// builds by asking GitHub every 20 s, so a push sat idle for 10-30 s before any
// building started. GitHub already tells us the moment a build job is queued (the
// workflow_job webhook), so we pass that on: a file named after the repo in
// buildWakeDir, which the dispatcher watches and acts on within half a second.

var buildWakeDir = "/run/build-runner/wake"

// A push to master reaches us ~2 s before GitHub queues the build it starts. A file in
// buildPushDir lets the dispatcher start a runner in that gap, so it is already listening when
// the job arrives (and stops it again if no job comes).
var buildPushDir = "/run/build-runner/push"

// wakeBuildRunner is called for every workflow_job event. Only a newly queued job
// that asks for the VPS runner needs one started.
func wakeBuildRunner(action, fullName string, labels []string) {
	if action != "queued" || fullName == "" {
		return
	}
	vps := false
	for _, l := range labels {
		vps = vps || l == "vps"
	}
	if !vps {
		return
	}
	leaveWake(buildWakeDir, fullName)
}

// wakeBuildRunnerOnPush is called for a push to master of a deployed service: its
// release build is about to be queued.
func wakeBuildRunnerOnPush(fullName string) {
	leaveWake(buildPushDir, fullName)
}

func leaveWake(dir, fullName string) {
	repo := fullName[strings.LastIndexByte(fullName, '/')+1:]
	if repo == "" || strings.HasPrefix(repo, ".") {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("deployer: cannot wake the build runner for %s: %v", repo, err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, repo), nil, 0o600); err != nil {
		log.Printf("deployer: cannot wake the build runner for %s: %v", repo, err)
	}
}
