package deployer

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// A push travels build → release → deploy before it is live, and only the last
// leg happens on this VPS. The first two run on GitHub Actions, which sends us
// nothing until the release is out, so this file asks GitHub how each service's
// newest build is doing and joins that to what the deployer knows, giving the
// Overview one line per push from the moment it lands until it is live.

// Pipeline is one push on its way to being live, as the Overview shows it.
type Pipeline struct {
	Name  string `json:"name"`
	SHA   string `json:"sha"`   // short commit id
	Title string `json:"title"` // first line of the commit message
	URL   string `json:"url"`   // the build on GitHub
	// Stage is queued / building / releasing / waiting (released, deploy not
	// started) / deploying / live / build_failed / deploy_failed.
	Stage       string `json:"stage"`
	Step        string `json:"step,omitempty"`         // the build step running now
	Tag         string `json:"tag,omitempty"`          // the release, once it exists
	Started     int64  `json:"started"`                // when the build started
	BuiltAt     int64  `json:"built_at,omitempty"`     // when the build (and so the release) finished
	DeploySince int64  `json:"deploy_since,omitempty"` // when the deploy started
	LiveAt      int64  `json:"live_at,omitempty"`
	// When the build job itself started. Before that the build is waiting for a
	// free runner, which is not part of how long a build takes.
	BuildStarted int64 `json:"build_started,omitempty"`
	// How long this service usually takes, so the page can draw progress: the
	// median of its latest recorded pushes (deploy-metrics.jsonl), else of its
	// recent build jobs and its last deploy. Once live, the deploy figure is that
	// push's own, so its bar shows how long it really took.
	TypicalBuildSec  float64 `json:"typical_build_sec"`
	TypicalDeploySec float64 `json:"typical_deploy_sec"`
	Retrying         bool    `json:"retrying,omitempty"` // a failed deploy that will be tried again
}

// GitHub tells us about pushes and builds through the webhook (NudgeBuilds), so
// polling is only the fallback: every minute when nothing is building, and every
// 10 s while something is, because the step a build is on only comes from asking.
// The requests carry an ETag, and GitHub does not count an unchanged (304) answer
// against the rate limit, so either costs next to nothing.
const (
	pipelineInterval = 10 * time.Second
	idleInterval     = time.Minute
)

// How long a finished push stays on the page: long enough to see it land, short
// enough that the strip is empty on an ordinary day.
const (
	showLiveFor   = 10 * time.Minute
	showFailedFor = 2 * time.Hour
)

type ghRun struct {
	ID         int64  `json:"id"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`     // queued / in_progress / completed
	Conclusion string `json:"conclusion"` // success / failure / cancelled / …
	Event      string `json:"event"`      // push / workflow_dispatch / …
	HTMLURL    string `json:"html_url"`
	StartedAt  string `json:"run_started_at"`
	UpdatedAt  string `json:"updated_at"`
	HeadCommit struct {
		Message string `json:"message"`
	} `json:"head_commit"`
}

type repoBuilds struct {
	etag string
	runs []ghRun // newest first
	step string  // the running step of runs[0], while it runs
	// buildStarted is when runs[0]'s build job started, while it runs (0 while
	// it still waits for a runner).
	buildStarted int64
	// jobSec is how long each finished run's build job took, by run id. A run's
	// own start-to-end also counts the wait for a free runner, which is long when
	// many services are pushed together and says nothing about the next build.
	jobSec map[int64]float64
	// jobStart is when each of those jobs started, so the wait for a runner
	// (job start minus run start) can be recorded too.
	jobStart map[int64]int64
}

var (
	buildsMu sync.Mutex
	builds   = map[string]*repoBuilds{} // repo -> what GitHub last said
)

var nudges = make(chan string, 32)

// NudgeBuilds asks for a repo's builds right away, because GitHub just said
// something about them changed. It never blocks the webhook.
func NudgeBuilds(repo string) {
	select {
	case nudges <- repo:
	default:
	}
}

// PollBuilds keeps the build cache fresh for every enabled service.
func PollBuilds() {
	refreshAll := func() {
		for _, s := range Get().Services {
			if s.Enabled {
				refreshBuilds(s.Repo)
			}
		}
	}
	refreshAll()
	last := time.Now()
	tick := time.NewTicker(pipelineInterval)
	for {
		select {
		case repo := <-nudges:
			refreshBuilds(repo)
		case <-tick.C:
			if anyBuilding() || time.Since(last) >= idleInterval {
				refreshAll()
				last = time.Now()
			}
		}
		recordFinished()
	}
}

