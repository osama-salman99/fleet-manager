package deployer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HandleConfig serves GET (read) and PUT (replace) of the deployer config.
func HandleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Get())

	case http.MethodPut:
		// Defense-in-depth: a PUT with a JSON body already forces a CORS preflight
		// (which has no allow headers here, so a cross-origin browser call is blocked),
		// but guard it the same way as redeploy so config writes never depend solely on
		// that, and a future method/content-type change can't silently reopen it.
		if !sameOriginOnly(w, r) {
			return
		}
		var c Config
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := Put(c); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("deployer: config updated (%d services)", len(c.Services))
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandleStatus reports, for each service, everything the config page shows:
// what is deployed, whether it is actually up, and when it last changed.
func HandleStatus(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Name       string `json:"name"`
		Repo       string `json:"repo"`
		Tag        string `json:"tag"`
		Enabled    bool   `json:"enabled"`
		State      string `json:"state"`            // active / inactive / n/a
		Healthy    bool   `json:"healthy"`          // HTTP check passed
		HasHealth  bool   `json:"has_health"`       // is there a URL to check at all
		DeployedAt string `json:"deployed_at"`      // RFC3339, empty if never
		Latest     string `json:"latest"`           // newest published release tag
		UpToDate   bool   `json:"up_to_date"`       // running the newest release
		LatestErr  string `json:"latest_err"`       // why we cannot tell, if we cannot
		Deploying  bool   `json:"deploying"`        // being installed right now
		DeployTag  string `json:"deploy_tag"`       // the version going in
		Failed     bool   `json:"failed"`           // newest release's initial deploy failed (may still be auto-retrying)
		FailedTag  string `json:"failed_tag"`       // the tag that failed
		FailedPerm bool   `json:"failed_permanent"` // auto-retries spent; only a manual Retry will move it
		FailedTry  int    `json:"failed_attempts"`  // how many full deploy cycles have failed
	}

	services := Get().Services
	out := make([]row, len(services))

	// One systemctl call for every unit's state, up front — never one fork per
	// service inside the goroutine fan-out below (see unitStates: the fork-storm
	// that OOM-kills children under the cgroup and misreads as an outage).
	units := make([]string, 0, len(services))
	for _, s := range services {
		if s.Unit != "" {
			units = append(units, s.Unit)
		}
	}
	states := unitStates(units)

	// Health checks and release lookups are network calls; run them concurrently
	// so the page is snappy.
	var wg sync.WaitGroup
	for i, s := range services {
		wg.Add(1)
		go func(i int, s Service) {
			defer wg.Done()
			tag := deployedTag(s)
			rel := latestFor(s.Repo)
			// A recorded failure only matters while the newest release is the one that
			// failed and it is not what is running: that is the state a human can act on
			// with Retry. An older failed tag that a newer release has since superseded
			// is stale noise. `failed` covers a release that is still auto-retrying too;
			// `failed_permanent` is the subset where the auto-retries are spent.
			rec, hasFail := readFailure(s)
			onLatest := hasFail && rec.Tag == rel.Tag && rec.Tag != tag
			out[i] = row{
				Name:       s.Name,
				Repo:       s.Repo,
				Tag:        tag,
				Enabled:    s.Enabled,
				State:      stateFromMap(s, states),
				HasHealth:  s.HealthURL != "",
				DeployedAt: deployedAt(s),
				Latest:     rel.Tag,
				UpToDate:   tag != "" && rel.Tag != "" && tag == rel.Tag,
				LatestErr:  rel.Err,
				FailedTag:  rec.Tag,
				Failed:     onLatest,
				FailedPerm: onLatest && rec.permanent(),
				FailedTry:  rec.Attempts,
			}
			if s.HealthURL != "" {
				out[i].Healthy = httpOK(s.HealthURL)
			}
			// Last, so it also covers a deploy that started while we were probing:
			// the state and health above would then be readings of a unit being
			// restarted underneath us, not of a service that is actually broken.
			if d, ok := Deploying(s.Name); ok {
				out[i].Deploying = true
				out[i].DeployTag = d.Tag
			}
		}(i, s)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// HandleRedeploy forces a service to the latest release, ignoring the recorded tag.
func HandleRedeploy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// POST target is in the URL path and the body is ignored, so this is a "simple"
	// request that needs no CORS preflight — the SSO cookie's SameSite=Lax does not
	// protect it against a same-site page (home.osmosis.work). Refuse cross-origin.
	if !sameOriginOnly(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/redeploy/")
	for _, s := range Get().Services {
		if s.Name == name {
			go func(s Service) {
				// Pressing this is a human saying "try it again", so it clears the
				// block the poller respects on a release that failed before.
				clearFailure(s)
				if err := Deploy(s, ""); err != nil {
					log.Printf("deployer: manual redeploy of %s failed: %v", s.Name, err)
				}
			}(s)
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprintf(w, "redeploying %s", name)
			return
		}
	}
	http.Error(w, "unknown service", http.StatusNotFound)
}

// HandleDecommission stops a service, deletes the build the deployer installed
// and switches it off. Unlike redeploy this runs synchronously: it is the kind of
// action a human wants a straight answer to, and the reply lists what was done.
func HandleDecommission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Same reasoning as redeploy, and more so — this one destroys files.
	if !sameOriginOnly(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/decommission/")
	steps, err := Decommission(name)
	if err != nil {
		log.Printf("deployer: decommission of %s failed: %v", name, err)
		status := http.StatusInternalServerError
		if strings.HasPrefix(err.Error(), "unknown service") {
			status = http.StatusNotFound
		}
		// Steps can be non-empty on failure (stopped, but the config write lost) —
		// send them anyway so the page can say how far it got.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(struct {
			Error string   `json:"error"`
			Steps []string `json:"steps,omitempty"`
		}{err.Error(), steps})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Name  string   `json:"name"`
		Steps []string `json:"steps"`
	}{name, steps})
}

