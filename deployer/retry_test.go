package deployer

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubDeployOnce replaces the single-attempt worker and the backoff schedule so a
// test can drive Deploy's retry loop deterministically and instantly.
func stubDeployOnce(t *testing.T, attempt func(n int) error) *int {
	t.Helper()
	prevFn, prevDelays := deployOnce, retryDelays
	// Same number of retries, none of the waiting.
	zero := make([]time.Duration, len(prevDelays))
	retryDelays = zero
	var calls int
	deployOnce = func(s Service, tag string) error {
		calls++
		return attempt(calls)
	}
	t.Cleanup(func() { deployOnce, retryDelays = prevFn, prevDelays })
	return &calls
}

// A jar service whose markers live in a temp dir, so Deploy's on-disk bookkeeping
// (deployed-tag / failed-tag) has somewhere real but throwaway to write.
func tempJarService(t *testing.T) Service {
	t.Helper()
	dir := t.TempDir()
	return Service{Name: "svc-" + filepath.Base(dir), Target: filepath.Join(dir, "app.jar"), Kind: KindJar}
}

func TestDeployRetriesUntilOneAttemptSucceeds(t *testing.T) {
	stubNotify(t)
	s := tempJarService(t)
	// Fail the first three attempts, then come up on the fourth.
	calls := stubDeployOnce(t, func(n int) error {
		if n < 4 {
			return errors.New("out of memory")
		}
		return nil
	})

	if err := Deploy(s, "v9-abc1234"); err != nil {
		t.Fatalf("a deploy that eventually comes up must report success: %v", err)
	}
	if *calls != 4 {
		t.Fatalf("expected 4 attempts (3 retries), got %d", *calls)
	}
	if deployedTag(s) != "v9-abc1234" {
		t.Fatalf("a successful deploy must record its tag, got %q", deployedTag(s))
	}
	if failedTag(s) != "" {
		t.Fatal("success must clear any prior failure marker")
	}
}

func TestDeployExhaustsInDeployRetriesThenSchedulesAutoRetry(t *testing.T) {
	sent := stubNotify(t)
	s := tempJarService(t)
	calls := stubDeployOnce(t, func(int) error { return errors.New("out of memory") })

	err := Deploy(s, "v9-bad0000")
	if err == nil {
		t.Fatal("a deploy that never comes up must report failure")
	}
	// One initial try plus every in-deploy retry in the schedule.
	if want := len(retryDelays) + 1; *calls != want {
		t.Fatalf("expected %d attempts, got %d", want, *calls)
	}
	// The tag is remembered, but as a first failure the poller will still auto-retry
	// it on a backoff — not a permanent give-up yet.
	rec, ok := readFailure(s)
	if !ok || rec.Tag != "v9-bad0000" {
		t.Fatalf("the bad tag must be remembered, got %+v", rec)
	}
	if rec.Attempts != 1 || rec.permanent() {
		t.Fatalf("first failure should schedule an auto-retry, not give up: %+v", rec)
	}
	if rec.NextRetry.IsZero() || rec.NextRetry.Before(time.Now()) {
		t.Fatalf("first failure must schedule a future retry, got %v", rec.NextRetry)
	}
	if len(*sent) != 1 {
		t.Fatalf("a failed cycle must page exactly once, not once per attempt: %d pages", len(*sent))
	}
}

