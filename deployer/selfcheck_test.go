package deployer

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionOf(t *testing.T) {
	for tag, want := range map[string]string{"v61-72b82dc": "v61", "v7": "v7", "v178-a-b": "v178"} {
		if got := versionOf(tag); got != want {
			t.Errorf("versionOf(%q) = %q, want %q", tag, got, want)
		}
	}
}

func staticSite(t *testing.T, status int, body string) Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	prevTries, prevGap := staticTries, staticGap
	staticTries, staticGap = 2, 0
	t.Cleanup(func() { staticTries, staticGap = prevTries, prevGap })
	return Service{Name: "site", Kind: KindTarball, HealthURL: srv.URL + "/version.json"}
}

// The static site used to have no check at all: verify returned nil for a unit-less
// service, so a deploy that left the old files (or none) being served passed.
func TestVerifyStaticWantsTheNewVersion(t *testing.T) {
	if err := verify(staticSite(t, 200, `{"version":"v61"}`), "v61-72b82dc"); err != nil {
		t.Fatalf("new version served, want pass: %v", err)
	}
	if err := verify(staticSite(t, 200, `{"version":"v60"}`), "v61-72b82dc"); err == nil {
		t.Fatal("old version still served, want failure")
	}
	if err := verify(staticSite(t, 404, `not found`), "v61-72b82dc"); err == nil {
		t.Fatal("version file missing, want failure")
	}
	if err := verify(Service{Name: "site", Kind: KindTarball}, "v61-x"); err != nil {
		t.Fatalf("no health URL configured keeps the old behaviour, got %v", err)
	}
}

// runSelfCheck runs the real checker script with fake curl and systemctl on PATH.
func runSelfCheck(t *testing.T, served string) (dir string, out string) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	write(t, filepath.Join(bin, "curl"), "#!/bin/sh\nprintf '%s' '"+served+"'\n")
	write(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\necho \"$@\" > \""+filepath.Join(dir, "restarted")+"\"\n")
	os.Chmod(filepath.Join(bin, "curl"), 0o755)
	os.Chmod(filepath.Join(bin, "systemctl"), 0o755)
	write(t, filepath.Join(dir, "vps-health"), "NEW-BROKEN")
	write(t, filepath.Join(dir, "vps-health.bak-v178"), "OLD-GOOD")
	write(t, filepath.Join(dir, ".deployed-tag"), "v178-new")

	cmd := exec.Command("/bin/sh", "-c", selfCheckScript, "selfcheck",
		"http://127.0.0.1:8200/version", "v178", "4", filepath.Join(dir, "vps-health.bak-v178"),
		filepath.Join(dir, "vps-health"), filepath.Join(dir, ".deployed-tag"), "v177-old",
		filepath.Join(dir, ".self-rollback"), "vps-health.service", "v178-new")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	b, _ := cmd.CombinedOutput()
	return dir, string(b)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

func TestSelfCheckLeavesAHealthyReleaseAlone(t *testing.T) {
	dir, out := runSelfCheck(t, `{"version":"v178"}`)
	if got := readFile(t, filepath.Join(dir, "vps-health")); got != "NEW-BROKEN" {
		t.Fatalf("binary replaced although the new version answered (%q): %s", got, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "restarted")); err == nil {
		t.Fatal("restarted a healthy release")
	}
}

// Before this, a vps-health release that never came up stayed down: the watchdog
// that restarts crashed services runs inside vps-health itself.
func TestSelfCheckRestoresTheOldBinaryWhenTheNewOneNeverAnswers(t *testing.T) {
	start := time.Now()
	dir, out := runSelfCheck(t, `{"version":"v177"}`)
	if time.Since(start) < 3*time.Second {
		t.Fatalf("gave up before the deadline: %s", out)
	}
	if got := readFile(t, filepath.Join(dir, "vps-health")); got != "OLD-GOOD" {
		t.Fatalf("binary = %q, want the backup restored: %s", got, out)
	}
	if got := readFile(t, filepath.Join(dir, ".deployed-tag")); got != "v177-old" {
		t.Fatalf("deployed tag = %q, want the previous tag back so the poller sees the gap", got)
	}
	if got := readFile(t, filepath.Join(dir, ".self-rollback")); got != "v178-new" {
		t.Fatalf("rollback note = %q, want the failed tag", got)
	}
	if got := readFile(t, filepath.Join(dir, "restarted")); !strings.Contains(got, "restart vps-health.service") {
		t.Fatalf("systemctl called with %q, want a restart of the unit", got)
	}
}

func TestCheckSelfRollbackRecordsTheFailedRelease(t *testing.T) {
	dir := t.TempDir()
	self := Service{Name: "vps-health", Kind: KindBinary, Unit: "vps-health.service",
		Target: filepath.Join(dir, "vps-health")}
	prev := cfg
	cfg = Config{Services: []Service{self}}
	t.Cleanup(func() { cfg = prev })
	prevNotify := notify
	alerted := ""
	notify = func(title, message, tag, priority string) { alerted = title }
	t.Cleanup(func() { notify = prevNotify })

	write(t, selfRollbackNote(self), "v178-new")
	CheckSelfRollback()

	if failedTag(self) != "v178-new" {
		t.Fatalf("failed tag = %q, want the rolled-back release recorded", failedTag(self))
	}
	if alerted == "" {
		t.Fatal("no alert sent for the rollback")
	}
	if _, err := os.Stat(selfRollbackNote(self)); !os.IsNotExist(err) {
		t.Fatal("rollback note left behind; it would be re-reported on every start")
	}
}