// releasePayload is the slice of the GitHub webhook body we care about.
type releasePayload struct {
	Action  string `json:"action"`
	Release struct {
		TagName string `json:"tag_name"`
	} `json:"release"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// HandleWebhook receives GitHub `release` events and deploys the new tag. Push and
// build events only wake the build poller, so the Overview shows a push at once.
func HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 25 MiB — GitHub's max webhook payload. The HMAC is computed over this exact body, so a
	// cap below the real payload size truncates it and the signature never matches (a release
	// with large auto-generated notes was silently 401'd, deploying only via the 10-min poll).
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	secret := Get().WebhookSecret
	if secret == "" {
		http.Error(w, "webhook secret not configured", http.StatusServiceUnavailable)
		return
	}
	if !validSignature(body, r.Header.Get("X-Hub-Signature-256"), secret) {
		log.Printf("deployer: webhook rejected (bad signature) from %s", r.RemoteAddr)
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	// GitHub pings the endpoint once when the webhook is created.
	if r.Header.Get("X-GitHub-Event") == "ping" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "pong")
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "push", "workflow_run", "workflow_job":
		var p struct {
			Ref        string `json:"ref"`
			Deleted    bool   `json:"deleted"`
			Action     string `json:"action"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			WorkflowJob struct {
				Labels []string `json:"labels"`
			} `json:"workflow_job"`
		}
		if json.Unmarshal(body, &p) == nil && r.Header.Get("X-GitHub-Event") == "workflow_job" {
			wakeBuildRunner(p.Action, p.Repository.FullName, p.WorkflowJob.Labels)
		}
		if json.Unmarshal(body, &p) == nil && (p.Ref == "" || p.Ref == "refs/heads/master") {
			if svc, ok := findByRepo(p.Repository.FullName); ok && svc.Enabled {
				if r.Header.Get("X-GitHub-Event") == "push" && p.Ref == "refs/heads/master" && !p.Deleted {
					wakeBuildRunnerOnPush(p.Repository.FullName)
				}
				NudgeBuilds(svc.Repo)
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("X-GitHub-Event") != "release" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var p releasePayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if p.Action != "published" {
		w.WriteHeader(http.StatusOK)
		return
	}

	svc, ok := findByRepo(p.Repository.FullName)
	if !ok {
		log.Printf("deployer: webhook for unknown repo %s", p.Repository.FullName)
		http.Error(w, "unknown repo", http.StatusNotFound)
		return
	}
	if !svc.Enabled {
		log.Printf("deployer: %s is disabled, ignoring release %s", svc.Name, p.Release.TagName)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Answer GitHub immediately; deploying can take a while (and may restart us).
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "deploying %s %s", svc.Name, p.Release.TagName)

	go func(s Service, tag string) {
		if err := Deploy(s, tag); err != nil {
			log.Printf("deployer: %s %s failed: %v", s.Name, tag, err)
		}
	}(svc, p.Release.TagName)
}

func validSignature(body []byte, header, secret string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// ---------- public fleet status ----------
//
// A deliberately minimal, unauthenticated, CORS-enabled snapshot for the
// osmosis.work launcher: per public-facing service, a stable id, its URL, and
// whether it is up right now. No repo/tag/deploy internals leave the building,
// and services without a PublicURL (internal-only units) are omitted entirely.
// Disabled services (enabled:false) are omitted too — a unit that was switched
// off on purpose is not "down", so it must not appear on the launcher or count
// against it.

const fleetOrigin = "https://osmosis.work"

var (
	fleetMu   sync.Mutex
	fleetBody []byte
	fleetAt   time.Time
)

// HandleFleet answers the launcher's health poll. It is intentionally NOT wrapped
// in RequireAuth in main.go — one of the two public data routes (the other is
// HandleServices).
func HandleFleet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", fleetOrigin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=15")

	// Probing every service on every hit would let a refresh loop spawn a burst of
	// health checks; a short memo keeps it to one sweep per ~10s no matter the traffic.
	fleetMu.Lock()
	body, age := fleetBody, time.Since(fleetAt)
	fleetMu.Unlock()
	if body == nil || age > 10*time.Second {
		body = buildFleet()
		fleetMu.Lock()
		fleetBody, fleetAt = body, time.Now()
		fleetMu.Unlock()
	}
	w.Write(body)
}

