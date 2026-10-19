package deployer

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Swapping the artifact under a running service is the one place in this program
// where a mistake takes a site down and systemd cannot recover: a unit whose jar
// is missing restart-loops forever. So every function here holds one invariant.
//
//	s.Target always exists.
//
// Nothing is deleted in the hope that a later step succeeds. The new artifact is
// staged beside the target and renamed over it — rename(2) replaces the file
// atomically, so there is no instant at which the service has no jar. The old
// artifact is kept alive by a hard link, not by moving it out of the way.
//
// That invariant is not academic: on 2026-07-13 a rollback removed
// /opt/roofz-crawler/app.jar and then failed to restore the backup, and the
// service stayed down until a human copied a jar back by hand.

// backupPath is where the artifact being replaced is kept for the life of a
// deploy. It is unique per tag: a single shared ".bak" lets two runs overwrite
// each other's copy, so the second rollback finds the first one's broken jar —
// or nothing at all.
func backupPath(s Service, tag string) string {
	return s.Target + ".bak-" + safeTag(tag)
}

// safeTag keeps a release tag usable as a filename suffix.
func safeTag(tag string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		}
		return '-'
	}, tag)
}

// usable reports whether a path holds something we could actually run or restore:
// a non-empty file, or a non-empty directory. An empty file is exactly as fatal
// as a missing one, and half the point of this fix is to stop trusting either.
func usable(path, kind string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if kind == KindTarball {
		if !fi.IsDir() {
			return false
		}
		ents, err := os.ReadDir(path)
		return err == nil && len(ents) > 0
	}
	return fi.Mode().IsRegular() && fi.Size() > 0
}

// install puts the new artifact in place and returns the backup holding the one
// it replaced ("" when there was nothing there yet).
//
// On any error the target is left exactly as it was — the service keeps running
// the version it was already running.
func install(s Service, tmp, tag string) (string, error) {
	if s.Kind == KindTarball {
		return installTarball(s, tmp, tag)
	}
	return installFile(s, tmp, tag)
}

func installFile(s Service, tmp, tag string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(s.Target), 0o755); err != nil {
		return "", err
	}

	mode := os.FileMode(0o644)
	if s.Kind == KindBinary {
		mode = 0o755
	}

	// Stage beside the target so the swap below is a same-filesystem rename.
	staged := s.Target + ".new"
	if err := copyFile(tmp, staged, mode); err != nil {
		os.Remove(staged)
		return "", err
	}

	// Back the current artifact up with a hard link: it costs no disk and no
	// time, and the old inode survives the rename that replaces the directory
	// entry. Crucially the target is never unlinked, so a failure anywhere below
	// cannot leave the service with no artifact.
	backup := ""
	if usable(s.Target, s.Kind) {
		backup = backupPath(s, tag)
		os.Remove(backup)
		if err := os.Link(s.Target, backup); err != nil {
			// No hard links (or a different device): copy instead. Slower, same
			// guarantee — the target still is not touched.
			if err := copyFile(s.Target, backup, mode); err != nil {
				os.Remove(staged)
				return "", fmt.Errorf("backup: %w", err)
			}
		}
	}

	if err := os.Rename(staged, s.Target); err != nil {
		os.Remove(staged)
		return backup, fmt.Errorf("swap: %w", err)
	}
	return backup, nil
}

func installTarball(s Service, tmp, tag string) (string, error) {
	// A directory cannot be swapped atomically, so unpack the whole tree first
	// and only touch the live one once the replacement is complete on disk.
	staged := s.Target + ".new"
	os.RemoveAll(staged)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return "", err
	}
	if err := untar(tmp, staged); err != nil {
		os.RemoveAll(staged)
		return "", err
	}

	backup := ""
	if usable(s.Target, s.Kind) {
		backup = backupPath(s, tag)
		os.RemoveAll(backup)
		if err := os.Rename(s.Target, backup); err != nil {
			os.RemoveAll(staged)
			return "", fmt.Errorf("backup: %w", err)
		}
	}

	if err := os.Rename(staged, s.Target); err != nil {
		// Put the old tree straight back rather than leaving the site with nothing.
		if backup != "" {
			_ = os.Rename(backup, s.Target)
		}
		os.RemoveAll(staged)
		return "", fmt.Errorf("swap: %w", err)
	}
	return backup, nil
}

