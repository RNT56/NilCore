package report

import (
	"os"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
)

// TestReplaySwarmReport is the named test (Verify line) for the swarm-dimension
// projection: the swarm-Kind fold, the broken-chain double gate, graceful
// degradation on a non-swarm log, and the additive claim-trace / schema-defect
// population in the same single Base pass.
func TestReplaySwarmReport(t *testing.T) {
	t.Run("SwarmKindFold", testSwarmKindFold)
	t.Run("BrokenChainBothGatesFalse", testSwarmBrokenChain)
	t.Run("CleanGateRequiresAllThree", testCleanGate)
	t.Run("NonSwarmLogDegrades", testNonSwarmLogDegrades)
	t.Run("ClaimTracesAndDefects", testClaimTracesAndDefects)
}

// testSwarmKindFold: the scoreboard_snapshot events fold into PassRows ordered by
// pass, the FINAL row drives the headline counts, and a swarm_pass_clean event with
// zero remaining on a verified chain flips FinalCleanPass true.
func testSwarmKindFold(t *testing.T) {
	events := []eventlog.Event{
		{Kind: "swarm_start", Detail: map[string]any{"goal": "x"}},
		{Kind: "scoreboard_snapshot", Detail: map[string]any{"pass": 1, "checked": 3, "passed": 2, "failed": 1, "retry_pass": 0, "remaining": 1}},
		{Kind: "scoreboard_snapshot", Detail: map[string]any{"pass": 2, "checked": 3, "passed": 3, "failed": 0, "retry_pass": 1, "remaining": 0}},
		{Kind: "swarm_pass_clean", Detail: map[string]any{"pass": 2}},
		{Kind: "swarm_done", Detail: map[string]any{}},
	}
	sr, err := ReplaySwarmReport(writeLog(t, events), "")
	if err != nil {
		t.Fatalf("ReplaySwarmReport: %v", err)
	}
	if len(sr.Swarm.PassRows) != 2 {
		t.Fatalf("want 2 pass rows, got %d: %+v", len(sr.Swarm.PassRows), sr.Swarm.PassRows)
	}
	if sr.Swarm.PassRows[0].Pass != 1 || sr.Swarm.PassRows[1].Pass != 2 {
		t.Fatalf("pass rows not ordered by pass: %+v", sr.Swarm.PassRows)
	}
	// Headline = the final (pass 2) snapshot.
	if sr.Swarm.Pass != 2 || sr.Swarm.Checked != 3 || sr.Swarm.Passed != 3 ||
		sr.Swarm.Failed != 0 || sr.Swarm.RetryPass != 1 || sr.Swarm.Remaining != 0 {
		t.Fatalf("final-pass headline counts wrong: %+v", sr.Swarm)
	}
	if !sr.Base.ChainVerified {
		t.Fatalf("seeded log should verify")
	}
	if !sr.Swarm.FinalCleanPass {
		t.Fatalf("verified chain + clean event + zero remaining must be FinalCleanPass")
	}
}

// testSwarmBrokenChain: a tampered log forces BOTH Base.FinalPass=false AND
// Swarm.FinalCleanPass=false, even though the scoreboard says everything passed.
func testSwarmBrokenChain(t *testing.T) {
	events := []eventlog.Event{
		{Kind: "verify", Detail: map[string]any{"passed": true}},
		{Kind: "scoreboard_snapshot", Detail: map[string]any{"pass": 1, "checked": 1, "passed": 1, "failed": 0, "remaining": 0}},
		{Kind: "swarm_pass_clean", Detail: map[string]any{"pass": 1}},
	}
	path := writeLog(t, events)

	// Corrupt a Detail byte so the hash chain no longer matches.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	tampered := tamperFirstPassed(data)
	if err := os.WriteFile(path, tampered, 0o644); err != nil {
		t.Fatalf("write tampered log: %v", err)
	}
	if eventlog.Verify(path) == nil {
		t.Fatalf("precondition: tampered log should NOT verify")
	}

	sr, err := ReplaySwarmReport(path, "")
	if err != nil {
		t.Fatalf("ReplaySwarmReport must not error on a broken chain: %v", err)
	}
	if sr.Base.ChainVerified {
		t.Errorf("broken chain should set Base.ChainVerified=false")
	}
	if sr.Base.FinalPass {
		t.Errorf("broken chain must force Base.FinalPass=false")
	}
	if sr.Swarm.FinalCleanPass {
		t.Errorf("broken chain must force Swarm.FinalCleanPass=false (green-over-broken-chain)")
	}
}

// testCleanGate: FinalCleanPass requires ALL THREE legs — chain verified, a
// swarm_pass_clean event present, AND zero remaining. Drop any one and it is false.
func testCleanGate(t *testing.T) {
	// Missing the clean event ⇒ not clean.
	noClean := []eventlog.Event{
		{Kind: "scoreboard_snapshot", Detail: map[string]any{"pass": 1, "remaining": 0}},
	}
	sr, err := ReplaySwarmReport(writeLog(t, noClean), "")
	if err != nil {
		t.Fatalf("ReplaySwarmReport: %v", err)
	}
	if sr.Swarm.FinalCleanPass {
		t.Errorf("no swarm_pass_clean event must not be FinalCleanPass")
	}

	// Clean event present but remaining>0 ⇒ not clean.
	remaining := []eventlog.Event{
		{Kind: "scoreboard_snapshot", Detail: map[string]any{"pass": 1, "remaining": 2}},
		{Kind: "swarm_pass_clean", Detail: map[string]any{"pass": 1}},
	}
	sr2, err := ReplaySwarmReport(writeLog(t, remaining), "")
	if err != nil {
		t.Fatalf("ReplaySwarmReport: %v", err)
	}
	if sr2.Swarm.FinalCleanPass {
		t.Errorf("remaining>0 must not be FinalCleanPass")
	}
}

