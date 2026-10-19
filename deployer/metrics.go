package deployer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Every push that reaches the end of the line — live, or failed for good — gets
// one line in a JSONL file: how long it waited for a runner, built, deployed, and
// took in all. The Overview's bars only remember the last push per service; this
// keeps every one, so build and deploy times can be compared over weeks.

var metricsFile = "/opt/vps-health/deploy-metrics.jsonl"

// PushMetric is one finished push.
type PushMetric struct {
	At        int64   `json:"at"` // when it went live or failed
	Service   string  `json:"service"`
	Tag       string  `json:"tag,omitempty"`
	SHA       string  `json:"sha"`
	Title     string  `json:"title"`
	Trigger   string  `json:"trigger"`              // push / workflow_dispatch / …
	Run       int64   `json:"run"`                  // the GitHub run id
	Outcome   string  `json:"outcome"`              // live / build_failed / deploy_failed
	TotalSec  float64 `json:"total_sec"`            // run start to live (or failure)
	QueueSec  float64 `json:"queue_sec,omitempty"`  // waiting for a free runner
	BuildSec  float64 `json:"build_sec,omitempty"`  // the build job itself
	DeploySec float64 `json:"deploy_sec,omitempty"` // starting the install to live
}

// Pushes older than this when first seen are not recorded: on a fresh file the
// last push of every service would otherwise be logged with whatever is left of
// its numbers.
const metricsMaxAge = 24 * time.Hour

// A live push whose build time GitHub has not told us yet is held back this long
// before it is recorded without one.
const metricsJobGrace = 10 * time.Minute

// metricFor is the line to record for a service's newest push, if it has finished.
func metricFor(name string, rb repoBuilds, ds deployState, now int64) (PushMetric, bool) {
	p, _ := pipelineFor(name, rb, ds, now)
	if p.Stage == "" {
		return PushMetric{}, false
	}
	r := rb.runs[0]
	m := PushMetric{Service: name, Tag: p.Tag, SHA: p.SHA, Title: p.Title, Trigger: r.Event, Run: r.ID, Outcome: p.Stage}
	switch p.Stage {
	case "live":
		m.At, m.DeploySec = p.LiveAt, ds.deploySec
		m.BuildSec = rb.jobSec[r.ID]
		if m.BuildSec == 0 && now-p.LiveAt < int64(metricsJobGrace.Seconds()) {
			return PushMetric{}, false // GitHub's job times are a moment behind
		}
	case "build_failed":
		m.At = p.BuiltAt
	case "deploy_failed":
		if p.Retrying {
			return PushMetric{}, false // not finished yet
		}
		m.At = now
		m.BuildSec = rb.jobSec[r.ID]
	default:
		return PushMetric{}, false
	}
	if m.At == 0 || p.Started == 0 || now-m.At > int64(metricsMaxAge.Seconds()) {
		return PushMetric{}, false
	}
	m.TotalSec = float64(m.At - p.Started)
	if js := rb.jobStart[r.ID]; js >= p.Started {
		m.QueueSec = float64(js - p.Started)
	}
	return m, true
}

var (
	metricsMu   sync.Mutex
	metricsSeen map[string]bool // service|run already in the file
	// metricsRecent is each service's latest pushes that went live, oldest first:
	// what the Overview's progress estimates are made from. It is updated the
	// moment a push is recorded, so the next push is estimated from the last one.
	metricsRecent map[string][]PushMetric
)

// How many of a service's latest pushes its estimates are the median of.
const estimateFrom = 5

func noteRecent(m PushMetric) {
	if m.Outcome != "live" {
		return
	}
	r := append(metricsRecent[m.Service], m)
	if len(r) > estimateFrom {
		r = r[len(r)-estimateFrom:]
	}
	metricsRecent[m.Service] = r
}

// Estimates is the median build and deploy time of a service's latest recorded
// pushes, 0 for either when none has one.
func Estimates(service string) (buildSec, deploySec float64) {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	loadMetricsSeen()
	var b, d []float64
	for _, m := range metricsRecent[service] {
		if m.BuildSec > 0 {
			b = append(b, m.BuildSec)
		}
		if m.DeploySec > 0 {
			d = append(d, m.DeploySec)
		}
	}
	return median(b), median(d)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

func metricKey(service string, run int64) string {
	return service + "|" + strconv.FormatInt(run, 10)
}

// loadMetricsSeen reads which pushes the file already holds, once, so a restart
// does not record them twice. Called with metricsMu held.
func loadMetricsSeen() {
	if metricsSeen != nil {
		return
	}
	metricsSeen, metricsRecent = map[string]bool{}, map[string][]PushMetric{}
	f, err := os.Open(metricsFile)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m PushMetric
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			metricsSeen[metricKey(m.Service, m.Run)] = true
			noteRecent(m)
		}
	}
}

func appendMetric(m PushMetric) {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	loadMetricsSeen()
	k := metricKey(m.Service, m.Run)
	if metricsSeen[k] {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	f, err := os.OpenFile(metricsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("deployer: cannot record push metrics: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err == nil {
		metricsSeen[k] = true
		noteRecent(m)
	}
}

// recordFinished writes a line for every service whose newest push has finished
// and is not in the file yet.
func recordFinished() {
	now := time.Now().Unix()
	for _, s := range Get().Services {
		if !s.Enabled {
			continue
		}
		buildsMu.Lock()
		rb, ok := builds[s.Repo]
		var copyRB repoBuilds
		if ok {
			copyRB = *rb
		}
		buildsMu.Unlock()
		if !ok || len(copyRB.runs) == 0 {
			continue
		}
		metricsMu.Lock()
		loadMetricsSeen()
		seen := metricsSeen[metricKey(s.Name, copyRB.runs[0].ID)]
		metricsMu.Unlock()
		if seen {
			continue
		}
		if m, ok := metricFor(s.Name, copyRB, stateOf(s), now); ok {
			appendMetric(m)
		}
	}
}

// HandleDeployMetrics serves the recorded pushes, newest first. ?service= keeps
// one service's, ?limit= caps how many (default 200).
func HandleDeployMetrics(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	only := r.URL.Query().Get("service")
	out := []PushMetric{}
	metricsMu.Lock()
	f, err := os.Open(metricsFile)
	if err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var m PushMetric
			if json.Unmarshal(sc.Bytes(), &m) == nil && (only == "" || m.Service == only) {
				out = append(out, m)
			}
		}
		f.Close()
	}
	metricsMu.Unlock()
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("cannot read metrics: %v", err), http.StatusInternalServerError)
		return
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
