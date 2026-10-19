package deployer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A queued VPS build must leave a wake-up file for the build runner straight away,
// instead of waiting up to 20 s for its next look at GitHub.
func TestQueuedVPSJobWakesTheBuildRunner(t *testing.T) {
	dir := t.TempDir()
	prev := buildWakeDir
	buildWakeDir = filepath.Join(dir, "wake")
	t.Cleanup(func() { buildWakeDir = prev })
	woke := func(repo string) bool {
		_, err := os.Stat(filepath.Join(buildWakeDir, repo))
		os.Remove(filepath.Join(buildWakeDir, repo))
		return err == nil
	}

	wakeBuildRunner("queued", "o/cart-app", []string{"self-hosted", "vps"})
	if !woke("cart-app") {
		t.Fatal("queued vps job: no wake-up file")
	}
	wakeBuildRunner("in_progress", "o/cart-app", []string{"vps"})
	wakeBuildRunner("queued", "o/cart-app", []string{"ubuntu-latest"})
	wakeBuildRunner("queued", "o/..", []string{"vps"})
	if left, _ := os.ReadDir(buildWakeDir); len(left) > 0 {
		t.Fatal("woke the runner for a started job, a GitHub-hosted job, or a bad name")
	}
}

func TestWebhookPassesQueuedJobsToTheBuildRunner(t *testing.T) {
	prev := buildWakeDir
	buildWakeDir = filepath.Join(t.TempDir(), "wake")
	t.Cleanup(func() { buildWakeDir = prev })
	mu.Lock()
	prevCfg := cfg
	cfg = Config{WebhookSecret: "sec"}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); cfg = prevCfg; mu.Unlock() })

	body := `{"action":"queued","repository":{"full_name":"o/mail-app"},"workflow_job":{"labels":["self-hosted","linux","x64","vps"]}}`
	mac := hmac.New(sha256.New, []byte("sec"))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", "workflow_job")
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	HandleWebhook(httptest.NewRecorder(), r)
	if _, err := os.Stat(filepath.Join(buildWakeDir, "mail-app")); err != nil {
		t.Fatalf("webhook did not wake the build runner: %v", err)
	}
}

// A push to master of a deployed service must tell the build runner at once, ~2 s before
// GitHub queues the build, so a runner is listening by the time the job arrives. Tags,
// other branches, deleted branches and repos we do not deploy must not start one.
func TestPushToMasterStartsARunnerEarly(t *testing.T) {
	prev := buildPushDir
	buildPushDir = filepath.Join(t.TempDir(), "push")
	t.Cleanup(func() { buildPushDir = prev })
	mu.Lock()
	prevCfg := cfg
	cfg = Config{WebhookSecret: "sec", Services: []Service{
		{Name: "cart", Repo: "o/cart", Enabled: true},
		{Name: "old", Repo: "o/old", Enabled: false}}}
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); cfg = prevCfg; mu.Unlock() })

	push := func(repo, ref string, deleted bool) bool {
		body := `{"ref":"` + ref + `","deleted":` + map[bool]string{true: "true", false: "false"}[deleted] +
			`,"repository":{"full_name":"` + repo + `"}}`
		mac := hmac.New(sha256.New, []byte("sec"))
		mac.Write([]byte(body))
		r := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(body))
		r.Header.Set("X-GitHub-Event", "push")
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		HandleWebhook(httptest.NewRecorder(), r)
		select { // leave the build poller's queue as the other tests expect it
		case <-nudges:
		default:
		}
		name := repo[strings.LastIndexByte(repo, '/')+1:]
		_, err := os.Stat(filepath.Join(buildPushDir, name))
		os.Remove(filepath.Join(buildPushDir, name))
		return err == nil
	}

	if !push("o/cart", "refs/heads/master", false) {
		t.Fatal("push to master of a deployed service: no early runner")
	}
	for _, c := range []struct {
		repo, ref string
		deleted   bool
	}{
		{"o/cart", "refs/tags/v36-abc", false},
		{"o/cart", "refs/heads/feature", false},
		{"o/cart", "refs/heads/master", true},
		{"o/old", "refs/heads/master", false},
		{"o/unknown", "refs/heads/master", false},
	} {
		if push(c.repo, c.ref, c.deleted) {
			t.Fatalf("started an early runner for %+v", c)
		}
	}
}
