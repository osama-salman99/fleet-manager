package deployer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// A push or build event must wake the build poller for that repo at once, so the
// Overview shows a push within seconds instead of on the next minute's poll.
// Pushes to other branches, and repos the deployer does not run, must not.
func TestWebhookNudgesBuildsOnPush(t *testing.T) {
	prevPush := buildPushDir
	buildPushDir = filepath.Join(t.TempDir(), "push")
	t.Cleanup(func() { buildPushDir = prevPush })
	mu.Lock()
	prev := cfg
	cfg = Config{WebhookSecret: "sec", Services: []Service{
		{Name: "a", Repo: "o/a", Enabled: true},
		{Name: "b", Repo: "o/b", Enabled: false},
	}}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); cfg = prev; mu.Unlock() })

	send := func(event, body string) {
		mac := hmac.New(sha256.New, []byte("sec"))
		mac.Write([]byte(body))
		r := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", event)
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		HandleWebhook(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", event, w.Code)
		}
	}
	got := func() string {
		select {
		case repo := <-nudges:
			return repo
		default:
			return ""
		}
	}

	send("push", `{"ref":"refs/heads/master","repository":{"full_name":"o/a"}}`)
	if r := got(); r != "o/a" {
		t.Fatalf("push to master: nudged %q, want o/a", r)
	}
	send("workflow_job", `{"action":"in_progress","repository":{"full_name":"o/a"}}`)
	if r := got(); r != "o/a" {
		t.Fatalf("build event: nudged %q, want o/a", r)
	}
	send("push", `{"ref":"refs/heads/feature","repository":{"full_name":"o/a"}}`)
	send("workflow_run", `{"repository":{"full_name":"o/b"}}`)
	send("push", `{"ref":"refs/heads/master","repository":{"full_name":"o/zzz"}}`)
	if r := got(); r != "" {
		t.Fatalf("nudged %q for a branch push, a disabled or an unknown repo", r)
	}
}
