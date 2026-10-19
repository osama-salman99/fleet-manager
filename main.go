package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/osama-salman99/fleet-manager/canary"
	"github.com/osama-salman99/fleet-manager/collector"
	"github.com/osama-salman99/fleet-manager/deployer"
	"github.com/osama-salman99/fleet-manager/otel"
	"github.com/osama-salman99/fleet-manager/watchdog"
)

//go:embed static
var staticFS embed.FS

// Scripts every page loads from <head>: client-side navigation, and the guarded
// DOM writers each page uses to update itself in place. Both get a route and a
// per-deploy cache-busting query.
var sharedScripts = []string{"spa.js", "dom.js", "nav.js"}

// Build version, set at link time via -ldflags "-X main.version=v<run>".
// Served uncached at /version and baked into every HTML page's bottom-left
// badge, so a stale cached tab shows red against the running build.
var version = "dev"

// versionBadge is injected before </body> of every served HTML page. The
// version literal rides in the (tab-cached) HTML; the script compares it to the
// uncached /version and goes red when this tab is behind the running build.
func versionBadge() []byte {
	const tmpl = `<style>#osmosis-ver{position:fixed;left:0;bottom:0;z-index:2147483000;padding:2px 5px;border-top-right-radius:4px;background:#fff;border-top:1px solid rgba(0,0,0,.12);border-right:1px solid rgba(0,0,0,.12);box-shadow:0 -1px 3px rgba(0,0,0,.06);font:600 10px/1.2 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;letter-spacing:.02em;color:#1a7f37;user-select:none;pointer-events:none}#osmosis-ver.stale{color:#d1242f}#osmosis-ver{background:#fff;border-top-color:rgba(0,0,0,.12);border-right-color:rgba(0,0,0,.12);box-shadow:0 -1px 3px rgba(0,0,0,.06);color:#1a7f37}#osmosis-ver.stale{color:#d1242f}@media (prefers-color-scheme:dark){#osmosis-ver{background:#161B22;border-top-color:rgba(255,255,255,.14);border-right-color:rgba(255,255,255,.14);box-shadow:none;color:#3fb950}#osmosis-ver.stale{color:#f87171}}@media (display-mode:standalone),(hover:hover) and (pointer:fine){#osmosis-ver{--vb:rgba(255,255,255,.9);--vl:rgba(0,0,0,.12);left:24px;bottom:24px;margin:0;padding:0;background:none;border:0;border-radius:0;box-shadow:none;opacity:1;font-size:11px;line-height:1;color:#1a7f37;transform:translate(-50%%,50%%) rotate(45deg)}#osmosis-ver.stale{color:#cf222e}#osmosis-ver::before{content:"";position:absolute;z-index:-1;left:50%%;top:50%%;width:150px;height:150px;margin:-129px 0 0 -75px;border-radius:50%%;background:radial-gradient(circle,transparent 45.5px,var(--vl) 46.5px,var(--vb) 47.5px);-webkit-mask:conic-gradient(transparent 128deg,#000 150deg 210deg,transparent 232deg);mask:conic-gradient(transparent 128deg,#000 150deg 210deg,transparent 232deg)}@media (prefers-color-scheme:dark){#osmosis-ver{--vb:rgba(22,27,34,.92);--vl:rgba(255,255,255,.16);color:#3fb950}#osmosis-ver.stale{color:#f87171}}}@media (hover:hover) and (pointer:fine){#osmosis-ver{left:14px;bottom:14px}}</style><div id="osmosis-ver">%s</div><script src="https://osmosis.work/osmosis-badge.js"></script>`
	return []byte(fmt.Sprintf(tmpl, version))
}

// withBadge inserts the version badge just before the last </body> in an HTML
// document. If there is no </body> it returns the document unchanged.
func withBadge(data []byte) []byte {
	// Cache-bust the shared scripts per deploy: they are edge-cached for hours (a
	// proxy rule overrides their no-cache), so without a version query a fix to one
	// would not reach browsers. The HTML itself is no-cache, so a fresh page always
	// points at the current version.
	if version != "dev" {
		for _, name := range sharedScripts {
			data = bytes.Replace(data, []byte(`src="/`+name+`"`), []byte(`src="/`+name+`?v=`+version+`"`), 1)
		}
	}
	i := bytes.LastIndex(data, []byte("</body>"))
	if i < 0 {
		return data
	}
	out := make([]byte, 0, len(data)+220)
	out = append(out, data[:i]...)
	out = append(out, versionBadge()...)
	out = append(out, data[i:]...)
	return out
}

