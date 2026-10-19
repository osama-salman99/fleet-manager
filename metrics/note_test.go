package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadNote(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "note.txt")
	if got := readNote(p); got != "" {
		t.Fatalf("missing file: got %q, want empty", got)
	}
	if err := os.WriteFile(p, []byte("  checked 10:00 · no better deal \nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readNote(p); got != "checked 10:00 · no better deal" {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat("é", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readNote(p); len([]rune(got)) != 80 {
		t.Fatalf("long note not cut to 80 runes: %d", len([]rune(got)))
	}
}
