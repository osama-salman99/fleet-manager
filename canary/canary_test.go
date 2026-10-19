package canary

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mustSpec(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test spec: %v", err)
	}
	return m
}

// The exact regression: a body field becomes required (confirm_finalization),
// resolved through a $ref into definitions. It must show up as an addition.
func TestDiffCatchesNewRequiredBodyField(t *testing.T) {
	const shape = `{
	  "paths": { "/portal/applications/{id}/finalize": { "put": { "parameters": [
	    {"in":"body","name":"body","schema":{"$ref":"#/definitions/Finalize"}}
	  ]}}},
	  "definitions": { "Finalize": { "required": [%s] } }
	}`
	old := mustSpec(t, strings.Replace(shape, "%s", `"application"`, 1))
	cur := mustSpec(t, strings.Replace(shape, "%s", `"application","confirm_finalization"`, 1))

	added, removed := diffSignatures(old, cur)
	if len(removed) != 0 {
		t.Fatalf("unexpected removals: %v", removed)
	}
	if !strings.Contains(strings.Join(added, "\n"), "confirm_finalization") {
		t.Fatalf("expected confirm_finalization in additions, got: %v", added)
	}
}

// A description/summary edit is not a contract change and must not page anyone.
func TestCosmeticChangeIsIgnored(t *testing.T) {
	a := mustSpec(t, `{"paths":{"/x":{"get":{"summary":"old","parameters":[{"in":"query","name":"q","required":true}]}}}}`)
	b := mustSpec(t, `{"paths":{"/x":{"get":{"summary":"NEW","parameters":[{"in":"query","name":"q","required":true}]}}}}`)
	added, removed := diffSignatures(a, b)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("cosmetic change should not diff: added=%v removed=%v", added, removed)
	}
}

// A removed endpoint and a required-ness flip must both surface.
func TestDiffCatchesEndpointAndRequiredFlip(t *testing.T) {
	a := mustSpec(t, `{"paths":{"/a":{"get":{"parameters":[{"in":"query","name":"q","required":false}]}},"/b":{"get":{}}}}`)
	b := mustSpec(t, `{"paths":{"/a":{"get":{"parameters":[{"in":"query","name":"q","required":true}]}}}}`)
	added, removed := diffSignatures(a, b)
	joinedR := strings.Join(removed, "\n")
	if !strings.Contains(joinedR, "endpoint /b") {
		t.Fatalf("expected removed endpoint /b, got removed=%v", removed)
	}
	if !strings.Contains(strings.Join(added, "\n"), "required=true") {
		t.Fatalf("expected q required=true addition, got added=%v", added)
	}
}

// Guard against the parser silently missing the real spec's shape: the live
// relet spec's finalize operation must yield the confirm_finalization signature.
func TestRealReletSpecExtractsConfirmFinalization(t *testing.T) {
	data, err := os.ReadFile("testdata/relet-swagger.json")
	if err != nil {
		t.Skip("no testdata spec available")
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse real spec: %v", err)
	}
	sigs := signatures(doc)
	for s := range sigs {
		if strings.Contains(s, "finalize") && strings.Contains(s, "confirm_finalization") {
			return
		}
	}
	t.Fatal("real relet spec: finalize confirm_finalization signature was not extracted")
}
