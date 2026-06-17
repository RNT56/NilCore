package super

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/integrate"
)

// handle is a tiny constructor for a spawned node with its deps, for the snapshot tests.
func nodeHandle(id string, deps ...string) *Handle {
	return &Handle{Spec: SubagentSpec{ID: id, DependsOn: deps}}
}

// recordIntegration must accumulate per-node disposition ACROSS waves (the load-bearing
// property: a node merged in wave 1 stays merged when wave 2 integrates), record the
// final verified tip SHA, and hand SaveState a deterministic snapshot.
func TestRecordIntegrationAccumulatesAcrossWaves(t *testing.T) {
	ctx := context.Background()
	var saved []Snapshot
	s := &Supervisor{
		Log:       nil,
		SaveState: func(_ context.Context, snap Snapshot) error { saved = append(saved, snap); return nil },
	}
	st := &runState{
		handles: map[string]*Handle{
			"t1": nodeHandle("t1"),
			"t2": nodeHandle("t2", "t1"),
			"t3": nodeHandle("t3", "t1"),
		},
		nodeStates: map[string]string{},
	}

	// Wave 1: t1 merges+verifies green. t2/t3 not yet attempted ⇒ still pending.
	s.recordIntegration(ctx, st, []integrate.MergeResult{
		{ID: "t1", Merged: true, Verified: true, SHA: "sha-after-t1"},
	})
	// Wave 2: t2 merges green; t3 conflicts (rolled back) ⇒ failed. The last result's
	// SHA is the current tip (t3 rolled back to t2's tip, so SHA == that tip).
	s.recordIntegration(ctx, st, []integrate.MergeResult{
		{ID: "t2", Merged: true, Verified: true, SHA: "sha-after-t2"},
		{ID: "t3", Merged: false, Verified: false, Conflict: true, SHA: "sha-after-t2"},
	})

	if len(saved) != 2 {
		t.Fatalf("SaveState called %d times, want 2 (once per integrate)", len(saved))
	}
	final := saved[len(saved)-1]

	// The tip is the last verified integration.
	if final.TipSHA != "sha-after-t2" {
		t.Errorf("TipSHA = %q, want sha-after-t2", final.TipSHA)
	}

	// t1 (merged in WAVE 1) is STILL merged after wave 2 — the accumulation guarantee.
	want := map[string]string{"t1": "merged", "t2": "merged", "t3": "failed"}
	got := map[string]string{}
	deps := map[string][]string{}
	for _, n := range final.Nodes {
		got[n.ID] = n.State
		deps[n.ID] = n.DependsOn
	}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("node %s state = %q, want %q", id, got[id], st)
		}
	}
	// Deps are preserved (resume re-releases a node only when all its deps are merged).
	if len(deps["t2"]) != 1 || deps["t2"][0] != "t1" {
		t.Errorf("t2 deps = %v, want [t1]", deps["t2"])
	}

	// Snapshot is deterministic (sorted by id) so the serialized form is stable.
	if len(final.Nodes) != 3 || final.Nodes[0].ID != "t1" || final.Nodes[2].ID != "t3" {
		t.Errorf("nodes not sorted by id: %+v", final.Nodes)
	}
}

// A node never attempted by any integrate stays "pending" (spawned but not merged),
// so resume re-releases it rather than dropping it.
func TestSnapshotDefaultsUnintegratedToPending(t *testing.T) {
	s := &Supervisor{}
	st := &runState{
		handles:    map[string]*Handle{"a": nodeHandle("a"), "b": nodeHandle("b", "a")},
		nodeStates: map[string]string{"a": "merged"}, // b never integrated
		tipSHA:     "tip",
	}
	snap := s.snapshot(st)
	state := map[string]string{}
	for _, n := range snap.Nodes {
		state[n.ID] = n.State
	}
	if state["a"] != "merged" || state["b"] != "pending" {
		t.Errorf("states = %v, want a=merged b=pending", state)
	}
}

