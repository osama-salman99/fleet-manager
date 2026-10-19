// Package canary watches the portal APIs' published OpenAPI/Swagger specs and
// pages a human when the contract changes underneath us — a new required
// parameter, a dropped endpoint — before it breaks a live application.
//
// It exists because on 2026-07-21 the ReletPortal added a required
// `confirm_finalization` field to PUT /finalize with no warning; the first we
// knew of it was a 400 on a real auto-apply. Each OSRE microservice publishes a
// /swagger.json, so we can diff it on a schedule instead of finding out the hard
// way. (The MVGM and Statamic portals publish no spec, so they are not here.)
package canary

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/osama-salman99/fleet-manager/ntfy"
)

const (
	baselineDir  = "/opt/vps-health/contracts"
	checkEvery   = 24 * time.Hour
	firstDelay   = 45 * time.Second
	fetchTimeout = 20 * time.Second
	maxMsgLen    = 1500
)

// specs are the OpenAPI docs worth watching, keyed by a short label used in the
// alert title and the baseline filename.
var specs = map[string]string{
	"relet":          "https://relet.portal.prd.osre.eu/swagger.json",
	"authentication": "https://authentication.portal.prd.osre.eu/swagger.json",
	"entree":         "https://entree.portal.prd.osre.eu/swagger.json",
	"file":           "https://file.portal.prd.osre.eu/swagger.json",
}

// Run establishes baselines shortly after boot, then re-checks daily. Contracts
// change on the order of days at most, so a tight interval would only add noise.
func Run() {
	time.Sleep(firstDelay)
	checkAll()
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for range ticker.C {
		checkAll()
	}
}

func checkAll() {
	if err := os.MkdirAll(baselineDir, 0o755); err != nil {
		log.Printf("canary: cannot create %s: %v", baselineDir, err)
		return
	}
	for name, url := range specs {
		checkOne(name, url)
	}
}

func checkOne(name, url string) {
	doc, err := fetchSpec(url)
	if err != nil {
		// A transient fetch or parse failure is not a contract change. Stay quiet
		// so the signal only ever fires on a real, successfully-compared diff.
		log.Printf("canary: %s skipped: %v", name, err)
		return
	}

	path := filepath.Join(baselineDir, name+".json")
	prev, err := os.ReadFile(path)
	if err != nil {
		writeBaseline(path, doc)
		log.Printf("canary: %s baseline established (%d operations tracked)", name, len(signatures(doc)))
		return
	}

	var old map[string]any
	if json.Unmarshal(prev, &old) != nil {
		writeBaseline(path, doc)
		return
	}

	added, removed := diffSignatures(old, doc)
	if len(added) == 0 && len(removed) == 0 {
		// Hash-level noise (reordering, descriptions) with no contract impact —
		// refresh the baseline silently so it does not keep re-triggering.
		writeBaseline(path, doc)
		return
	}

	msg := formatDelta(added, removed)
	log.Printf("canary: %s contract changed:\n%s", name, msg)
	ntfy.Send("canary", fmt.Sprintf("Portal contract changed: %s", name), msg, "warning", "high")
	writeBaseline(path, doc)
}

func writeBaseline(path string, doc map[string]any) {
	// Store the canonical spec so the next run can diff structurally, not just
	// against a hash. json.Marshal sorts object keys, giving a stable form.
	data, err := json.Marshal(doc)
	if err != nil {
		log.Printf("canary: marshal baseline %s: %v", path, err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("canary: write baseline %s: %v", path, err)
	}
}

func fetchSpec(url string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		return nil, fmt.Errorf("no paths object (not a spec?)")
	}
	return doc, nil
}

// signatures reduces a spec to the set of contract facts that actually matter to
// a caller: which endpoints exist, and for each operation which parameters it
// takes and which are required (body fields resolved through $ref). A change to
// this set is a change we could break on; anything else (descriptions, examples,
// ordering) is not, and never reaches a human.
func signatures(doc map[string]any) map[string]bool {
	sigs := map[string]bool{}
	paths, _ := doc["paths"].(map[string]any)
	defs, _ := doc["definitions"].(map[string]any)

	for p, pv := range paths {
		sigs["endpoint "+p] = true
		ops, ok := pv.(map[string]any)
		if !ok {
			continue
		}
		for method, ov := range ops {
			op, ok := ov.(map[string]any)
			if !ok {
				continue
			}
			m := strings.ToUpper(method)
			params, _ := op["parameters"].([]any)
			for _, prm := range params {
				pm, ok := prm.(map[string]any)
				if !ok {
					continue
				}
				in, _ := pm["in"].(string)
				name, _ := pm["name"].(string)
				req, _ := pm["required"].(bool)
				if in == "body" {
					schema, _ := pm["schema"].(map[string]any)
					for _, f := range schemaRequired(schema, defs) {
						sigs[fmt.Sprintf("%s %s body.%s (required)", m, p, f)] = true
					}
					continue
				}
				sigs[fmt.Sprintf("%s %s %s (in=%s, required=%t)", m, p, name, in, req)] = true
			}
		}
	}
	return sigs
}

// schemaRequired returns the required field names of a body parameter's schema,
// following a single $ref into definitions if present.
func schemaRequired(schema, defs map[string]any) []string {
	if schema == nil {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		name := ref[strings.LastIndex(ref, "/")+1:]
		if d, ok := defs[name].(map[string]any); ok {
			return requiredList(d)
		}
		return nil
	}
	return requiredList(schema)
}

func requiredList(m map[string]any) []string {
	raw, ok := m["required"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func diffSignatures(old, cur map[string]any) (added, removed []string) {
	o := signatures(old)
	n := signatures(cur)
	for s := range n {
		if !o[s] {
			added = append(added, s)
		}
	}
	for s := range o {
		if !n[s] {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func formatDelta(added, removed []string) string {
	var b strings.Builder
	for _, a := range added {
		b.WriteString("+ " + a + "\n")
	}
	for _, r := range removed {
		b.WriteString("- " + r + "\n")
	}
	msg := strings.TrimRight(b.String(), "\n")
	if len(msg) > maxMsgLen {
		msg = msg[:maxMsgLen] + "\n… (truncated)"
	}
	return msg
}

// ntfy sending (with the ASCII-Title fold) now lives in the shared ntfy package.
