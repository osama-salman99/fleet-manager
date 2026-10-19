package ntfy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAsciiHeader(t *testing.T) {
	cases := []struct{ in, want string }{
		// plain ASCII is untouched
		{"Deploy failed", "Deploy failed"},
		{"ROLLBACK FAILED", "ROLLBACK FAILED"},
		// the typographic characters that actually show up in alert text
		{"Deploy failed — roofz", "Deploy failed - roofz"},
		{"naar → van", "naar -> van"},
		{"“quoted” ‘title’", "\"quoted\" 'title'"},
		{"Retrying…", "Retrying..."},
		// unmappable → removed, and the whitespace it leaves is collapsed
		{"Deploy 🚀 failed", "Deploy failed"},
		{"café façade", "caf faade"},
		// non-breaking space folds to a normal space (and collapses)
		{"a  b", "a b"},
		{"", ""},
	}
	for _, c := range cases {
		got := asciiHeader(c.in)
		if got != c.want {
			t.Errorf("asciiHeader(%q) = %q, want %q", c.in, got, c.want)
		}
		for _, r := range got {
			if r >= 128 {
				t.Errorf("asciiHeader(%q) = %q still contains a non-ASCII rune %q", c.in, got, r)
			}
		}
	}
}

// TestSendFoldsTitleHeader is the real regression guard: it proves Send puts an
// ASCII-only Title on the wire. ntfy silently drops a whole push whose Title
// header carries a non-ASCII byte, so a raw Title reaching the request is the
// exact fleet-wide bug this package exists to make impossible.
func TestSendFoldsTitleHeader(t *testing.T) {
	var gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTitle = r.Header.Get("Title")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	oldURL := URL
	URL = srv.URL
	defer func() { URL = oldURL }()

	Send("test", "Service Down — vps-health 🚨", "body", "rotating_light", "high")

	if gotTitle != "Service Down - vps-health" {
		t.Fatalf("Title on the wire = %q, want the ASCII-folded form", gotTitle)
	}
	for _, r := range gotTitle {
		if r >= 128 {
			t.Fatalf("Title on the wire %q still carries a non-ASCII rune %q", gotTitle, r)
		}
	}
}