// rollback restores the artifact a deploy replaced, and reports whether it
// actually worked — the alert that follows must never claim a rollback that did
// not happen. That was the last defect of the three: the one message you would
// rely on to tell you the VPS needs a human told you the opposite.
//
// It validates the backup before touching anything, and never removes the
// current artifact to make room: a bad-but-present jar is strictly better than
// an empty directory.
func rollback(s Service, backup string) error {
	if backup == "" {
		return fmt.Errorf("no previous artifact to restore")
	}
	if !usable(backup, s.Kind) {
		return fmt.Errorf("backup %s is missing or empty", filepath.Base(backup))
	}
	log.Printf("deployer: rolling back %s", s.Name)

	if s.Kind == KindTarball {
		// Park the failed tree instead of deleting it, so a failed restore can
		// still put something back.
		failed := s.Target + ".failed"
		os.RemoveAll(failed)
		if usable(s.Target, s.Kind) {
			if err := os.Rename(s.Target, failed); err != nil {
				return fmt.Errorf("park failed tree: %w", err)
			}
		}
		if err := os.Rename(backup, s.Target); err != nil {
			_ = os.Rename(failed, s.Target) // best effort: leave *something* serving
			return fmt.Errorf("restore: %w", err)
		}
		os.RemoveAll(failed)
	} else {
		// rename(2) replaces the target atomically: the file is never absent.
		if err := os.Rename(backup, s.Target); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
	}

	if err := restart(s); err != nil {
		return fmt.Errorf("restart after restore: %w", err)
	}
	return nil
}

// selfHeal puts an artifact back when a previous run left the target missing.
// A unit in that state is hard-down right now, and a deploy that is about to
// fail would otherwise still have nothing to roll back to.
func selfHeal(s Service) {
	if usable(s.Target, s.Kind) {
		return
	}
	for _, m := range backups(s) {
		if !usable(m, s.Kind) {
			continue
		}
		if err := os.Rename(m, s.Target); err != nil {
			continue
		}
		log.Printf("deployer: %s had no artifact — restored %s", s.Name, filepath.Base(m))
		_ = restart(s)
		return
	}
}

// backups lists every artifact copy lying beside the target, newest names last.
// It matches the legacy shared ".bak" as well as the per-tag ones, because the
// one left behind by hand during the incident is still a perfectly good jar.
func backups(s Service) []string {
	m, _ := filepath.Glob(s.Target + ".bak*")
	return m
}

// pruneBackups clears the copies out after a deploy that worked. Only called on
// success: while a deploy is in flight these files are the only way back.
//
// vps-health deploys itself, and a process cannot roll itself back: verify() and
// restart() both short-circuit for the self unit, and the actual restart happens
// ~2s later in a detached goroutine — so by the time the new binary comes up (and
// possibly panics on a config error or a failed port-bind), this function has
// already run. Pruning every backup here would delete the last-known-good binary
// seconds before it is needed, leaving a restart-loop with nothing to restore but
// a manual rebuild. So on the self path, keep the most recent backup as the
// last-known-good and only clear older ones, which is enough for a manual (or
// future automated) recovery to copy the good binary back into place.
func pruneBackups(s Service) {
	if isSelf(s) {
		keepLatestBackup(s)
		return
	}
	for _, m := range backups(s) {
		os.RemoveAll(m)
	}
}

// isSelf reports whether this service is vps-health deploying itself — the one
// unit that cannot be rolled back in-process.
func isSelf(s Service) bool {
	return s.Unit == "vps-health.service"
}

// keepLatestBackup removes every backup beside the target except the newest by
// mtime. A backup is a hard link to the binary that was live before its deploy,
// so it carries that binary's install time; the newest is therefore the
// last-known-good. Keeping exactly one bounds disk use while never destroying the
// only route back from a bad self-deploy.
func keepLatestBackup(s Service) {
	all := backups(s)
	var newest string
	var newestT time.Time
	for _, m := range all {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if newest == "" || fi.ModTime().After(newestT) {
			newest, newestT = m, fi.ModTime()
		}
	}
	for _, m := range all {
		if m != newest {
			os.RemoveAll(m)
		}
	}
}

func untar(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Reject path traversal (../) before touching the filesystem.
		target := filepath.Join(dest, filepath.Clean("/"+h.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			return fmt.Errorf("unsafe path in archive: %s", h.Name)
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
