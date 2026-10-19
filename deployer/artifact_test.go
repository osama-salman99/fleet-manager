package deployer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The whole point of these tests is one property: whatever goes wrong,
// s.Target still exists afterwards. A missing artifact is the only failure
// systemd cannot recover from — it restart-loops on it forever.

func jarService(t *testing.T) (Service, string) {
	t.Helper()
	dir := t.TempDir()
	return Service{
		Name:   "test-svc",
		Kind:   KindJar,
		Target: filepath.Join(dir, "app.jar"),
		// No Unit: restart() is a no-op, so these tests never touch systemd.
	}, dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// The incident, exactly: rollback is asked to restore a backup that is not
// there. The old code removed the target first and then discovered the backup
// was missing, leaving /opt/<svc>/ empty.
func TestRollbackWithMissingBackupKeepsTheArtifact(t *testing.T) {
	s, dir := jarService(t)
	write(t, s.Target, "the running jar")

	err := rollback(s, filepath.Join(dir, "app.jar.bak-v9-gone"))
	if err == nil {
		t.Fatal("rollback should report failure when the backup is missing")
	}
	if got := read(t, s.Target); got != "the running jar" {
		t.Fatalf("the artifact was destroyed: %q", got)
	}
}

// An empty backup file is exactly as fatal as a missing one — restoring it
// would leave systemd with a 0-byte jar.
func TestRollbackWithEmptyBackupKeepsTheArtifact(t *testing.T) {
	s, dir := jarService(t)
	write(t, s.Target, "the running jar")
	empty := filepath.Join(dir, "app.jar.bak-v9-empty")
	write(t, empty, "")

	if err := rollback(s, empty); err == nil {
		t.Fatal("rollback should refuse an empty backup")
	}
	if got := read(t, s.Target); got != "the running jar" {
		t.Fatalf("the artifact was destroyed: %q", got)
	}
}

func TestRollbackWithNoBackupAtAllKeepsTheArtifact(t *testing.T) {
	s, _ := jarService(t)
	write(t, s.Target, "the running jar")

	if err := rollback(s, ""); err == nil {
		t.Fatal("rollback should report that there is nothing to restore")
	}
	if got := read(t, s.Target); got != "the running jar" {
		t.Fatalf("the artifact was destroyed: %q", got)
	}
}

// The happy path of a failed deploy: install the bad jar, roll back, end up on
// the old one.
func TestInstallThenRollbackRestoresThePreviousJar(t *testing.T) {
	s, _ := jarService(t)
	write(t, s.Target, "good jar")

	tmp := filepath.Join(t.TempDir(), "new.jar")
	write(t, tmp, "bad jar")

	backup, err := install(s, tmp, "v7-broken")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, s.Target); got != "bad jar" {
		t.Fatalf("install did not put the new jar in place: %q", got)
	}
	if backup == "" {
		t.Fatal("install should have kept a backup of the jar it replaced")
	}

	if err := rollback(s, backup); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if got := read(t, s.Target); got != "good jar" {
		t.Fatalf("rollback did not restore the previous jar: %q", got)
	}
}

// install must never leave a window where the service has no jar, and must not
// touch the target at all if it cannot stage the replacement.
func TestInstallFromMissingSourceLeavesTheArtifactAlone(t *testing.T) {
	s, _ := jarService(t)
	write(t, s.Target, "good jar")

	_, err := install(s, "/nonexistent/does-not-exist.jar", "v7-broken")
	if err == nil {
		t.Fatal("install should fail when the downloaded file is not there")
	}
	if got := read(t, s.Target); got != "good jar" {
		t.Fatalf("a failed install damaged the running artifact: %q", got)
	}
}

// Two deploys of the same service used to interleave: the second backed the
// *broken* jar up over the first one's backup, so the last known-good artifact
// was gone before either rollback ran.
func TestBackupPathIsUniquePerTag(t *testing.T) {
	s, dir := jarService(t)
	if backupPath(s, "v7-a1221b5") == backupPath(s, "v8-ab738d3") {
		t.Fatal("two tags must not share one backup path")
	}
	// A tag comes from GitHub, so it is not to be trusted as a filename.
	hostile := backupPath(s, "v7/../../etc")
	if filepath.Dir(hostile) != dir {
		t.Fatalf("a tag escaped the service directory: %q", hostile)
	}
}

func TestDeployLockSerializesOneService(t *testing.T) {
	if !lockService("svc-a") {
		t.Fatal("the first deploy should get the lock")
	}
	if lockService("svc-a") {
		t.Fatal("a second concurrent deploy of the same service must be refused")
	}
	if !lockService("svc-b") {
		t.Fatal("a different service must not be blocked")
	}
	unlockService("svc-a")
	unlockService("svc-b")

	if !lockService("svc-a") {
		t.Fatal("the lock should be reusable once released")
	}
	unlockService("svc-a")
}