// testNonSwarmLogDegrades: a plain (non-swarm) log still yields a valid SwarmReport
// whose Swarm dimension is the zero value (no swarm Kinds were present).
func testNonSwarmLogDegrades(t *testing.T) {
	events := []eventlog.Event{
		{Kind: "verify", Detail: map[string]any{"passed": true}},
		{Kind: "final_verify", Detail: map[string]any{"passed": true}},
	}
	sr, err := ReplaySwarmReport(writeLog(t, events), "")
	if err != nil {
		t.Fatalf("ReplaySwarmReport: %v", err)
	}
	if sr.Base == nil {
		t.Fatalf("Base must be populated")
	}
	if len(sr.Swarm.PassRows) != 0 || sr.Swarm.FinalCleanPass {
		t.Errorf("non-swarm log should leave Swarm zero, got %+v", sr.Swarm)
	}
	// The Base dimension is unaffected — a clean all-pass run is still FinalPass.
	if !sr.Base.FinalPass {
		t.Errorf("Base should still compute FinalPass for a clean log")
	}
}

// testClaimTracesAndDefects: the additive ClaimTraces + SchemaDefects are populated
// in the same Base pass — claim traces from the folded artifact, defects from a
// schema_verify event, and the trace Attempt stamped from a claim_resolved event.
func testClaimTracesAndDefects(t *testing.T) {
	root := t.TempDir()
	a := &artifact.Artifact{
		ID:        "co-1",
		Kind:      artifact.KindReport,
		Title:     "T",
		CreatedAt: time.Now().UTC(),
		Claims: []artifact.Claim{
			{ID: "c-rev", Field: "revenue", Evidence: artifact.Evidence{
				Value: "1B", SourceURL: "https://sec.gov/a", Verifier: "finance.sec_fact",
				Status: artifact.StatusPass, Detail: "ok", RetrievedAt: time.Now().UTC()}},
			{ID: "c-eps", Field: "eps", Evidence: artifact.Evidence{
				Value: "3.1", SourceURL: "https://sec.gov/b", Verifier: "finance.sec_fact",
				Status: artifact.StatusFail, Detail: "mismatch"}},
		},
	}
	if err := artifact.Write(root, a); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	events := []eventlog.Event{
		{Kind: "artifact_verify", Detail: map[string]any{"id": "co-1", "green": false}},
		{Kind: "schema_verify", Detail: map[string]any{
			"id": "co-1", "kind": "report", "passed": false,
			"defects": []any{
				map[string]any{"code": "MissingCitation", "field": "source_url", "claim_id": "c-eps", "reason": "claim \"c-eps\": source missing"},
			},
		}},
		{Kind: "claim_resolved", Detail: map[string]any{"attempt": 2, "claim_id": "c-rev"}},
	}
	sr, err := ReplaySwarmReport(writeLog(t, events), root)
	if err != nil {
		t.Fatalf("ReplaySwarmReport: %v", err)
	}
	m := sr.Base
	if len(m.ClaimTraces) != 2 {
		t.Fatalf("want 2 claim traces, got %d: %+v", len(m.ClaimTraces), m.ClaimTraces)
	}
	// Trace 0 (c-rev): pass, source resolved, Attempt stamped from claim_resolved.
	tr := traceByClaim(m.ClaimTraces, "c-rev")
	if tr == nil {
		t.Fatalf("missing c-rev trace")
	}
	if tr.Status != artifact.StatusPass || !tr.Source.Resolved || tr.Attempt != 2 ||
		tr.Source.Locator != "https://sec.gov/a" || tr.Value != "1B" {
		t.Errorf("c-rev trace wrong: %+v", *tr)
	}
	// Trace 1 (c-eps): fail still counts as a resolved source (decisive verdict).
	tre := traceByClaim(m.ClaimTraces, "c-eps")
	if tre == nil || tre.Status != artifact.StatusFail || !tre.Source.Resolved || tre.Attempt != 0 {
		t.Errorf("c-eps trace wrong: %+v", tre)
	}
	// Schema defect folded from the schema_verify event.
	if len(m.SchemaDefects) != 1 {
		t.Fatalf("want 1 schema defect, got %d: %+v", len(m.SchemaDefects), m.SchemaDefects)
	}
	d := m.SchemaDefects[0]
	if d.ArtifactID != "co-1" || d.ClaimID != "c-eps" || d.Field != "source_url" || d.Code != "MissingCitation" {
		t.Errorf("schema defect row wrong: %+v", d)
	}
}

// traceByClaim finds the trace with the given claim id, or nil.
func traceByClaim(traces []ClaimTrace, claimID string) *ClaimTrace {
	for i := range traces {
		if traces[i].ClaimID == claimID {
			return &traces[i]
		}
	}
	return nil
}
