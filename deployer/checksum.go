package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Artifact integrity. The deployer installs GitHub release assets as root and
// restarts a unit on them — so whoever can publish a release can run code as root
// on this VPS. A release is only as trustworthy as the account/token that made it;
// a compromised PAT is the one path from "someone stole a credential" to "someone
// owns the VPS". A checksum published *alongside* the artifact does not close that
// on its own (an attacker who can forge the artifact can forge its checksum too),
// but it does catch the cases that actually happen without any attacker: a
// truncated/corrupted download, a mismatched build, an asset swapped by mistake —
// and it is the seam a signature check slots into later.
//
// Rollout is staged so it can never brick a deploy while only some repos emit a
// checksum:
//   1. deployer verifies fail-open (this state): a mismatch is alerted loudly but
//      the install proceeds; a release with no checksum is installed silently.
//   2. every release workflow emits an "<asset>.sha256" companion.
//   3. flip checksumEnforced to true: a mismatch then fails the attempt and rolls
//      back, exactly like a failed health check.
//
// The companion is a separate tiny asset rather than a line in the release body
// because the body is auto-generated markdown (`gh release create --generate-notes`)
// and parsing freeform text for a digest is fragile; an "<asset>.sha256" file is
// unambiguous and matches the `sha256sum` convention.

// checksumEnforced makes a mismatch fail the deploy (stage 3). Left false until
// every release workflow emits a companion checksum, so a legitimate release from
// a not-yet-migrated repo still installs.
const checksumEnforced = false

// checksumStatus is the outcome of comparing a downloaded artifact against the
// SHA-256 published beside its release.
type checksumStatus int

const (
	checksumOK       checksumStatus = iota // a checksum was published and the download matches it
	checksumAbsent                         // the release published no companion (a repo not yet emitting, or unverifiable)
	checksumMismatch                       // a checksum was published and the download does NOT match — the dangerous case
)

// checksumAssetName is the companion asset carrying an artifact's SHA-256:
// "<artifact>.sha256", holding the lowercase hex digest.
func checksumAssetName(artifact string) string { return artifact + ".sha256" }

// findChecksumAsset returns the "<artifact>.sha256" companion asset, if the
// release published one.
func findChecksumAsset(rel ghRelease, artifact string) (ghAsset, bool) {
	want := checksumAssetName(artifact)
	for _, a := range rel.Assets {
		if a.Name == want {
			return a, true
		}
	}
	return ghAsset{}, false
}

// parseExpectedSHA pulls the hex digest out of a .sha256 asset body. It accepts a
// bare 64-char hex string, a GNU `sha256sum` line ("<hex>  <name>"), or the BSD
// form ("SHA256 (name) = <hex>") — whichever a workflow happens to emit.
func parseExpectedSHA(body []byte) (string, error) {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "", fmt.Errorf("empty checksum")
	}
	// BSD form: take what follows the '='.
	if strings.Contains(s, "(") && strings.Contains(s, "=") {
		s = strings.TrimSpace(s[strings.LastIndex(s, "=")+1:])
	}
	tok := strings.ToLower(strings.Fields(s)[0])
	if len(tok) != 64 {
		return "", fmt.Errorf("not a sha-256 hex digest: %q", tok)
	}
	for _, r := range tok {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return "", fmt.Errorf("checksum is not hex: %q", tok)
		}
	}
	return tok, nil
}

// fileSHA256 returns the lowercase hex SHA-256 of a file on disk.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchChecksumBody downloads a companion .sha256 asset and returns its bytes.
// A var so a test can drive verifyChecksum without a network. The companion is a
// single hex line, so cap what we read to keep a wrong/huge asset from being
// slurped into memory.
var fetchChecksumBody = func(a ghAsset) ([]byte, error) {
	tmp, err := download(a, "")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	f, err := os.Open(tmp)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 4096))
}

// verifyChecksum compares the artifact downloaded to tmp against the SHA-256 the
// release published as an "<asset>.sha256" companion. It returns the status, the
// expected/got digests (for logging), and a non-nil err only when a checksum was
// published but could not be evaluated (fetch/parse/hash failure) — the caller
// treats that as unverified, not as tampering.
func verifyChecksum(rel ghRelease, asset ghAsset, tmp string) (checksumStatus, string, string, error) {
	ca, ok := findChecksumAsset(rel, asset.Name)
	if !ok {
		return checksumAbsent, "", "", nil
	}
	body, err := fetchChecksumBody(ca)
	if err != nil {
		return checksumAbsent, "", "", fmt.Errorf("fetch checksum: %w", err)
	}
	want, err := parseExpectedSHA(body)
	if err != nil {
		return checksumAbsent, "", "", fmt.Errorf("parse checksum: %w", err)
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		return checksumAbsent, "", "", fmt.Errorf("hash artifact: %w", err)
	}
	if got != want {
		return checksumMismatch, want, got, nil
	}
	return checksumOK, want, got, nil
}
