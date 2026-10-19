package metrics

import (
	"os"
	"testing"
)

func TestClassifyProc(t *testing.T) {
	gradle := []string{"/opt/java/openjdk/bin/java", "-Xmx512m", "-cp",
		"/home/coder/.gradle/wrapper/dists/gradle-8.12/lib/gradle-daemon-main-8.12.jar",
		"org.gradle.launcher.daemon.bootstrap.GradleDaemon", "8.12"}

	cases := []struct {
		name string
		argv []string
		want string
	}{
		// The daemon is a java process, and must not be filed as one: it is the
		// single thing on this VPS most worth naming out loud.
		{"gradle daemon", gradle, "Gradle daemon"},
		{"plain jvm", []string{"/opt/java/openjdk/bin/java", "-jar", "app.jar"}, "Java"},
		{"claude session", []string{"claude", "--dangerously-skip-permissions", "--resume", "abc"}, "Claude sessions"},
		{"coder agent", []string{"./coder", "agent", "--no-reap"}, "Coder agent"},
		{"tmux server", []string{"tmux", "new-session", "-d", "-s", "remote-1"}, "tmux"},
		{"node", []string{"node", "server.js"}, "Node"},
		{"shell", []string{"/bin/bash", "-c", "ls"}, "Shells & tools"},
		// climothee sits in a tmux command line and starts with "cl" — it must not
		// be swept up as a Claude session and inflate the count.
		{"lookalike", []string{"climothee", "--resume", "d36bb083"}, "Shells & tools"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyProc(c.argv); got != c.want {
				t.Errorf("classifyProc(%v) = %q, want %q", c.argv, got, c.want)
			}
		})
	}
}

// The breakdown is drawn as segments of the card's memory figure, so the one thing
// it must never do is total more than that figure. Run it against whatever cgroup
// this test happens to be inside — the numbers vary, the invariant does not.
func TestBreakdownFitsInsideTheWhole(t *testing.T) {
	const scope = "/sys/fs/cgroup"
	if _, err := os.Stat(scope + "/cgroup.procs"); err != nil {
		t.Skip("no cgroup v2 here")
	}

	stat := readCgroupStat(scope)
	total := float64(stat["anon"]+stat["slab"]) / 1024 / 1024
	if total == 0 {
		t.Skip("cgroup reports no memory")
	}

	groups := readBreakdown(scope, total)
	if len(groups) == 0 {
		t.Fatal("no processes attributed in a cgroup that is using memory")
	}

	var sum float64
	for i, g := range groups {
		if g.MemMB < 0 {
			t.Errorf("%s has negative memory: %.1f", g.Name, g.MemMB)
		}
		if i > 0 && groups[i-1].MemMB < g.MemMB {
			t.Errorf("not sorted: %s (%.1f) before %s (%.1f)",
				groups[i-1].Name, groups[i-1].MemMB, g.Name, g.MemMB)
		}
		sum += g.MemMB
		t.Logf("%-20s %8.1f MB  ×%d", g.Name, g.MemMB, g.Count)
	}
	t.Logf("%-20s %8.1f MB (cgroup anon+slab: %.1f MB)", "SUM", sum, total)

	// The remainder closes the gap exactly, so the parts should land on the whole.
	// Processes come and go between the two reads, so allow a little slack.
	if sum > total*1.05 {
		t.Errorf("breakdown sums to %.1f MB, more than the %.1f MB it divides", sum, total)
	}
}