func anyBuilding() bool {
	buildsMu.Lock()
	defer buildsMu.Unlock()
	for _, rb := range builds {
		if len(rb.runs) > 0 && rb.runs[0].Status != "completed" {
			return true
		}
	}
	return false
}

func refreshBuilds(repo string) {
	buildsMu.Lock()
	rb := builds[repo]
	if rb == nil {
		rb = &repoBuilds{}
		builds[repo] = rb
	}
	etag := rb.etag
	buildsMu.Unlock()

	var body struct {
		Runs []ghRun `json:"workflow_runs"`
	}
	newEtag, changed, err := ghGet(fmt.Sprintf("https://api.github.com/repos/%s/actions/runs?branch=master&per_page=6", repo), etag, &body)
	if err != nil {
		log.Printf("deployer: cannot read builds for %s: %v", repo, err)
		return
	}
	step, started := "", int64(0)
	buildsMu.Lock()
	runs := body.Runs
	if !changed {
		runs = rb.runs
	}
	known, knownStart := rb.jobSec, rb.jobStart
	buildsMu.Unlock()
	if len(runs) > 0 && runs[0].Status != "completed" {
		step, started = currentStep(repo, runs[0].ID)
	}
	// A finished run's job time never changes, so each run is asked about once.
	jobSec, jobStart := map[int64]float64{}, map[int64]int64{}
	for _, r := range runs {
		if r.Status != "completed" || r.Conclusion != "success" {
			continue
		}
		if d, ok := known[r.ID]; ok {
			jobSec[r.ID], jobStart[r.ID] = d, knownStart[r.ID]
		} else if st, d := jobSeconds(repo, r.ID); d > 0 {
			jobSec[r.ID], jobStart[r.ID] = d, st
		}
	}

	buildsMu.Lock()
	defer buildsMu.Unlock()
	if changed {
		rb.etag, rb.runs = newEtag, runs
	}
	rb.step, rb.buildStarted, rb.jobSec, rb.jobStart = step, started, jobSec, jobStart
}

type ghJobs struct {
	Jobs []struct {
		Status      string `json:"status"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
		Steps       []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"steps"`
	} `json:"jobs"`
}

