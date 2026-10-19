// Package ntfy is the one place the fleet's ntfy alerts are sent from.
//
// It exists because three near-identical copies of this POST used to live in the
// canary, watchdog and deployer packages, and none of them folded the Title
// header to ASCII. ntfy delivers a non-ASCII Title via a path that silently
// DROPS the entire message (the Java NtfyClient and the host watch scripts all
// fold it too). No call site trips it today because every current
// Title is a static ASCII string, but the first time anyone interpolates a unit
// name, tag or arrow into a Title, that alert would have vanished with no error.
// Folding in one shared Send makes the whole class of bug structurally
// impossible and is covered by ntfy_test.go.
package ntfy

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// URL is the ntfy topic every fleet alert posts to, from the NTFY_URL setting.
// Anyone who knows a topic can read and post to it, so it is never written in the
// code. Left empty, alerts are only logged. A var so a test can point Send at an
// httptest server.
var URL = os.Getenv("NTFY_URL")

// client has a deadline so a hung ntfy POST can't stall a caller's loop (the
// watchdog ticker, the deploy path). A var so a test can swap it if needed.
var client = &http.Client{Timeout: 10 * time.Second}

// Send POSTs message to ntfy with the given Title/Tags/Priority. logPrefix names
// the source package so an error line is still attributable after unification.
// Errors are logged, never fatal — a dropped alert must not take down the caller.
func Send(logPrefix, title, message, tag, priority string) {
	if URL == "" {
		log.Printf("%s: alert (no NTFY_URL set): %s: %s", logPrefix, title, message)
		return
	}
	req, err := http.NewRequest("POST", URL, strings.NewReader(message))
	if err != nil {
		log.Printf("%s: ntfy request error: %v", logPrefix, err)
		return
	}
	req.Header.Set("Title", asciiHeader(title))
	req.Header.Set("Tags", tag)
	req.Header.Set("Priority", priority)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("%s: ntfy send error: %v", logPrefix, err)
		return
	}
	resp.Body.Close()
}

// asciiHeader folds title to pure ASCII so ntfy can't silently drop the push.
// It maps the typographic characters that actually show up in alert text
// (dashes, arrows, smart quotes, ellipsis, non-breaking space) to ASCII, drops
// anything still non-ASCII (matching the Java NtfyClient's "unmappable ->
// removed" behavior), and collapses the whitespace those removals can leave.
func asciiHeader(title string) string {
	title = headerFolder.Replace(title)
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var headerFolder = strings.NewReplacer(
	"—", "-", "–", "-", "―", "-", "‑", "-", // em / en / horizontal-bar / non-breaking hyphen
	"→", "->", "←", "<-",
	"“", "\"", "”", "\"", "‘", "'", "’", "'",
	"…", "...",
	" ", " ", // non-breaking space
)
