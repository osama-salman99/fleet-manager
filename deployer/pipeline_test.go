package deployer

import (
	"testing"
	"time"
)

func TestPipelineFollowsAPushToLive(t *testing.T) {
	now := int64(1_800_000_000)
	at := func(sec int64) string { return time.Unix(now-sec, 0).UTC().Format(time.RFC3339) }
	run := func(status, concl string) repoBuilds {
		return repoBuilds{
			step:         "Build",
			buildStarted: now - 100,
			jobSec:       map[int64]float64{2: 30},
			runs: []ghRun{
				{HeadSHA: "abc1234ffff", Status: status, Conclusion: concl, StartedAt: at(120), UpdatedAt: at(60),
					HeadCommit: struct {
						Message string `json:"message"`
					}{"Fix the thing\n\nbody"}},
				{ID: 2, HeadSHA: "old0000", Status: "completed", Conclusion: "success", StartedAt: at(1000), UpdatedAt: at(900)},
				{HeadSHA: "old0001", Status: "completed", Conclusion: "success", StartedAt: at(2000), UpdatedAt: at(1960)},
			},
		}
	}
	cases := []struct {
		name  string
		rb    repoBuilds
		ds    deployState
		stage string
		show  bool
	}{
		{"building", run("in_progress", ""), deployState{tag: "v1-old0000"}, "building", true},
		{"built, deploy not started", run("completed", "success"), deployState{tag: "v1-old0000"}, "waiting", true},
		{"deploying", run("completed", "success"), deployState{tag: "v1-old0000", deploying: "v2-abc1234", since: now - 10}, "deploying", true},
		{"live just now", run("completed", "success"), deployState{tag: "v2-abc1234", liveAt: now - 30}, "live", true},
		{"live long ago", run("completed", "success"), deployState{tag: "v2-abc1234", liveAt: now - 3600}, "live", false},
		{"build failed", run("completed", "failure"), deployState{tag: "v1-old0000"}, "build_failed", true},
		{"deploy failed, retrying", run("completed", "success"), deployState{tag: "v1-old0000", failedTag: "v2-abc1234"}, "deploy_failed", true},
	}
	for _, c := range cases {
		p, show := pipelineFor("svc", c.rb, c.ds, now)
		if p.Stage != c.stage || show != c.show {
			t.Errorf("%s: stage %q show %v, want %q %v", c.name, p.Stage, show, c.stage, c.show)
		}
	}
	p, _ := pipelineFor("svc", run("in_progress", ""), deployState{deploySec: 90}, now)
	if p.SHA != "abc1234" || p.Title != "Fix the thing" || p.Step != "Build" || p.TypicalDeploySec != 90 || p.BuildStarted != now-100 {
		t.Errorf("pipeline = %+v", p)
	}
	// The median of the two finished builds: 30 s of job time for the first (its
	// run's 100 s included waiting for a runner) and 40 s for the second, whose
	// job time is unknown. The running one does not count.
	if p.TypicalBuildSec != 40 {
		t.Errorf("typical build = %v, want 40", p.TypicalBuildSec)
	}
}