func buildFleet() []byte {
	type svc struct {
		ID  string `json:"id"`
		URL string `json:"url"`
		Up  bool   `json:"up"`
	}

	var pub []Service
	for _, s := range Get().Services {
		if s.PublicURL != "" && s.Enabled {
			pub = append(pub, s)
		}
	}

	units := make([]string, 0, len(pub))
	for _, s := range pub {
		if s.Unit != "" {
			units = append(units, s.Unit)
		}
	}
	states := unitStates(units)

	list := make([]svc, len(pub))
	var wg sync.WaitGroup
	for i, s := range pub {
		wg.Add(1)
		go func(i int, s Service) {
			defer wg.Done()
			// Judge on what there is to judge: a systemd unit must be active, a
			// health URL must answer. A pure static site (no unit, no health — e.g.
			// osmosis-index, served straight from Caddy) has nothing to fail, so it
			// counts as up rather than being falsely reported down.
			up := true
			if s.Unit != "" {
				up = stateFromMap(s, states) == "active"
			}
			if up && s.HealthURL != "" {
				up = httpOK(s.HealthURL)
			}
			list[i] = svc{ID: s.Name, URL: s.PublicURL, Up: up}
		}(i, s)
	}
	wg.Wait()

	upN := 0
	for _, x := range list {
		if x.Up {
			upN++
		}
	}
	b, _ := json.Marshal(struct {
		TS       int64 `json:"ts"`
		Up       int   `json:"up"`
		Down     int   `json:"down"`
		Services []svc `json:"services"`
	}{time.Now().Unix(), upN, len(list) - upN, list})
	return b
}

// HandleServices reports which public-facing services are switched on. Unlike
// HandleFleet it does NO health probing and hides nothing — it lists every
// service that has a PublicURL together with its enabled/disabled state, so a
// consumer can tell "intentionally off" apart from "down" (HandleFleet already
// omits the disabled ones, so on its own it cannot make that distinction).
// Public and CORS-enabled, same as the fleet route; no deploy internals leak.
func HandleServices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", fleetOrigin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=15")

	type svc struct {
		ID      string `json:"id"`
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
	}
	var list []svc
	for _, s := range Get().Services {
		if s.PublicURL == "" {
			continue
		}
		list = append(list, svc{ID: s.Name, URL: s.PublicURL, Enabled: s.Enabled})
	}
	b, _ := json.Marshal(struct {
		TS       int64 `json:"ts"`
		Services []svc `json:"services"`
	}{time.Now().Unix(), list})
	w.Write(b)
}