// Hammer the lock the way the three callers do (webhook, poller, redeploy
// button) and assert that only one gets in at a time.
func TestDeployLockUnderContention(t *testing.T) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	inside, maxInside, admitted := 0, 0, 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !lockService("hot-svc") {
				return // a deploy is already running; the poller retries later
			}
			mu.Lock()
			inside++
			admitted++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()

			mu.Lock()
			inside--
			mu.Unlock()
			unlockService("hot-svc")
		}()
	}
	wg.Wait()

	if maxInside > 1 {
		t.Fatalf("%d deploys ran at once; the whole bug is that two can", maxInside)
	}
	if admitted == 0 {
		t.Fatal("nobody got in at all")
	}
}

// A release that failed must not be re-attempted on a timer.
func TestFailedTagIsRememberedAndCleared(t *testing.T) {
	s, _ := jarService(t)
	write(t, s.Target, "good jar")

	if failedTag(s) != "" {
		t.Fatal("a fresh service has no failed tag")
	}
	recordFailure(s, "v7-a1221b5")
	if got := failedTag(s); got != "v7-a1221b5" {
		t.Fatalf("failed tag not remembered: %q", got)
	}
	clearFailure(s)
	if failedTag(s) != "" {
		t.Fatal("clearFailure should let the tag be tried again")
	}
}

// If an earlier crash left no artifact, the next deploy puts one back before it
// does anything else — the unit is restart-looping in that state.
func TestSelfHealRestoresAnOrphanedBackup(t *testing.T) {
	s, dir := jarService(t)
	write(t, filepath.Join(dir, "app.jar.bak-v6-old"), "last good jar")
	// No app.jar at all — this is how the incident left the VPS.

	selfHeal(s)

	if !usable(s.Target, s.Kind) {
		t.Fatal("selfHeal should have restored the jar")
	}
	if got := read(t, s.Target); got != "last good jar" {
		t.Fatalf("restored the wrong thing: %q", got)
	}
}

func TestSelfHealLeavesAGoodArtifactAlone(t *testing.T) {
	s, dir := jarService(t)
	write(t, s.Target, "running jar")
	write(t, filepath.Join(dir, "app.jar.bak-v6-old"), "stale jar")

	selfHeal(s)

	if got := read(t, s.Target); got != "running jar" {
		t.Fatalf("selfHeal clobbered a healthy artifact: %q", got)
	}
}

// The static-site path (a directory) has the same invariant and none of the
// atomicity a file rename gives us, so it gets its own test.
func TestTarballRollbackWithMissingBackupKeepsTheTree(t *testing.T) {
	dir := t.TempDir()
	s := Service{Name: "site", Kind: KindTarball, Target: filepath.Join(dir, "www")}
	if err := os.MkdirAll(s.Target, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.Target, "index.html"), "<h1>live</h1>")

	if err := rollback(s, filepath.Join(dir, "www.bak-v9-gone")); err == nil {
		t.Fatal("rollback should report a missing backup tree")
	}
	if got := read(t, filepath.Join(s.Target, "index.html")); got != "<h1>live</h1>" {
		t.Fatalf("the live site was destroyed: %q", got)
	}
}

func TestUsableRejectsEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.jar")
	write(t, empty, "")
	full := filepath.Join(dir, "full.jar")
	write(t, full, "x")

	if usable(filepath.Join(dir, "nope.jar"), KindJar) {
		t.Fatal("a missing file is not usable")
	}
	if usable(empty, KindJar) {
		t.Fatal("a 0-byte jar is not usable — it is as fatal as a missing one")
	}
	if !usable(full, KindJar) {
		t.Fatal("a non-empty jar is usable")
	}

	emptyDir := filepath.Join(dir, "emptydir")
	os.MkdirAll(emptyDir, 0o755)
	if usable(emptyDir, KindTarball) {
		t.Fatal("an empty directory is not a site")
	}
}

// stubNotify captures what the deployer would have paged a human with.
func stubNotify(t *testing.T) *[]string {
	t.Helper()
	prev := notify
	var sent []string
	notify = func(title, message, tag, priority string) {
		sent = append(sent, title+" | "+message+" | "+priority)
	}
	t.Cleanup(func() { notify = prev })
	return &sent
}

// The fourth defect: the alert said "rolled back" even when the rollback had
// failed and the service was left with nothing. The one message you would rely
// on to tell you the VPS needs a human told you the opposite.
func TestUndoTellsTheTruthWhenTheRollbackFails(t *testing.T) {
	sent := stubNotify(t)
	s, _ := jarService(t)
	write(t, s.Target, "bad jar") // the broken deploy is live

	rollbackAttempt(s, "v7-a1221b5", "", "unhealthy") // no backup to restore

	if len(*sent) != 1 {
		t.Fatalf("expected exactly one alert, got %d", len(*sent))
	}
	msg := (*sent)[0]
	if !strings.HasPrefix(msg, "ROLLBACK FAILED") {
		t.Fatalf("a failed rollback must say so: %q", msg)
	}
	if !strings.Contains(msg, "max") {
		t.Fatalf("a service that may be down deserves max priority: %q", msg)
	}
	if got := read(t, s.Target); got != "bad jar" {
		t.Fatalf("undo destroyed the artifact: %q", got)
	}
}

