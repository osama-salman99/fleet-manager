package deployer

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A same-site (or cross-site) browser POST must not be able to trigger a redeploy on
// the strength of the Lax SSO cookie alone; a same-origin fetch and a non-browser
// caller (no Sec-Fetch-Site) must still get through. Uses an unknown service name so a
// passing request stops at 404 without kicking off a real Deploy.
func TestHandleRedeployCSRFGuard(t *testing.T) {
	cases := []struct {
		name string
		sfs  string // Sec-Fetch-Site header ("" = omitted, like a non-browser client)
		want int
	}{
		{"cross-site refused", "cross-site", http.StatusForbidden},
		{"same-site refused", "same-site", http.StatusForbidden},
		{"same-origin allowed through", "same-origin", http.StatusNotFound},
		{"non-browser (no header) allowed through", "", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/redeploy/no-such-service-xyz", nil)
			if c.sfs != "" {
				req.Header.Set("Sec-Fetch-Site", c.sfs)
			}
			rec := httptest.NewRecorder()
			HandleRedeploy(rec, req)
			if rec.Code != c.want {
				t.Fatalf("Sec-Fetch-Site %q: got %d, want %d", c.sfs, rec.Code, c.want)
			}
		})
	}
}
