package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Every way fetchArtifact can go: where the jar comes from (the build runner's work
// folder, the hour-long copy, a GitHub download) or why it gives up, for a release
// whose jar is already uploaded and for one that so far has only its checksum.

const relJar = "cart-1.0.0.jar"

var (
	released    = []byte("cart v43, as released")
	releasedSHA = func() string { s := sha256.Sum256(released); return hex.EncodeToString(s[:]) }()
)

type fetchWorld struct {
	t         *testing.T
	work      string // the runner's build/libs, gone once the job ends
	kept      string // the build runner's hour-long copy
	out       string // where the deployer stages the artifact
	gh        *httptest.Server
	downloads atomic.Int32
	failDL    bool
	rel       ghRelease
	relErr    error
	sumBody   string
	sumErr    error
}

func newFetchWorld(t *testing.T) *fetchWorld {
	w := &fetchWorld{t: t, sumBody: releasedSHA}
	root := t.TempDir()
	w.work = filepath.Join(root, "runner-2", "_work", "cart", "cart", "build", "libs")
	w.kept = filepath.Join(root, "artifacts", "cart")
	w.out = t.TempDir()
	os.MkdirAll(w.work, 0o755)
	os.MkdirAll(w.kept, 0o700)
	w.gh = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.downloads.Add(1)
		if w.failDL {
			rw.WriteHeader(http.StatusBadGateway)
			return
		}
		rw.Write(released)
	}))
	t.Cleanup(w.gh.Close)

	prevDirs, prevRel, prevSum := localArtifactDirs, fetchRelease, fetchChecksumBody
	localArtifactDirs = func(repo string) []string {
		return []string{filepath.Join(root, "runner*", "_work", repo, repo, "build", "libs"), filepath.Join(root, "artifacts", repo)}
	}
	fetchRelease = func(string, string) (ghRelease, error) { return w.rel, w.relErr }
	fetchChecksumBody = func(ghAsset) ([]byte, error) { return []byte(w.sumBody), w.sumErr }
	t.Cleanup(func() { localArtifactDirs, fetchRelease, fetchChecksumBody = prevDirs, prevRel, prevSum })
	return w
}

