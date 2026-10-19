package deployer

import (
	"net/http"
	"net/url"
	"os"
)

// The shared SSO cookie signing key (random, from /root/.secrets/admin-auth.env). This service no
// longer checks a password or keeps its own session — login lives at auth.osmosis.work; here we only
// validate the shared osmosis_sso cookie (see sso.go) and bounce unauthenticated browsers there.
var ssoKey = os.Getenv("SSO_SIGNING_KEY")

// authURL is the central sign-on redirect, carrying ?next back to the current page.
func authURL(r *http.Request) string {
	scheme := "https"
	if !isTLS(r) {
		scheme = "http"
	}
	next := scheme + "://" + r.Host + r.URL.RequestURI()
	return "https://auth.osmosis.work/?next=" + url.QueryEscape(next)
}

func authenticated(r *http.Request) bool {
	return ssoAuthed(r)
}

// isTLS reports whether the original client request was HTTPS. Caddy terminates TLS and forwards
// over plain HTTP, so trust its header.
func isTLS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// RequireAuth gates a handler behind a valid shared cookie. Browsers get redirected to the central
// login; API calls get a clean 401 instead of an HTML redirect.
// sameOriginOnly refuses a browser cross-context request to a state-mutating route
// and reports whether the request may proceed. The SSO cookie is SameSite=Lax, which
// blocks true third-party requests but NOT same-site ones — and home.osmosis.work is
// intentionally public and same-site with the admin surfaces, so a script or an XSS on
// any *.osmosis.work page could fire a request here with the Lax cookie attached. A
// "simple" POST whose target rides in the URL path (e.g. /api/redeploy/<svc>, body
// ignored) triggers no CORS preflight, so Lax alone does not protect it. The config
// page's own fetches are Sec-Fetch-Site: same-origin and non-browser callers send no
// such header — both still pass; only a browser same-site/cross-site request is refused.
// (Applied to mutating routes only, never to GET: same-site top-level navigations —
// clicking through to health.osmosis.work from the launcher — are legitimate.)
func sameOriginOnly(w http.ResponseWriter, r *http.Request) bool {
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return false
	}
	return true
}

func RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authenticated(r) {
			next(w, r)
			return
		}
		if isAPI(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, authURL(r), http.StatusSeeOther)
	}
}

func isAPI(r *http.Request) bool {
	return len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/"
}

// HandleLogout clears the shared cookie (global logout) and returns to the central login.
func HandleLogout(w http.ResponseWriter, r *http.Request) {
	clearSSO(w, r)
	http.Redirect(w, r, "https://auth.osmosis.work/", http.StatusSeeOther)
}