// seedResume must: seed the preserved tip + per-node states, keep the prior nodes in
// the snapshot (continuity across a SECOND restart) WITHOUT putting them in st.handles
// (so mergeOrder never tries to re-merge their swept branches), and produce a trusted
// resume framing that names what is already merged.
func TestSeedResumeContinuityAndNoPhantomMerge(t *testing.T) {
	s := &Supervisor{}
	st := &runState{handles: map[string]*Handle{}, nodeStates: map[string]string{}}

	rs := &ResumeState{
		TipSHA:    "tipsha123456789",
		TipBranch: "resume/supervise-thread-1",
		Nodes: []ResumeNode{
			{ID: "t1", State: "merged"},
			{ID: "t2", DependsOn: []string{"t1"}, State: "merged"},
			{ID: "t3", DependsOn: []string{"t1"}, State: "failed"},
		},
	}
	msg := s.seedResume(st, rs)

	// State is seeded: tip, branch, per-node dispositions.
	if st.tipSHA != rs.TipSHA || st.branch != rs.TipBranch {
		t.Errorf("tip/branch not seeded: tip=%q branch=%q", st.tipSHA, st.branch)
	}
	if st.nodeStates["t1"] != "merged" || st.nodeStates["t3"] != "failed" {
		t.Errorf("node states not seeded: %v", st.nodeStates)
	}
	// Prior nodes are NOT live handles — so mergeOrder (which needs Passed+Branch) emits
	// nothing, i.e. no swept branch is ever re-merged on resume.
	if len(st.handles) != 0 {
		t.Errorf("seedResume must not create live handles, got %d", len(st.handles))
	}
	if order := s.mergeOrder(st); len(order) != 0 {
		t.Errorf("mergeOrder over a freshly-resumed state must be empty (no phantom re-merge), got %+v", order)
	}

	// Snapshot continuity: a checkpoint taken right after resume still reports the prior
	// merged nodes (so a SECOND crash does not lose them), with the preserved tip.
	snap := s.snapshot(st)
	if snap.TipSHA != rs.TipSHA {
		t.Errorf("snapshot tip = %q, want %q", snap.TipSHA, rs.TipSHA)
	}
	got := map[string]string{}
	for _, n := range snap.Nodes {
		got[n.ID] = n.State
	}
	if got["t1"] != "merged" || got["t2"] != "merged" || got["t3"] != "failed" {
		t.Errorf("snapshot lost prior dispositions: %v", got)
	}

	// The model framing names the already-merged work as the starting point.
	if !strings.Contains(msg, "RESUMING") || !strings.Contains(msg, "t1") || !strings.Contains(msg, "t2") {
		t.Errorf("resume framing should name the merged nodes; got %q", msg)
	}

	// A live handle for a resumed id supersedes the prior record (no duplicate node).
	st.handles["t3"] = &Handle{Spec: SubagentSpec{ID: "t3", DependsOn: []string{"t1"}}}
	st.nodeStates["t3"] = "merged" // t3 re-ran and merged this time
	snap2 := s.snapshot(st)
	count, state := 0, ""
	for _, n := range snap2.Nodes {
		if n.ID == "t3" {
			count++
			state = n.State
		}
	}
	if count != 1 || state != "merged" {
		t.Errorf("a live handle must supersede the prior resume record for t3: count=%d state=%q", count, state)
	}
}

// A run that was never seeded (Resume nil) has no resumeNodes, so snapshot() is exactly
// the handle-only walk — the resume path is inert on a fresh run.
func TestSnapshotWithoutResumeIsHandleOnly(t *testing.T) {
	s := &Supervisor{}
	st := &runState{
		handles:    map[string]*Handle{"a": nodeHandle("a")},
		nodeStates: map[string]string{"a": "merged"},
		tipSHA:     "tip",
	}
	snap := s.snapshot(st)
	if len(snap.Nodes) != 1 || snap.Nodes[0].ID != "a" {
		t.Errorf("fresh-run snapshot should be handle-only: %+v", snap.Nodes)
	}
}

// With SaveState nil, recordIntegration still maintains the in-memory bookkeeping but
// takes no snapshot — observably byte-identical to a run without durable resume.
func TestRecordIntegrationNilSaveStateIsNoOp(t *testing.T) {
	s := &Supervisor{} // SaveState nil
	st := &runState{handles: map[string]*Handle{"t1": nodeHandle("t1")}, nodeStates: map[string]string{}}
	s.recordIntegration(context.Background(), st, []integrate.MergeResult{
		{ID: "t1", Merged: true, Verified: true, SHA: "x"},
	})
	if st.nodeStates["t1"] != "merged" || st.tipSHA != "x" {
		t.Errorf("bookkeeping not maintained: states=%v tip=%q", st.nodeStates, st.tipSHA)
	}
	// No panic, no save — the nil seam is the only observable difference.
}