func (w *fetchWorld) sumAsset() ghAsset {
	return ghAsset{Name: relJar + ".sha256", URL: w.gh.URL + "/sum"}
}
func (w *fetchWorld) jarAsset() ghAsset { return ghAsset{Name: relJar, URL: w.gh.URL + "/jar"} }
func (w *fetchWorld) uploaded() {
	w.rel = ghRelease{TagName: "v43-x", Assets: []ghAsset{w.sumAsset(), w.jarAsset()}}
}
func (w *fetchWorld) checksumOnly() {
	w.rel = ghRelease{TagName: "v43-x", Assets: []ghAsset{w.sumAsset()}}
}
func (w *fetchWorld) put(dir string, b []byte) {
	if err := os.WriteFile(filepath.Join(dir, relJar), b, 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *fetchWorld) fetch(pattern string) (ghAsset, string, error) {
	s := Service{Name: "cart", Repo: "osama-salman99/cart", AssetPattern: pattern}
	asset, _, tmp, err := fetchArtifact(s, "v43-x", w.out)
	return asset, tmp, err
}

func TestFetchArtifactPaths(t *testing.T) {
	const fromWork, fromKept, fromGitHub, notUploaded, cannotLocate, downloadFailed = "work", "kept", "github", "not uploaded", "locate", "download"
	cases := []struct {
		name  string
		setup func(w *fetchWorld)
		want  string
	}{
		// The jar is already on the release.
		{"uploaded, built here, still in the work folder", func(w *fetchWorld) { w.uploaded(); w.put(w.work, released) }, fromWork},
		{"uploaded, built here, only the kept copy left", func(w *fetchWorld) { w.uploaded(); w.put(w.kept, released) }, fromKept},
		{"uploaded, work folder holds another build, kept copy matches", func(w *fetchWorld) {
			w.uploaded()
			w.put(w.work, []byte("a newer build"))
			w.put(w.kept, released)
		}, fromKept},
		{"uploaded, nothing built here", func(w *fetchWorld) { w.uploaded() }, fromGitHub},
		{"uploaded, local build differs", func(w *fetchWorld) { w.uploaded(); w.put(w.work, []byte("tampered")); w.put(w.kept, []byte("old")) }, fromGitHub},
		{"uploaded, no checksum published: local never trusted", func(w *fetchWorld) {
			w.rel = ghRelease{TagName: "v43-x", Assets: []ghAsset{w.jarAsset()}}
			w.put(w.work, released)
		}, fromGitHub},
		{"uploaded, checksum unreadable: local never trusted", func(w *fetchWorld) {
			w.uploaded()
			w.sumErr = errors.New("github blip")
			w.put(w.work, released)
		}, fromGitHub},
		{"uploaded, checksum file is not a checksum", func(w *fetchWorld) { w.uploaded(); w.sumBody = "nonsense"; w.put(w.work, released) }, fromGitHub},
		{"uploaded, local is a link to the right bytes", func(w *fetchWorld) {
			w.uploaded()
			other := filepath.Join(w.t.TempDir(), "x.jar")
			os.WriteFile(other, released, 0o644)
			os.Symlink(other, filepath.Join(w.work, relJar))
		}, fromGitHub},
		{"uploaded, nothing built here, download fails", func(w *fetchWorld) { w.uploaded(); w.failDL = true }, downloadFailed},

		// Published with only its checksum; the jar is still uploading.
		{"checksum only, built here, in the work folder", func(w *fetchWorld) { w.checksumOnly(); w.put(w.work, released) }, fromWork},
		{"checksum only, built here, only the kept copy left", func(w *fetchWorld) { w.checksumOnly(); w.put(w.kept, released) }, fromKept},
		{"checksum only, nothing built here", func(w *fetchWorld) { w.checksumOnly() }, notUploaded},
		{"checksum only, local build differs", func(w *fetchWorld) { w.checksumOnly(); w.put(w.work, []byte("tampered")) }, notUploaded},
		{"checksum only, checksum unreadable", func(w *fetchWorld) { w.checksumOnly(); w.sumErr = errors.New("blip"); w.put(w.work, released) }, notUploaded},
		{"checksum only, local is a link", func(w *fetchWorld) {
			w.checksumOnly()
			other := filepath.Join(w.t.TempDir(), "x.jar")
			os.WriteFile(other, released, 0o644)
			os.Symlink(other, filepath.Join(w.work, relJar))
		}, notUploaded},

		// Nothing to go on.
		{"release has neither jar nor checksum", func(w *fetchWorld) { w.rel = ghRelease{TagName: "v43-x"}; w.put(w.work, released) }, cannotLocate},
		{"checksum names a file the service does not deploy", func(w *fetchWorld) {
			w.rel = ghRelease{TagName: "v43-x", Assets: []ghAsset{{Name: "notes.txt.sha256"}}}
		}, cannotLocate},
		{"GitHub cannot be reached", func(w *fetchWorld) { w.relErr = errors.New("github 502"); w.put(w.work, released) }, cannotLocate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newFetchWorld(t)
			c.setup(w)
			asset, tmp, err := w.fetch(`cart-.*\.jar$`)
			staged, _ := filepath.Glob(filepath.Join(w.out, "deploy-*"))

			switch c.want {
			case fromWork, fromKept, fromGitHub:
				if err != nil {
					t.Fatalf("want the jar from %s, got error %v", c.want, err)
				}
				if asset.Name != relJar {
					t.Fatalf("asset name %q", asset.Name)
				}
				if b, _ := os.ReadFile(tmp); string(b) != string(released) {
					t.Fatalf("staged something other than the released jar: %q", b)
				}
				if filepath.Dir(tmp) != w.out || len(staged) != 1 {
					t.Fatalf("want exactly the one staged file in the deploy folder, have %v", staged)
				}
				if dl := w.downloads.Load(); (c.want == fromGitHub) != (dl == 1) || dl > 1 {
					t.Fatalf("want it from %s, but GitHub was downloaded from %d times", c.want, dl)
				}
			case notUploaded:
				if !errors.Is(err, errJarNotUploaded) {
					t.Fatalf("want errJarNotUploaded (so the retry waits for the upload), got %v", err)
				}
			case cannotLocate, downloadFailed:
				if err == nil || !strings.HasPrefix(err.Error(), c.want) {
					t.Fatalf("want a %q error, got %v", c.want, err)
				}
			}
			if c.want == notUploaded || c.want == cannotLocate || c.want == downloadFailed {
				if tmp != "" || len(staged) != 0 {
					t.Fatalf("a failed fetch left %q / %v behind", tmp, staged)
				}
				if c.want != downloadFailed && w.downloads.Load() != 0 {
					t.Fatal("downloaded although the release has no jar")
				}
			}
		})
	}
}