func runJobs(repo string, run int64) (ghJobs, bool) {
	var body ghJobs
	_, _, err := ghGet(fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d/jobs", repo, run), "", &body)
	return body, err == nil
}

// jobSeconds is when a finished run's jobs started and how long they ran, first
// start to last end, or 0 if GitHub cannot say.
func jobSeconds(repo string, run int64) (int64, float64) {
	body, ok := runJobs(repo, run)
	if !ok {
		return 0, 0
	}
	var first, last int64
	for _, j := range body.Jobs {
		s, e := ghTime(j.StartedAt), ghTime(j.CompletedAt)
		if s == 0 || e < s {
			return 0, 0
		}
		if first == 0 || s < first {
			first = s
		}
		if e > last {
			last = e
		}
	}
	return first, float64(last - first)
}

// currentStep names the step a running build is on, so "building" can say what
// it is doing — the Release workflows spend most of their time in one or two —
// and when its job started (0 while it still waits for a runner).
func currentStep(repo string, run int64) (string, int64) {
	body, ok := runJobs(repo, run)
	if !ok {
		return "", 0
	}
	var started int64
	for _, j := range body.Jobs {
		if j.Status != "queued" && j.Status != "waiting" {
			if t := ghTime(j.StartedAt); t > 0 && (started == 0 || t < started) {
				started = t
			}
		}
		for _, st := range j.Steps {
			if st.Status == "in_progress" {
				return strings.TrimPrefix(st.Name, "Run "), started
			}
		}
	}
	return "", started
}

// ghGet fetches url into out. With an etag it asks only for changes, and
// reports changed=false (out untouched) when there are none.
func ghGet(url, etag string, out any) (string, bool, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := Get().GitHubToken; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := ghClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return etag, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("github: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", false, err
	}
	return resp.Header.Get("ETag"), true, nil
}

func ghTime(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// deployState is what the deployer knows about one service, gathered once so
// pipelineFor can be tested without a VPS.
type deployState struct {
	tag        string  // running now
	liveAt     int64   // when that went in
	deploying  string  // tag being installed, if any
	since      int64   // when that install started
	failedTag  string  // newest tag that failed to deploy
	failedPerm bool    // and will not be retried on its own
	deploySec  float64 // how long the last successful deploy took
}

func stateOf(s Service) deployState {
	st := deployState{tag: deployedTag(s), deploySec: lastDeploySec(s)}
	if t, err := time.Parse(time.RFC3339, deployedAt(s)); err == nil {
		st.liveAt = t.Unix()
	}
	if d, ok := Deploying(s.Name); ok {
		st.deploying, st.since = d.Tag, d.Since.Unix()
	}
	if rec, ok := readFailure(s); ok {
		st.failedTag, st.failedPerm = rec.Tag, rec.permanent()
	}
	return st
}

// pipelineFor turns a service's newest build and its deploy state into one
// Pipeline. ok is false when there is nothing worth showing: no builds, or a
// push that went live or failed long enough ago to be old news.
func pipelineFor(name string, rb repoBuilds, ds deployState, now int64) (Pipeline, bool) {
	if len(rb.runs) == 0 {
		return Pipeline{}, false
	}
	r := rb.runs[0]
	sha := short(r.HeadSHA)
	title, _, _ := strings.Cut(r.HeadCommit.Message, "\n")
	p := Pipeline{
		Name: name, SHA: sha, Title: title, URL: r.HTMLURL,
		Started: ghTime(r.StartedAt), TypicalBuildSec: typicalBuild(rb.runs, rb.jobSec),
		TypicalDeploySec: ds.deploySec,
	}
	// Releases are tagged v<n>-<short sha>, which is how a build is matched to
	// the release it made and the deploy that installed it.
	ours := func(tag string) bool { return tag != "" && strings.HasSuffix(tag, "-"+sha) }

	switch {
	case r.Status == "queued" || r.Status == "waiting" || r.Status == "pending":
		p.Stage = "queued"
	case r.Status != "completed":
		p.Stage, p.Step, p.BuildStarted = "building", rb.step, rb.buildStarted
		if strings.Contains(strings.ToLower(rb.step), "release") {
			p.Stage = "releasing"
		}
	case r.Conclusion != "success":
		p.Stage, p.Step = "build_failed", r.Conclusion
		p.BuiltAt = ghTime(r.UpdatedAt)
		if now-p.BuiltAt > int64(showFailedFor.Seconds()) {
			return p, false
		}
	default:
		p.BuiltAt = ghTime(r.UpdatedAt)
		switch {
		case ours(ds.tag):
			p.Stage, p.Tag, p.LiveAt = "live", ds.tag, ds.liveAt
			if now-p.LiveAt > int64(showLiveFor.Seconds()) {
				return p, false
			}
		case ours(ds.deploying):
			p.Stage, p.Tag, p.DeploySince = "deploying", ds.deploying, ds.since
		case ours(ds.failedTag):
			p.Stage, p.Tag, p.Retrying = "deploy_failed", ds.failedTag, !ds.failedPerm
			if ds.failedPerm && now-p.BuiltAt > int64(showFailedFor.Seconds()) {
				return p, false
			}
		default:
			// Released, but the deployer has not started on it: the webhook is a
			// moment away, or another service's deploy holds the fleet lock.
			p.Stage = "waiting"
			if now-p.BuiltAt > int64(showFailedFor.Seconds()) {
				return p, false // a service that is off, or never picked it up
			}
		}
	}
	return p, true
}

// typicalBuild is the median length of the recent successful builds: their job
// time where known, else the run's own start to end.
func typicalBuild(runs []ghRun, jobSec map[int64]float64) float64 {
	var d []float64
	for _, r := range runs {
		if r.Status == "completed" && r.Conclusion == "success" {
			if j := jobSec[r.ID]; j > 0 {
				d = append(d, j)
			} else if s, e := ghTime(r.StartedAt), ghTime(r.UpdatedAt); s > 0 && e >= s {
				d = append(d, float64(e-s))
			}
		}
	}
	if len(d) == 0 {
		return 0
	}
	sort.Float64s(d)
	return d[len(d)/2]
}

// Pipelines lists every push still on its way, plus ones that just landed or
// failed, newest first.
func Pipelines() []Pipeline {
	now := time.Now().Unix()
	out := []Pipeline{}
	for _, s := range Get().Services {
		if !s.Enabled {
			continue
		}
		ds := stateOf(s)
		buildsMu.Lock()
		rb, ok := builds[s.Repo]
		var copyRB repoBuilds
		if ok {
			copyRB = *rb
		}
		buildsMu.Unlock()
		if p, show := pipelineFor(s.Name, copyRB, ds, now); show {
			// The recorded pushes are the estimate wherever there are any; GitHub's
			// job times and the last deploy only fill in until there are.
			if b, d := Estimates(s.Name); b > 0 || d > 0 {
				if b > 0 {
					p.TypicalBuildSec = b
				}
				if d > 0 && p.Stage != "live" {
					p.TypicalDeploySec = d
				}
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// HandlePipelines serves Pipelines to the Overview.
func HandlePipelines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(Pipelines())
}
