package deployer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Pinned cross-language interop vector. The same (key, user, exp) mints this exact
// string in server.py (Python) and crawler-core (Java); if this breaks, SSO breaks.
const (
	refKey    = "testkey123"
	refUser   = "osmosis"
	refExp    = int64(9999999999)
	refCookie = "b3Ntb3Npczo5OTk5OTk5OTk5OmEzYmQ1NTU0MWI0MTBkOWUzYWUzZWFhNjU3OTdmMGY2NGU2ZDE4MjhlNzcyOTkzZGU5ZWY0ZDkwOWM2MTI1ZTQ="
)

func TestSsoInteropVector(t *testing.T) {
	if got := ssoMint(refKey, refUser, refExp); got != refCookie {
		t.Fatalf("mint mismatch:\n got  %s\n want %s", got, refCookie)
	}
	if !ssoValid(refKey, refCookie, 0) {
		t.Fatal("reference cookie should validate")
	}
	if ssoValid("wrongkey", refCookie, 0) {
		t.Fatal("wrong key must not validate")
	}
	if ssoValid(refKey, refCookie, refExp) {
		t.Fatal("expired cookie (now == exp) must not validate")
	}
	if ssoValid(refKey, "not-base64!!", 0) || ssoValid(refKey, "", 0) {
		t.Fatal("garbage/empty must not validate")
	}
}

func TestRequireAuthAcceptsSSO(t *testing.T) {
	ssoKey = "testkey123"
	defer func() { ssoKey = "" }()

	h := RequireAuth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	noCookie := httptest.NewRecorder()
	h(noCookie, httptest.NewRequest("GET", "/", nil))
	if noCookie.Code != http.StatusSeeOther {
		t.Fatalf("no cookie: want 303 redirect, got %d", noCookie.Code)
	}

	withSSO := httptest.NewRequest("GET", "/", nil)
	withSSO.AddCookie(&http.Cookie{Name: ssoCookieName, Value: ssoMint(ssoKey, "osmosis", time.Now().Add(time.Hour).Unix())})
	rec := httptest.NewRecorder()
	h(rec, withSSO)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid SSO cookie: want 200, got %d", rec.Code)
	}
}
