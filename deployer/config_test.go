package deployer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// saveLocked must write atomically (temp + rename) and round-trip the secrets, and
// leave no temp file behind — a truncated config is what makes Load() reseed defaults
// and lose the token and webhook secret.
func TestSaveLockedAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prev := configPath
	configPath = filepath.Join(dir, "deploy-config.json")
	t.Cleanup(func() { configPath = prev })

	mu.Lock()
	cfg = Config{GitHubToken: "tok-123", WebhookSecret: "sec-456",
		Services: []Service{{Name: "x", Enabled: false}}}
	err := saveLocked()
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file was not cleaned up / renamed")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if got.GitHubToken != "tok-123" || got.WebhookSecret != "sec-456" {
		t.Fatalf("round-trip lost secrets: %+v", got)
	}
}

// A corrupt existing config must be preserved (not silently overwritten with defaults)
// and must alert — losing the token/secret unnoticed is the harm.
func TestLoadCorruptPreservesAndAlerts(t *testing.T) {
	dir := t.TempDir()
	prev := configPath
	configPath = filepath.Join(dir, "deploy-config.json")
	t.Cleanup(func() { configPath = prev })

	// a truncated write that fails to parse but still holds the (now unusable) token
	if err := os.WriteFile(configPath, []byte(`{"github_token":"real-tok","services":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	var alerted bool
	prevNotify := notify
	notify = func(title, message, tag, priority string) { alerted = true }
	t.Cleanup(func() { notify = prevNotify })

	Load()

	if _, err := os.Stat(configPath + ".corrupt"); err != nil {
		t.Fatalf("corrupt config was not preserved as .corrupt: %v", err)
	}
	if !alerted {
		t.Fatal("expected an urgent alert on a corrupt config")
	}
	if Get().GitHubToken == "real-tok" {
		t.Fatal("must not have adopted a value from the corrupt file")
	}
}
