package deployer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The release builds run on this VPS (the build runner), so the jar a release
// publishes was usually written to disk here seconds before we are told about it.
// Downloading it back from GitHub costs ~3-4 s of every deploy. Instead we look for
// that file locally and use it only if it hashes to the SHA-256 the release itself
// published, so a local copy can never install anything GitHub would not have given
// us. Anything missing, unreadable or different: download as before.

// localArtifactDirs lists, as globs, where a finished build of repo may still sit:
// the runner's work folder (gone once the runner exits), and the copy the build
// runner keeps of each finished jar for an hour (root-only).
var localArtifactDirs = func(repo string) []string {
	return []string{
		"/opt/build-runner/runner*/_work/" + repo + "/" + repo + "/build/libs",
		"/var/lib/build-runner/artifacts/" + repo,
	}
}

// expectedSHA returns the digest the release published beside asset, or "" when
// it published none (or it cannot be read), in which case nothing local is trusted.
func expectedSHA(rel ghRelease, asset ghAsset) string {
	ca, ok := findChecksumAsset(rel, asset.Name)
	if !ok {
		return ""
	}
	body, err := fetchChecksumBody(ca)
	if err != nil {
		return ""
	}
	want, err := parseExpectedSHA(body)
	if err != nil {
		return ""
	}
	return want
}

// localArtifact copies the first local file named name whose SHA-256 is want into
// a temp file in dir and returns its path, or "" if there is none.
func localArtifact(repo, name, want, dir string) string {
	if want == "" || name == "" || strings.ContainsAny(name, "/*?[") {
		return ""
	}
	for _, pat := range localArtifactDirs(repo) {
		dirs, _ := filepath.Glob(pat)
		for _, d := range dirs {
			if tmp, err := copyIfMatches(filepath.Join(d, name), want, dir); err == nil {
				return tmp
			}
		}
	}
	return ""
}

// copyIfMatches copies src to a temp file in dir, hashing as it goes, and keeps
// the copy only if it hashes to want. It hashes the copy, not src, so a file that
// changes underneath us cannot pass the check and then install something else.
func copyIfMatches(src, want, dir string) (string, error) {
	if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file: %s", src)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	f, err := os.CreateTemp(dir, "deploy-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), in)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("%s does not match the release checksum", src)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// errJarNotUploaded means the release is published but its jar is not attached
// yet and no matching copy was built here. Deploy's retry picks it up seconds later.
var errJarNotUploaded = fmt.Errorf("the release's jar is not uploaded yet and no matching copy was built here")

// fetchArtifact puts the release's artifact in a temp file in dir: the copy built on
// this VPS when it hashes to the release's published SHA-256, else a GitHub download.
//
// The release workflows publish "<jar>.sha256" first and upload the jar after it, so
// the deploy can start while the (tens of MB) upload is still running. For those few
// seconds the release names its jar only through that checksum file, and only the
// local copy can be used.
func fetchArtifact(s Service, tag, dir string) (ghAsset, ghRelease, string, error) {
	asset, rel, err := findAsset(s, tag)
	uploaded := err == nil
	if !uploaded {
		var ok bool
		if asset, ok = announcedArtifact(s, rel); !ok {
			return ghAsset{}, rel, "", fmt.Errorf("locate asset: %w", err)
		}
	}
	repo := s.Repo[strings.LastIndexByte(s.Repo, '/')+1:]
	if tmp := localArtifact(repo, asset.Name, expectedSHA(rel, asset), dir); tmp != "" {
		log.Printf("deployer: %s %s using the copy built on this VPS", s.Name, tag)
		return asset, rel, tmp, nil
	}
	if !uploaded {
		return ghAsset{}, rel, "", errJarNotUploaded
	}
	tmp, err := download(asset, dir)
	if err != nil {
		return ghAsset{}, rel, "", fmt.Errorf("download: %w", err)
	}
	return asset, rel, tmp, nil
}

// announcedArtifact finds the artifact a release names only through its checksum
// file: "<name>.sha256" whose <name> matches the service's asset pattern.
func announcedArtifact(s Service, rel ghRelease) (ghAsset, bool) {
	re, err := regexp.Compile(s.AssetPattern)
	if err != nil {
		return ghAsset{}, false
	}
	for _, a := range rel.Assets {
		if name, ok := strings.CutSuffix(a.Name, ".sha256"); ok && re.MatchString(name) {
			return ghAsset{Name: name}, true
		}
	}
	return ghAsset{}, false
}
