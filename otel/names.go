package otel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// Session ids are long random strings, so the recent-replies list shows the
// name the session was given instead — "Health", "Sessions", "VPS". Only the
// session controller knows those names: they come from the /rename record in
// each session's own transcript, and it already publishes the mapping.
//
// A request straight to its loopback port carries no forwarded-proto header, so
// it counts as local and skips the sign-in wall. If the controller is down or
// slow we simply keep the last mapping we had and fall back to the short id —
// this is a nicety on a dashboard, never a reason to hold the page up.
const sessionsAPI = "http://127.0.0.1:8221/api/sessions"

type sessionListing struct {
	Sessions []struct {
		Title     string `json:"title"`
		SessionID string `json:"session_id"`
	} `json:"sessions"`
}

// RunSessionNames refreshes the id-to-name mapping on a ticker.
func (s *Store) RunSessionNames(interval time.Duration) {
	url := os.Getenv("SESSIONS_API")
	if url == "" {
		url = sessionsAPI
	}
	client := &http.Client{Timeout: 4 * time.Second}
	var lastErr string
	fetch := func() {
		names, err := fetchSessionNames(client, url)
		if err != nil {
			// Log a change of state only — the poll is frequent and a controller
			// restart would otherwise fill the journal with the same line.
			if e := err.Error(); e != lastErr {
				log.Printf("claude session names: %v (keeping the last mapping)", e)
				lastErr = e
			}
			return
		}
		lastErr = ""
		s.mergeNames(names)
	}
	fetch()
	for range time.Tick(interval) {
		fetch()
	}
}

// mergeNames folds a fresh listing into what we already know. It merges rather
// than replaces because the controller only lists the most recent thirty
// sessions, and a reply from one that has since dropped off should keep the
// name we already learned for it. A rename still wins — the fresh listing is
// written over the old entry. The reset is only a backstop so the map cannot
// grow forever.
func (s *Store) mergeNames(names map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil || len(s.names) > 1000 {
		s.names = make(map[string]string, len(names))
	}
	for id, name := range names {
		s.names[id] = name
	}
}

func fetchSessionNames(client *http.Client, url string) (map[string]string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var listing sessionListing
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(listing.Sessions))
	for _, sess := range listing.Sessions {
		if sess.SessionID != "" && sess.Title != "" {
			names[sess.SessionID] = sess.Title
		}
	}
	return names, nil
}