func main() {
	// Load first: the collector reads the deployer's service list, so an empty
	// config here would mean a dashboard with no services on it.
	deployer.Load()
	// If the self-check had to restore this binary after a bad release, record it.
	deployer.CheckSelfRollback()

	// The build-failure watchdog is a systemd unit, so the config toggle only
	// means anything if something reconciles it — at boot, and on every save.
	deployer.OnSave = func(c deployer.Config) { watchdog.SetBuildWatchdog(c.WatchdogEnabled) }
	go watchdog.SetBuildWatchdog(deployer.Get().WatchdogEnabled)

	go collector.Run()
	go watchdog.Run()
	go deployer.Poll()
	go deployer.PollBuilds()

	// Ingest Claude Code's own OpenTelemetry: the sessions run inside the Coder
	// container and export to the host across the docker bridge, so this listener
	// sits on the bridge gateway (ufw only lets the docker subnet reach the port),
	// separate from the loopback-only dashboard.
	telemetry := otel.NewStore()
	go startOTLP(telemetry)

	// Track the account's real rate-limit quota (utilization + reset per window)
	// with the current OAuth token, read out of the Coder container read-only:
	// once a minute from the free usage address /usage reads, and every 5 minutes
	// from a 1-token request that harvests the anthropic-ratelimit-unified-*
	// headers (the warning labels, and the fallback when the usage read fails).
	// Read-only on purpose: refreshing would rotate the token and log the running
	// Claude Code out.
	container := os.Getenv("CODER_CONTAINER")
	if container == "" {
		container = "coder-osmosis-osmosis-dot-work"
	}
	go telemetry.RunLimits(container, time.Minute, 5*time.Minute)

	// Ask the session controller what each session is called, so the Claude page
	// can list "Health" rather than a piece of a random id.
	go telemetry.RunSessionNames(time.Minute)

	// Watch the portal APIs' published OpenAPI specs and page on a contract change
	// (e.g. a newly-required parameter) before it breaks a live application.
	go canary.Run()

	mux := http.NewServeMux()
	// The whole dashboard is gated now (the catch-all "/" and the live feed).
	// There is no local login page; the public routes are the static assets,
	// /version, /spa.js, /logout, the HMAC-authed GitHub webhook, and the two
	// CORS launcher data routes /api/fleet and /api/services.
	mux.HandleFunc("/", deployer.RequireAuth(serveIndex))
	// Public + uncached: the frontend badge polls this to detect a stale tab.
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "{\"version\":%q}", version)
	})
	mux.HandleFunc("/favicon.svg", serveFavicon)
	mux.HandleFunc("/favicon.png", serveStaticFile("static/favicon.png", "image/png"))
	mux.HandleFunc("/apple-touch-icon.png", serveStaticFile("static/apple-touch-icon.png", "image/png"))
	// The home-screen app: its manifest and icons are fetched without the login cookie when the
	// page is added to a phone's home screen, so they are public like the favicons.
	mux.HandleFunc("/manifest.json", serveStaticFile("static/manifest.json", "application/manifest+json"))
	for _, name := range []string{"icon-192.png", "icon-512.png", "icon-maskable-512.png"} {
		mux.HandleFunc("/"+name, serveStaticFile("static/"+name, "image/png"))
	}
	mux.HandleFunc("/icons/", serveIcon)
	// Client-side navigation, the DOM helpers every page patches itself with, and
	// the draggable tab pill. All are shared by every page, hold no data (so no auth) and are served
	// no-cache so a deploy's version is picked up on the next load.
	for _, name := range sharedScripts {
		path := "static/" + name
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			data, err := staticFS.ReadFile(path)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Write(data)
		})
	}
	mux.HandleFunc("/events", deployer.RequireAuth(serveSSE))

	// GitHub calls this; it authenticates with an HMAC signature, not a session.
	mux.HandleFunc("/hooks/github", deployer.HandleWebhook)

	// No local login — /logout clears the shared cookie and returns to auth.osmosis.work.
	mux.HandleFunc("/logout", deployer.HandleLogout)

	// The Claude Code telemetry page and its data feed — same SSO wall as the rest.
	mux.HandleFunc("/claude", deployer.RequireAuth(serveStaticFile("static/claude.html", "text/html; charset=utf-8")))
	mux.HandleFunc("/api/claude", deployer.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(telemetry.Snapshot())
	}))

	// Just the account quota, for the strip on the Overview page — the whole
	// /api/claude snapshot is far more than a two-bar strip needs once a minute.
	mux.HandleFunc("/api/limits", deployer.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(telemetry.Quota())
	}))

	// Config tab and its API — require a signed-in session.
	// Hetzner tab: every offer the deal watcher saw on its last hourly check, as a
	// 3D chart. The watcher (outside this repo) writes the file; this only serves it.
	mux.HandleFunc("/hetzner", deployer.RequireAuth(serveStaticFile("static/hetzner.html", "text/html; charset=utf-8")))
	mux.HandleFunc("/api/hetzner", deployer.RequireAuth(serveHetznerOffers))

	// Builds tab: every recorded push, from commit to live.
	mux.HandleFunc("/builds", deployer.RequireAuth(serveStaticFile("static/builds.html", "text/html; charset=utf-8")))

	mux.HandleFunc("/config", deployer.RequireAuth(serveStaticFile("static/config.html", "text/html; charset=utf-8")))
	mux.HandleFunc("/api/config", deployer.RequireAuth(deployer.HandleConfig))
	mux.HandleFunc("/api/status", deployer.RequireAuth(deployer.HandleStatus))
	mux.HandleFunc("/api/pipelines", deployer.RequireAuth(deployer.HandlePipelines))
	mux.HandleFunc("/api/deploy-metrics", deployer.RequireAuth(deployer.HandleDeployMetrics))
	mux.HandleFunc("/api/redeploy/", deployer.RequireAuth(deployer.HandleRedeploy))
	mux.HandleFunc("/api/decommission/", deployer.RequireAuth(deployer.HandleDecommission))

	// Public, CORS-enabled fleet health for the osmosis.work launcher — one of the
	// two data routes NOT behind the SSO wall (the other is /api/services below;
	// minimal: id/url/up only).
	mux.HandleFunc("/api/fleet", deployer.HandleFleet)

	// Public, CORS-enabled enabled/disabled state per service — lets a consumer
	// tell "switched off on purpose" apart from "down" (fleet omits disabled).
	mux.HandleFunc("/api/services", deployer.HandleServices)

	// The QA-reports dashboard (/cleanup, /cleanup/report, /api/qa-reports) was
	// retired 2026-08-09 along with the QA nav tab: the agent that wrote those
	// shift reports was retired 2026-08-04, so the page only ever showed a frozen
	// archive. The reports themselves still live in the qa-agent repo's REPORTS/.

	// Bind loopback only: Caddy reverse-proxies health.osmosis.work to
	// 127.0.0.1:8200 and the GitHub webhook arrives through Caddy too, so the
	// service never needs a public interface. ufw already blocks the port; this
	// makes the bind match that intent.
	log.Println("vps-health listening on 127.0.0.1:8200")
	log.Fatal(http.ListenAndServe("127.0.0.1:8200", mux))
}

