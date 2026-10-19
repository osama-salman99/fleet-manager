package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalArtifactOnlyUsesACopyMatchingTheReleaseChecksum(t *testing.T) {
	root := t.TempDir()
	libs := filepath.Join(root, "runner-2", "_work", "cart", "cart", "build", "libs")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	old := localArtifactDirs
	localArtifactDirs = func(repo string) []string {
		return []string{filepath.Join(root, "runner*", "_work", repo, repo, "build", "libs")}
	}
	defer func() { localArtifactDirs = old }()

	jar := []byte("the jar the release published")
	sum := sha256.Sum256(jar)
	want := hex.EncodeToString(sum[:])
	out := t.TempDir()

	// Not built here: download instead.
	if got := localArtifact("cart", "cart-1.0.0.jar", want, out); got != "" {
		t.Fatalf("nothing on disk, yet a local copy was used: %s", got)
	}

	// A different build of the same name (an older run, a tampered file): refused.
	os.WriteFile(filepath.Join(libs, "cart-1.0.0.jar"), []byte("some other build"), 0o644)
	if got := localArtifact("cart", "cart-1.0.0.jar", want, out); got != "" {
		t.Fatalf("a file that does not match the release checksum was used: %s", got)
	}

	// No published checksum: nothing local is trusted.
	if got := localArtifact("cart", "cart-1.0.0.jar", "", out); got != "" {
		t.Fatalf("used a local copy without a checksum to check it against: %s", got)
	}

	// A link in place of the jar: refused even if what it points at matches.
	target := filepath.Join(root, "elsewhere.jar")
	os.WriteFile(target, jar, 0o644)
	os.Remove(filepath.Join(libs, "cart-1.0.0.jar"))
	os.Symlink(target, filepath.Join(libs, "cart-1.0.0.jar"))
	if got := localArtifact("cart", "cart-1.0.0.jar", want, out); got != "" {
		t.Fatalf("followed a link to another file: %s", got)
	}

	// The real build: used, as a copy in the download folder.
	os.Remove(filepath.Join(libs, "cart-1.0.0.jar"))
	os.WriteFile(filepath.Join(libs, "cart-1.0.0.jar"), jar, 0o644)
	got := localArtifact("cart", "cart-1.0.0.jar", want, out)
	if got == "" || filepath.Dir(got) != out {
		t.Fatalf("the matching local build was not used (got %q)", got)
	}
	if b, _ := os.ReadFile(got); string(b) != string(jar) {
		t.Fatalf("the copy differs from the build")
	}
	if left, _ := filepath.Glob(filepath.Join(out, "deploy-*")); len(left) != 1 {
		t.Fatalf("rejected copies were left behind: %v", left)
	}
}

func TestDeployStartsFromTheLocalJarBeforeTheUploadFinishes(t *testing.T) {
	root := t.TempDir()
	libs := filepath.Join(root, "runner", "_work", "cart", "cart", "build", "libs")
	os.MkdirAll(libs, 0o755)
	jar := []byte("cart v42")
	sum := sha256.Sum256(jar)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(jar) }))
	defer gh.Close()
	var rel ghRelease
	prevDirs, prevRel, prevSum := localArtifactDirs, fetchRelease, fetchChecksumBody
	localArtifactDirs = func(repo string) []string {
		return []string{filepath.Join(root, "runner*", "_work", repo, repo, "build", "libs")}
	}
	fetchRelease = func(string, string) (ghRelease, error) { return rel, nil }
	fetchChecksumBody = func(ghAsset) ([]byte, error) { return []byte(hex.EncodeToString(sum[:])), nil }
	defer func() { localArtifactDirs, fetchRelease, fetchChecksumBody = prevDirs, prevRel, prevSum }()

	s := Service{Name: "cart", Repo: "osama-salman99/cart", AssetPattern: `cart-.*\.jar$`}
	out := t.TempDir()
	checksumOnly := ghRelease{TagName: "v42-x", Assets: []ghAsset{{Name: "cart-1.0.0.jar.sha256", URL: gh.URL + "/sum"}}}
	uploaded := ghRelease{TagName: "v42-x", Assets: []ghAsset{checksumOnly.Assets[0], {Name: "cart-1.0.0.jar", URL: gh.URL + "/jar"}}}

	// Published with only its checksum, nothing built here: wait for the upload.
	rel = checksumOnly
	if _, _, tmp, err := fetchArtifact(s, "v42-x", out); err != errJarNotUploaded {
		t.Fatalf("want errJarNotUploaded, got %v (%q)", err, tmp)
	}

	// The same, with a local jar that is not the one released: still wait.
	os.WriteFile(filepath.Join(libs, "cart-1.0.0.jar"), []byte("another build"), 0o644)
	if _, _, tmp, err := fetchArtifact(s, "v42-x", out); err != errJarNotUploaded {
		t.Fatalf("a jar that does not match the checksum must not be installed; got %v (%q)", err, tmp)
	}

	// The released jar was built here: deploy it now, without waiting for the upload.
	os.WriteFile(filepath.Join(libs, "cart-1.0.0.jar"), jar, 0o644)
	asset, _, tmp, err := fetchArtifact(s, "v42-x", out)
	if err != nil || asset.Name != "cart-1.0.0.jar" {
		t.Fatalf("the local jar was not used before the upload finished: %v %+v", err, asset)
	}
	if b, _ := os.ReadFile(tmp); string(b) != string(jar) {
		t.Fatal("installed something other than the built jar")
	}
	os.Remove(tmp)

	// Once uploaded and with nothing local, it downloads as before.
	os.Remove(filepath.Join(libs, "cart-1.0.0.jar"))
	rel = uploaded
	asset, _, tmp, err = fetchArtifact(s, "v42-x", out)
	if err != nil || asset.URL != gh.URL+"/jar" {
		t.Fatalf("did not download the uploaded jar: %v %+v", err, asset)
	}
	if b, _ := os.ReadFile(tmp); string(b) != string(jar) {
		t.Fatal("the download differs from the jar")
	}
}