// The deploy sees a checksum-only release, waits, and installs the jar once the upload lands.
func TestFetchArtifactPicksUpTheJarOnceUploaded(t *testing.T) {
	w := newFetchWorld(t)
	w.checksumOnly()
	if _, _, err := w.fetch(`cart-.*\.jar$`); !errors.Is(err, errJarNotUploaded) {
		t.Fatalf("first attempt: want errJarNotUploaded, got %v", err)
	}
	w.uploaded()
	if _, tmp, err := w.fetch(`cart-.*\.jar$`); err != nil || tmp == "" {
		t.Fatalf("retry after the upload: %v", err)
	}
}

func TestFetchArtifactBadPattern(t *testing.T) {
	w := newFetchWorld(t)
	w.checksumOnly()
	w.put(w.work, released)
	if _, _, err := w.fetch(`cart-(`); err == nil || !strings.HasPrefix(err.Error(), "locate asset") {
		t.Fatalf("a broken asset pattern must stop the deploy, got %v", err)
	}
}

// A name from a release is joined onto a folder path; one that could leave the folder
// is never looked up, even when the file it points at holds exactly the released bytes.
func TestLocalArtifactRefusesUnsafeNames(t *testing.T) {
	w := newFetchWorld(t)
	os.WriteFile(filepath.Join(w.work, "..", "outside.jar"), released, 0o644)
	os.MkdirAll(filepath.Join(w.work, "sub"), 0o755)
	os.WriteFile(filepath.Join(w.work, "sub", relJar), released, 0o644)
	for _, name := range []string{"", "../outside.jar", "sub/" + relJar} {
		if got := localArtifact("cart", name, releasedSHA, w.out); got != "" {
			t.Errorf("looked up unsafe name %q: %s", name, got)
		}
	}
	// Glob characters in a name are refused too, rather than trusted to stay literal.
	for _, name := range []string{"*.jar", "cart-?.0.0.jar", "[c]art-1.0.0.jar"} {
		os.WriteFile(filepath.Join(w.work, name), released, 0o644)
		if got := localArtifact("cart", name, releasedSHA, w.out); got != "" {
			t.Errorf("looked up name with glob characters %q: %s", name, got)
		}
	}
}

// Sad paths on the local side: each must fall back to GitHub (or, with only the
// checksum published, wait for the upload), never install or crash.
func TestLocalArtifactSadPaths(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *fetchWorld)
	}{
		{"jar name is a folder", func(w *fetchWorld) { os.MkdirAll(filepath.Join(w.work, relJar), 0o755) }},
		{"local jar is empty", func(w *fetchWorld) { w.put(w.work, nil) }},
		{"local jar is cut short", func(w *fetchWorld) { w.put(w.work, released[:len(released)-1]) }},
		{"local jar has extra bytes", func(w *fetchWorld) { w.put(w.work, append(append([]byte{}, released...), '\n')) }},
		{"local jar cannot be read", func(w *fetchWorld) {
			if os.Geteuid() == 0 {
				w.t.Skip("root reads everything")
			}
			w.put(w.work, released)
			os.Chmod(filepath.Join(w.work, relJar), 0)
		}},
		{"search folders do not exist", func(w *fetchWorld) { os.RemoveAll(w.work); os.RemoveAll(w.kept) }},
	}
	for _, c := range cases {
		for _, phase := range []string{"uploaded", "checksum only"} {
			t.Run(c.name+", "+phase, func(t *testing.T) {
				w := newFetchWorld(t)
				if phase == "uploaded" {
					w.uploaded()
				} else {
					w.checksumOnly()
				}
				c.setup(w)
				_, tmp, err := w.fetch(`cart-.*\.jar$`)
				staged, _ := filepath.Glob(filepath.Join(w.out, "deploy-*"))
				if phase == "uploaded" {
					if err != nil || w.downloads.Load() != 1 {
						t.Fatalf("want a GitHub download, got err=%v downloads=%d", err, w.downloads.Load())
					}
					if b, _ := os.ReadFile(tmp); string(b) != string(released) || len(staged) != 1 {
						t.Fatalf("staged %q (%v)", b, staged)
					}
				} else if !errors.Is(err, errJarNotUploaded) || len(staged) != 0 || w.downloads.Load() != 0 {
					t.Fatalf("want to wait for the upload with nothing staged, got err=%v staged=%v", err, staged)
				}
			})
		}
	}
}

// The staging folder is gone (a broken state dir): nothing can be staged, from
// either source, and the deploy fails instead of installing something half-written.
func TestFetchArtifactCannotStage(t *testing.T) {
	w := newFetchWorld(t)
	w.uploaded()
	w.put(w.work, released)
	w.out = filepath.Join(w.out, "missing")
	if _, tmp, err := w.fetch(`cart-.*\.jar$`); err == nil || !strings.HasPrefix(err.Error(), "download") {
		t.Fatalf("want a download error, got %v (%q)", err, tmp)
	}
}