func TestUndoReportsASuccessfulRollback(t *testing.T) {
	sent := stubNotify(t)
	s, _ := jarService(t)
	write(t, s.Target, "good jar")

	tmp := filepath.Join(t.TempDir(), "new.jar")
	write(t, tmp, "bad jar")
	backup, err := install(s, tmp, "v7-a1221b5")
	if err != nil {
		t.Fatal(err)
	}

	// A successful per-attempt rollback is silent — Deploy decides the verdict only
	// once the retries run out, and markFailed pages then.
	rollbackAttempt(s, "v7-a1221b5", backup, "unhealthy")
	if len(*sent) != 0 {
		t.Fatalf("a successful rollback should not page anyone on its own: %v", *sent)
	}
	if got := read(t, s.Target); got != "good jar" {
		t.Fatalf("the previous jar was not restored: %q", got)
	}

	markFailed(s, "v7-a1221b5", errors.New("unhealthy"))

	msg := (*sent)[0]
	if !strings.HasPrefix(msg, "Deploy failed") || strings.Contains(msg, "ROLLBACK FAILED") {
		t.Fatalf("giving up after a good rollback should report a plain deploy failure: %q", msg)
	}
	if failedTag(s) != "v7-a1221b5" {
		t.Fatal("the bad tag must be remembered so the poller stops retrying it")
	}
}

// The static-site path keeps its markers *inside* the directory it swaps, so
// recording the failure before the rollback would throw the record away with the
// tree — and the broken release would be retried on the next poll.
func TestFailedTarballDeployIsStillRememberedAfterRollback(t *testing.T) {
	stubNotify(t)
	dir := t.TempDir()
	s := Service{Name: "site", Kind: KindTarball, Target: filepath.Join(dir, "www")}

	// the live site, with the deployer's markers inside it
	os.MkdirAll(s.Target, 0o755)
	write(t, filepath.Join(s.Target, "index.html"), "<h1>live</h1>")
	write(t, deployedTagPath(s), "v1-good")

	// a backup tree, as install() would have parked it
	backup := backupPath(s, "v2-broken")
	os.MkdirAll(backup, 0o755)
	write(t, filepath.Join(backup, "index.html"), "<h1>live</h1>")
	write(t, filepath.Join(backup, ".deployed-tag"), "v1-good")

	rollbackAttempt(s, "v2-broken", backup, "unhealthy")
	markFailed(s, "v2-broken", errors.New("unhealthy"))

	if got := read(t, filepath.Join(s.Target, "index.html")); got != "<h1>live</h1>" {
		t.Fatalf("the site was not restored: %q", got)
	}
	if got := failedTag(s); got != "v2-broken" {
		t.Fatalf("the failure was lost with the swapped-out tree: %q — the poller would retry the broken release", got)
	}
}

// A normal service can be rolled back in-process, so after a good deploy every
// backup is dead weight and pruneBackups clears all of them.
func TestPruneBackupsRemovesAllForANormalService(t *testing.T) {
	s, _ := jarService(t) // no Unit -> not the self service
	write(t, backupPath(s, "v1"), "old-1")
	write(t, backupPath(s, "v2"), "old-2")

	pruneBackups(s)

	if got := backups(s); len(got) != 0 {
		t.Fatalf("expected all backups pruned for a normal service, still have %v", got)
	}
}

// vps-health cannot roll itself back, so pruneBackups must keep the newest backup
// (the last-known-good binary) as the only route home after a bad self-deploy,
// while still clearing older ones so they do not pile up.
func TestPruneBackupsKeepsTheLatestForSelf(t *testing.T) {
	dir := t.TempDir()
	s := Service{Name: "vps-health", Kind: KindBinary, Unit: "vps-health.service",
		Target: filepath.Join(dir, "vps-health")}

	older := backupPath(s, "v1")
	newer := backupPath(s, "v2")
	write(t, older, "good-v1")
	write(t, newer, "good-v2")

	// Make v2 unambiguously newer by mtime; hard-linked backups carry the install
	// time of the binary they copy, and each deploy installs a newer one.
	old := time.Now().Add(-1 * time.Hour)
	recent := time.Now()
	if err := os.Chtimes(older, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, recent, recent); err != nil {
		t.Fatal(err)
	}

	pruneBackups(s)

	if usable(older, KindBinary) {
		t.Fatalf("older backup should have been pruned")
	}
	if !usable(newer, KindBinary) {
		t.Fatalf("the last-known-good binary was destroyed on self-deploy — a bad self-deploy would restart-loop with nothing to restore")
	}
}
