package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGradePressure(t *testing.T) {
	cases := []struct {
		name       string
		some, full float64
		want       string
	}{
		{"idle", 0, 0, "calm"},
		{"a whisper stays calm", 0.4, 0, "calm"},
		{"just over the floor", 1.0, 0, "light"},
		{"clearly light", 6, 0, "light"},
		{"some crosses into medium", 10, 0, "medium"},
		{"light some but sustained full escalates", 5, 2, "medium"},
		{"some crosses into heavy", 40, 5, "heavy"},
		{"full pins it to heavy below the some line", 30, 10, "heavy"},
	}
	for _, c := range cases {
		if got := gradePressure(c.some, c.full); got != c.want {
			t.Errorf("%s: gradePressure(%.1f, %.1f) = %q, want %q", c.name, c.some, c.full, got, c.want)
		}
	}
}

func TestReadPressure(t *testing.T) {
	// The exact shape the kernel writes to memory.pressure: two lines, four fields
	// each, avg60 in the middle. The grader must pull avg60 (12.34), not avg10.
	const psi = "some avg10=99.00 avg60=12.34 avg300=3.00 total=123456789\n" +
		"full avg10=50.00 avg60=3.50 avg300=1.00 total=98765432\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.pressure"), []byte(psi), 0o644); err != nil {
		t.Fatal(err)
	}

	p := readPressure(dir, "memory.pressure")
	if p == nil {
		t.Fatal("readPressure returned nil for a file that exists")
	}
	if p.Some != 12.34 || p.Full != 3.50 {
		t.Errorf("parsed some=%.2f full=%.2f, want some=12.34 full=3.50", p.Some, p.Full)
	}
	// some=12.34 → medium band; full=3.50 would also reach medium. Either way: medium.
	if p.Level != "medium" {
		t.Errorf("graded %q, want medium", p.Level)
	}

	// A cgroup or kernel that does not expose the file must read as nil, not as a
	// confident calm — the caller leaves the metric off entirely in that case.
	if readPressure(dir, "does-not-exist.pressure") != nil {
		t.Error("readPressure should return nil for a missing file")
	}
}
