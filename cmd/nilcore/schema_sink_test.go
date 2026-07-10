package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/schema"
	"nilcore/internal/eventlog"
)

// The report's SchemaDefects section was permanently empty because nothing ever emitted
// a schema_verify event: SchemaVerifier.EventSink was never set at either construction
// site. This pins the PRODUCER half — the sink must serialize the event into exactly the
// Detail shape the report decoder reads: {"id","kind","defects":[{code,field,claim_id,
// reason}],"passed"}.
func TestEvidenceEventSinkEmitsSchemaVerifyInDecodableShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}

	sink := evidenceEventSink(log)
	if sink == nil {
		t.Fatal("evidenceEventSink returned nil for a non-nil log")
	}
	sink(schema.SchemaVerifyEvent{
		ArtifactID: "rpt-1",
		Kind:       artifact.Kind("report"),
		Passed:     false,
		Defects: []schema.DefectMeta{{
			Code: "citation_required", Field: "claims[0].source_url",
			ClaimID: "c1", Reason: "missing citation",
		}},
	})
	if err := log.Err(); err != nil {
		t.Fatalf("log write: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("log close: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct {
			Kind   string         `json:"kind"`
			Detail map[string]any `json:"detail"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Kind == schema.EventKind {
			found = e.Detail
		}
	}
	if found == nil {
		t.Fatalf("no %q event in the log — the report's SchemaDefects section stays empty", schema.EventKind)
	}

	if found["id"] != "rpt-1" {
		t.Errorf(`Detail["id"] = %v, want "rpt-1"`, found["id"])
	}
	if passed, _ := found["passed"].(bool); passed {
		t.Error(`Detail["passed"] must be false for a defective artifact`)
	}
	defects, ok := found["defects"].([]any)
	if !ok || len(defects) != 1 {
		t.Fatalf(`Detail["defects"] = %#v, want one defect`, found["defects"])
	}
	d, ok := defects[0].(map[string]any)
	if !ok {
		t.Fatalf("defect is %T, want an object", defects[0])
	}
	for k, want := range map[string]string{
		"code":     "citation_required",
		"field":    "claims[0].source_url",
		"claim_id": "c1",
		"reason":   "missing citation",
	} {
		if got, _ := d[k].(string); got != want {
			t.Errorf("defect[%q] = %q, want %q (the report decoder reads this exact key)", k, got, want)
		}
	}
}
