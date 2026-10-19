package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExpectedSHA(t *testing.T) {
	valid := strings.Repeat("a", 64)
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare hex", valid, valid, false},
		{"uppercase folds to lower", strings.ToUpper(valid), valid, false},
		{"gnu sha256sum line", valid + "  vps-health-linux-amd64", valid, false},
		{"trailing newline", valid + "\n", valid, false},
		{"bsd form", "SHA256 (vps-health-linux-amd64) = " + valid, valid, false},
		{"empty", "", "", true},
		{"whitespace only", "   \n", "", true},
		{"too short", strings.Repeat("a", 63), "", true},
		{"too long", strings.Repeat("a", 65), "", true},
		{"non-hex", strings.Repeat("g", 64), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseExpectedSHA([]byte(c.in))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "artifact")
	content := []byte("the quick brown fox")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	got, err := fileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}

	if _, err := fileSHA256(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected error hashing a missing file")
	}
}

func TestFindChecksumAsset(t *testing.T) {
	rel := ghRelease{Assets: []ghAsset{
		{Name: "vps-health-linux-amd64", URL: "u1"},
		{Name: "vps-health-linux-amd64.sha256", URL: "u2"},
	}}
	if a, ok := findChecksumAsset(rel, "vps-health-linux-amd64"); !ok || a.URL != "u2" {
		t.Fatalf("expected the .sha256 companion, got ok=%v a=%+v", ok, a)
	}
	if _, ok := findChecksumAsset(rel, "some-other-jar"); ok {
		t.Fatal("did not expect a companion for a different artifact")
	}
	relNoCompanion := ghRelease{Assets: []ghAsset{{Name: "app.jar", URL: "u"}}}
	if _, ok := findChecksumAsset(relNoCompanion, "app.jar"); ok {
		t.Fatal("did not expect a companion when none was published")
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "deploy-artifact")
	content := []byte("release binary bytes")
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	realHex := hex.EncodeToString(sum[:])

	asset := ghAsset{Name: "app.jar", URL: "main"}
	withCompanion := ghRelease{Assets: []ghAsset{asset, {Name: "app.jar.sha256", URL: "sum"}}}
	noCompanion := ghRelease{Assets: []ghAsset{asset}}

	prev := fetchChecksumBody
	t.Cleanup(func() { fetchChecksumBody = prev })

	// Published and matching -> OK.
	fetchChecksumBody = func(a ghAsset) ([]byte, error) { return []byte(realHex), nil }
	if st, want, got, err := verifyChecksum(withCompanion, asset, tmp); st != checksumOK || err != nil || want != got {
		t.Fatalf("match: st=%v want=%s got=%s err=%v", st, want, got, err)
	}

	// Published and NOT matching -> mismatch, with both digests reported.
	other := strings.Repeat("b", 64)
	fetchChecksumBody = func(a ghAsset) ([]byte, error) { return []byte(other), nil }
	if st, want, got, err := verifyChecksum(withCompanion, asset, tmp); st != checksumMismatch || err != nil || want != other || got != realHex {
		t.Fatalf("mismatch: st=%v want=%s got=%s err=%v", st, want, got, err)
	}

	// No companion published -> absent, no error.
	if st, _, _, err := verifyChecksum(noCompanion, asset, tmp); st != checksumAbsent || err != nil {
		t.Fatalf("absent: st=%v err=%v", st, err)
	}

	// Companion present but unfetchable -> absent + error (unverified, not tampering).
	fetchChecksumBody = func(a ghAsset) ([]byte, error) { return nil, os.ErrDeadlineExceeded }
	if st, _, _, err := verifyChecksum(withCompanion, asset, tmp); st != checksumAbsent || err == nil {
		t.Fatalf("fetch-error: st=%v err=%v", st, err)
	}

	// Companion present but garbage body -> absent + parse error.
	fetchChecksumBody = func(a ghAsset) ([]byte, error) { return []byte("not-a-checksum"), nil }
	if st, _, _, err := verifyChecksum(withCompanion, asset, tmp); st != checksumAbsent || err == nil {
		t.Fatalf("garbage: st=%v err=%v", st, err)
	}
}
