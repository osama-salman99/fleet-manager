package deployer

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricsRecordEachFinishedPushOnce(t *testing.T) {
	now := int64(1_800_000_000)
	at := func(sec int64) string { return time.Unix(now-sec, 0).UTC().Format(time.RFC3339) }
	metricsFile = filepath.Join(t.TempDir(), "m.jsonl")
	metricsSeen = nil
	rb := repoBuilds{
		runs: []ghRun{{ID: 7, HeadSHA: "abc1234ffff", Status: "completed", Conclusion: "success", Event: "push",
			StartedAt: at(160), UpdatedAt: at(20)}},
		jobSec:   map[int64]float64{7: 25},
		jobStart: map[int64]int64{7: now - 60},
	}
	live := deployState{tag: "v9-abc1234", liveAt: now - 30, deploySec: 5}

	m, ok := metricFor("svc", rb, live, now)
	if !ok {
		t.Fatal("a live push was not recorded")
	}
	// Run start 160 s ago, job start 60 s ago, live 30 s ago.
	if m.TotalSec != 130 || m.QueueSec != 100 || m.BuildSec != 25 || m.DeploySec != 5 || m.Outcome != "live" || m.Tag != "v9-abc1234" {
		t.Errorf("metric = %+v", m)
	}
	// Still building, or released and not yet live: nothing to record.
	if _, ok := metricFor("svc", rb, deployState{tag: "v8-old0000"}, now); ok {
		t.Error("recorded a push that is not live yet")
	}
	// Live but GitHub has not given the job time yet: wait, unless it is old.
	noJob := rb
	noJob.jobSec = nil
	if _, ok := metricFor("svc", noJob, live, now); ok {
		t.Error("recorded a push before its build time was known")
	}

	appendMetric(m)
	appendMetric(m)
	metricsSeen = nil // as after a restart
	appendMetric(m)
	b, _ := os.ReadFile(metricsFile)
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("file has %d lines, want 1", n)
	}

	// The estimate follows the recorded pushes as they come in.
	if b, d := Estimates("svc"); b != 25 || d != 5 {
		t.Errorf("estimates = %v %v, want 25 5", b, d)
	}
	m2 := m
	m2.Run, m2.BuildSec, m2.DeploySec = 8, 40, 9
	appendMetric(m2)
	m3 := m2
	m3.Run = 9
	appendMetric(m3)
	if b, d := Estimates("svc"); b != 40 || d != 9 {
		t.Errorf("estimates after two slower pushes = %v %v, want 40 9", b, d)
	}

	rec := httptest.NewRecorder()
	HandleDeployMetrics(rec, httptest.NewRequest("GET", "/api/deploy-metrics?service=svc", nil))
	var got []PushMetric
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 3 || got[0].Run != 9 {
		t.Errorf("served %s", rec.Body.String())
	}
}