// startOTLP serves the OTLP/HTTP receiver on the docker bridge gateway so
// sessions inside the workspace container can reach it; ufw restricts the port
// to the docker subnet. A bind failure is logged, not fatal — the dashboard
// must keep serving even if telemetry ingest cannot come up.
func startOTLP(store *otel.Store) {
	const addr = "172.17.0.1:4318"
	log.Printf("otel ingest listening on %s", addr)
	// Explicit timeouts: this listener is reachable by everything in the workspace
	// container (docker-bridge gateway), so a stuck/slow OTLP client (or a Slowloris
	// from inside the container) must not hold connections + goroutines open forever on
	// a memory-tight host.
	srv := &http.Server{
		Addr:              addr,
		Handler:           store.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("otel ingest stopped: %v", err)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		serve404(w)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Always revalidate: the SPA fetches these pages on navigation, and a stale
	// cached copy would swap in an old layout after a deploy.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(withBadge(data))
}

func serve404(w http.ResponseWriter) {
	data, err := staticFS.ReadFile("static/404.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write(withBadge(data))
}

func serveFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/favicon.svg")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write(data)
}

// serveIcon hands out a service's icon. The icons are embedded rather than
// fetched from each service, because the dashboard has to draw a service most
// clearly at the moment that service is down — which is exactly when its own
// favicon would fail to load.
func serveIcon(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/icons/")
	// The name comes off the wire, so it does not get to pick a path.
	if name == "" || !iconName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/icons/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

var iconName = regexp.MustCompile(`^[a-z0-9-]+\.svg$`)

func serveStaticFile(path, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile(path)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", contentType)
		if strings.HasPrefix(contentType, "text/html") {
			data = withBadge(data)
			// Revalidate HTML on every load so the SPA never swaps in a stale page.
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.Write(data)
	}
}

func serveSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	send := func() {
		snap, ok := collector.Latest()
		if !ok {
			return
		}
		data, err := json.Marshal(snap)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Send the current reading immediately on connect, then once a second. Waiting
	// a full tick first left the dashboard showing "Waiting for the first reading…"
	// for up to a second on every (re)connect — very visible now that switching to
	// Overview reconnects the stream.
	send()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// hetznerOffersFile is written by hetzner-watch (/opt/hetzner-watch/check.py).
const hetznerOffersFile = "/var/lib/hetzner-watch/offers.json"

func serveHetznerOffers(w http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(hetznerOffersFile)
	if err != nil {
		data = []byte(`{"offers":[]}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}
