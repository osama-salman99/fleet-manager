package deployer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The target path comes out of a hand-edited JSON file, and this function is the
// only thing standing between a typo there and an os.RemoveAll on something the
// host needs.
func TestRemovableTarget(t *testing.T) {
	ok := []string{
		"/opt/holland2stay-crawler/app.jar",
		"/var/www/age",
		"/opt/vps-health/vps-health",
	}
	for _, p := range ok {
		if err := removableTarget(p); err != nil {
			t.Errorf("removableTarget(%q) = %v, want nil", p, err)
		}
	}

	bad := []string{
		"",            // no target configured at all
		"   ",         // whitespace is not a path
		"/",           // the host
		"/opt",        // every service's parent
		"/var/www",    // every static site
		"opt/app.jar", // relative: resolved against wherever this happens to run
		"/opt/../",    // shallow once cleaned
	}
	for _, p := range bad {
		if err := removableTarget(p); err == nil {
			t.Errorf("removableTarget(%q) = nil, want refusal", p)
		}
	}
}

func TestDecommissionUnknownService(t *testing.T) {
	withConfig(t, Config{Services: []Service{{Name: "a"}}})
	if _, err := Decommission("nope"); err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("Decommission(unknown) = %v, want an unknown-service error", err)
	}
}

// vps-health runs this code: decommissioning it would stop the unit serving the
// request and delete the binary that would have to start again.
func TestDecommissionRefusesSelf(t *testing.T) {
	withConfig(t, Config{Services: []Service{
		{Name: "vps-health", Unit: "vps-health.service", Target: "/opt/vps-health/vps-health", Enabled: true},
	}})
	if _, err := Decommission("vps-health"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Decommission(self) = %v, want a refusal", err)
	}
	if !Get().Services[0].Enabled {
		t.Error("a refused decommission still switched the service off")
	}
}

// The end state that matters: the build and its bookkeeping are gone from disk and
// the service can no longer be picked up by the poller.
func TestDecommissionRemovesBuildAndDisables(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "opt", "demo", "app.jar")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := Service{Name: "demo", Kind: KindJar, Target: target, Enabled: true} // no Unit: nothing to systemctl
	for _, f := range []string{target, target + ".bak-v1", deployedTagPath(svc), failedTagPath(svc)} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withConfig(t, Config{Services: []Service{svc}})

	if _, err := Decommission("demo"); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	for _, f := range []string{target, target + ".bak-v1", deployedTagPath(svc), failedTagPath(svc)} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still exists after decommission", filepath.Base(f))
		}
	}
	if Get().Services[0].Enabled {
		t.Error("service still enabled after decommission")
	}
}

// withConfig points the package at a throwaway config file and loads the given
// config into it, so a test can exercise the real Get/Put path without touching
// the real /opt/vps-health/deploy-config.json.
func withConfig(t *testing.T, c Config) {
	t.Helper()
	orig := configPath
	configPath = filepath.Join(t.TempDir(), "deploy-config.json")
	t.Cleanup(func() { configPath = orig })
	if err := Put(c); err != nil {
		t.Fatalf("seed config: %v", err)
	}
}
