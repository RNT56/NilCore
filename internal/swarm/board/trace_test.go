package board

// trace_test.go pins the I7/I3 trust boundary of the trace projection: Traces carries
// the verifier-set verdict fields and the key-free SourceURL (the audit anchor) and
// NOTHING model-authored. The structural guarantee is that there is no Value field to
// leak in the first place — this test asserts it stays that way and that the trusted
// fields are projected faithfully.

import (
	"reflect"
	"testing"
)

// TestTracesProjectTrustedFields asserts every trusted field (Status/Detail/Verifier +
// the key-free SourceURL) is carried through from the snapshot's shard rows, in shard-id
// order.
func TestTracesProjectTrustedFields(t *testing.T) {
	snap := Snapshot{
		Shards: []ShardRow{
			{ID: "b", Pass: 2, Passed: true, Status: "pass", Detail: "ok", Verifier: "vk", SourceURL: "https://example.com/b"},
			{ID: "a", Pass: 1, Passed: false, Status: "fail", Detail: "mismatch", Verifier: "vk", SourceURL: "https://example.com/a"},
		},
	}
	tr := Traces(snap)
	if len(tr) != 2 {
		t.Fatalf("traces = %d, want 2", len(tr))
	}
	// Sorted by shard id: a before b.
	if tr[0].Shard != "a" || tr[1].Shard != "b" {
		t.Fatalf("traces not sorted by shard: %q,%q", tr[0].Shard, tr[1].Shard)
	}
	a := tr[0]
	if a.Status != "fail" || a.Detail != "mismatch" || a.Verifier != "vk" || a.SourceURL != "https://example.com/a" {
		t.Fatalf("trace a projected wrong trusted fields: %+v", a)
	}
	if a.Passed {
		t.Fatalf("trace a Passed=true, want false")
	}
}

// TestTraceCarriesNoModelValue is the I7 structural guarantee: the Trace type has NO
// field that could carry a model-authored Value/Statement/ExtractionMethod. We assert it
// by reflection over the type's fields, so adding such a field would FAIL the build's
// intent here — the trace is trusted-fields-only by construction.
func TestTraceCarriesNoModelValue(t *testing.T) {
	rt := reflect.TypeOf(Trace{})
	banned := map[string]bool{
		"Value":            true,
		"Statement":        true,
		"ExtractionMethod": true,
	}
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if banned[name] {
			t.Fatalf("Trace carries a model-authored field %q — I7 forbids projecting it", name)
		}
	}
	// The same structural guarantee for ShardRow, the snapshot row the trace reads from.
	rr := reflect.TypeOf(ShardRow{})
	for i := 0; i < rr.NumField(); i++ {
		if banned[rr.Field(i).Name] {
			t.Fatalf("ShardRow carries a model-authored field %q — I7 forbids it on the snapshot", rr.Field(i).Name)
		}
	}
}

// TestTracesKeyFreeSourceURLPresent asserts the SourceURL provenance IS projected (it is
// the audit anchor, required key-free by I3) — the trace would be useless without it.
func TestTracesKeyFreeSourceURLPresent(t *testing.T) {
	snap := Snapshot{Shards: []ShardRow{{ID: "x", Status: "pass", SourceURL: "https://example.com/x"}}}
	tr := Traces(snap)
	if len(tr) != 1 || tr[0].SourceURL != "https://example.com/x" {
		t.Fatalf("SourceURL not projected: %+v", tr)
	}
}

// TestTracesEmpty asserts an empty snapshot yields an empty (non-nil-panicking) trace
// slice.
func TestTracesEmpty(t *testing.T) {
	if tr := Traces(Snapshot{}); len(tr) != 0 {
		t.Fatalf("empty snapshot yielded %d traces, want 0", len(tr))
	}
}