// A release that keeps failing is retried a bounded number of times and only then
// left permanently for a human — and it pages once at the start and once at the
// end, never for the quiet retries in between.
func TestMarkFailedEscalatesToPermanent(t *testing.T) {
	sent := stubNotify(t)
	s := tempJarService(t)

	total := len(pollRetryBackoff) + 1 // initial + every auto-retry
	for i := 1; i <= total; i++ {
		markFailed(s, "v3-bad", errors.New("oom"))
		rec, _ := readFailure(s)
		if rec.Attempts != i {
			t.Fatalf("cycle %d: attempts=%d", i, rec.Attempts)
		}
		last := i == total
		if rec.permanent() != last {
			t.Fatalf("cycle %d: permanent=%v (want %v)", i, rec.permanent(), last)
		}
		if last && !rec.NextRetry.IsZero() {
			t.Fatalf("a permanent failure must not schedule another retry: %v", rec.NextRetry)
		}
	}
	// First failure and the final give-up page; the middle retries stay quiet.
	if len(*sent) != 2 {
		t.Fatalf("expected exactly 2 pages (first + give-up), got %d: %v", len(*sent), *sent)
	}
	if !strings.Contains((*sent)[len(*sent)-1], "max") {
		t.Fatalf("the final give-up must page at max priority: %q", (*sent)[len(*sent)-1])
	}
	// A newer release resets the count: a fresh tag is a first failure again.
	markFailed(s, "v4-new", errors.New("oom"))
	if rec, _ := readFailure(s); rec.Attempts != 1 || rec.permanent() {
		t.Fatalf("a newer tag must start over, got %+v", rec)
	}
}

func TestDeployRefusesToOverlapItself(t *testing.T) {
	stubNotify(t)
	s := tempJarService(t)

	// Hold the service's deploy lock, then confirm a second Deploy bails immediately
	// instead of running a competing attempt.
	if !lockService(s.Name) {
		t.Fatal("could not take the deploy lock")
	}
	defer unlockService(s.Name)

	var ran sync.WaitGroup
	ran.Add(1)
	var err error
	go func() { defer ran.Done(); err = Deploy(s, "v1-xxxxxxx") }()
	ran.Wait()

	if err == nil {
		t.Fatal("a second concurrent deploy must be refused, not run")
	}
	if got := deployedTag(s); got != "" {
		t.Fatalf("the refused deploy must not have installed anything, got %q", got)
	}
}

func TestDeployWaitsForAnotherServiceThenRuns(t *testing.T) {
	stubNotify(t)
	s := tempJarService(t)

	// Another service is mid-deploy (the fleet gate is held). A deploy of *this*
	// service must neither start a second JVM alongside it nor be dropped: it waits,
	// and goes ahead the moment the other one finishes.
	calls := stubDeployOnce(t, func(int) error { return nil })
	if !lockFleetDeploy() {
		t.Fatal("could not take the fleet deploy lock")
	}
	done := make(chan error, 1)
	go func() { done <- Deploy(s, "v1-xxxxxxx") }()

	select {
	case err := <-done:
		unlockFleetDeploy()
		t.Fatalf("deploy returned while another service was deploying: %v", err)
	case <-time.After(600 * time.Millisecond):
	}
	if *calls != 0 {
		t.Fatal("the deploy started while another service was deploying")
	}

	unlockFleetDeploy()
	select {
	case err := <-done:
		if err != nil || *calls != 1 || deployedTag(s) != "v1-xxxxxxx" {
			t.Fatalf("deploy should have run once the gate freed: err=%v calls=%d tag=%q", err, *calls, deployedTag(s))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deploy never started after the other one finished")
	}
}

func TestDeployGivesUpWaitingAfterFleetWait(t *testing.T) {
	stubNotify(t)
	s := tempJarService(t)
	prev := fleetWait
	fleetWait = 300 * time.Millisecond
	t.Cleanup(func() { fleetWait = prev })

	// A stalled deploy elsewhere must not hold this one forever.
	calls := stubDeployOnce(t, func(int) error { return nil })
	if !lockFleetDeploy() {
		t.Fatal("could not take the fleet deploy lock")
	}
	defer unlockFleetDeploy()
	err := Deploy(s, "v1-xxxxxxx")
	if err == nil || !strings.Contains(err.Error(), "another service is deploying") {
		t.Fatalf("want a give-up after fleetWait, got %v", err)
	}
	if *calls != 0 {
		t.Fatal("the skipped deploy must not have run")
	}
}
