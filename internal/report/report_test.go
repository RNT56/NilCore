package report

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
)

// writeLog appends the given events to a fresh, hash-chained log file via the real
// eventlog.Open/Append path so the chain is valid (ReplayReport's Verify passes),
// then returns the path. Using the real writer (not hand-written JSONL) keeps the
// test honest about the on-wire shape and the chain.
func writeLog(t *testing.T, events []eventlog.Event) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "run-1.jsonl")
	l, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, e := range events {
		l.Append(e)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("seeded log does not verify: %v", err)
	}
	return path
}

// TestReplayReport is the named test (Verify line). It is a suite of subtests
// covering every acceptance bullet: family classification, graceful degradation
// without claim_* events, seeded-artifact folding, broken-chain fail-closed, and
// the FinalPass gate.
func TestReplayReport(t *testing.T) {
	t.Run("Families", testFamilies)
	t.Run("GracefulDegradationNoClaimEvents", testGracefulDegradation)
	t.Run("RetryHistoryFromClaimEvents", testRetryHistory)
	t.Run("SeededArtifactFold", testSeededArtifact)
	t.Run("SchemaDefects", testSchemaDefects)
	t.Run("BrokenChainFailsClosed", testBrokenChain)
	t.Run("FinalPassGate", testFinalPassGate)
	t.Run("EmptyLog", testEmptyLog)
	t.Run("MissingLogIsError", testMissingLog)
}

// testSchemaDefects proves a correctly-shaped schema_verify event projects into
// SchemaDefectRows. The on-wire shape is EXACTLY what the SchemaVerifier's eventlog
// sink emits (SW-T06): the artifact id at Detail["id"], and a "defects" list whose
// entries carry the lowercase {code, field, claim_id, reason}. A field-name mismatch
// here would silently leave SchemaDefects empty, so this locks the decoder to the shape.
func testSchemaDefects(t *testing.T) {
	events := []eventlog.Event{
		{Task: "fin", Kind: "schema_verify", Detail: map[string]any{
			"id":     "co-041",
			"passed": false,
			"defects": []map[string]any{
				{"code": "missing_field", "field": "value", "claim_id": "co-041-rev", "reason": "value is empty"},
				{"code": "missing_citation", "field": "source_url", "claim_id": "co-041-eps", "reason": "no source url"},
			},
		}},
	}
	m, err := ReplayReport(writeLog(t, events), "")
	if err != nil {
		t.Fatalf("ReplayReport: %v", err)
	}
	if len(m.SchemaDefects) != 2 {
		t.Fatalf("want 2 schema defect rows, got %d: %+v", len(m.SchemaDefects), m.SchemaDefects)
	}
	want0 := SchemaDefectRow{
		ArtifactID: "co-041", ClaimID: "co-041-rev", Field: "value",
		Code: "missing_field", Reason: "value is empty",
	}
	if m.SchemaDefects[0] != want0 {
		t.Errorf("schema defect row 0 = %+v, want %+v", m.SchemaDefects[0], want0)
	}
	if m.SchemaDefects[1].Code != "missing_citation" || m.SchemaDefects[1].ClaimID != "co-041-eps" {
		t.Errorf("schema defect row 1 mismatch: %+v", m.SchemaDefects[1])
	}

	// A passing schema check (no defects) yields zero rows — and never an error.
	clean := []eventlog.Event{
		{Task: "fin", Kind: "schema_verify", Detail: map[string]any{"id": "co-041", "passed": true}},
	}
	mc, err := ReplayReport(writeLog(t, clean), "")
	if err != nil {
		t.Fatalf("ReplayReport (clean): %v", err)
	}
	if len(mc.SchemaDefects) != 0 {
		t.Errorf("a passing schema check must yield zero defect rows, got %+v", mc.SchemaDefects)
	}
}

// testFamilies asserts each verify-family Kind projects to a CheckResult with the
// correct Family + Passed, decoded from that family's Detail shape.
func testFamilies(t *testing.T) {
	events := []eventlog.Event{
		{Task: "A", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "A", Kind: "final_verify", Detail: map[string]any{"passed": true}},
		{Task: "P", Kind: "project_verify", Detail: map[string]any{"iteration": 3, "unmet": 0, "no_progress": 0}},
		{Task: "P", Kind: "project_acceptance", Detail: map[string]any{"proposed": 2, "added": 1, "dropped": 0, "total": 5}},
		{Task: "M", Kind: "integration_verify", Detail: map[string]any{"branch": "task/x", "passed": true, "sha": "deadbeef"}},
		{Task: "M", Kind: "integration_rollback", Detail: map[string]any{"branch": "task/y", "escalate": true}},
		{Task: "M", Kind: "integration_conflict", Detail: map[string]any{"branch": "task/z", "escalate": true}},
		{Task: "Z", Kind: "artifact_verify", Detail: map[string]any{"id": "no-such", "green": true}},
		// A non-family kind must NOT appear as a check.
		{Task: "A", Kind: "tool_exec", Detail: map[string]any{"name": "fs"}},
	}
	m, err := ReplayReport(writeLog(t, events), "")
	if err != nil {
		t.Fatalf("ReplayReport: %v", err)
	}

	type want struct {
		family string
		passed bool
	}
	wants := []want{
		{"verify", true},
		{"final_verify", true},
		{"project_verify", true},        // unmet==0
		{"project_acceptance", true},    // bookkeeping, always passed
		{"integration_verify", true},    // passed:true
		{"integration_rollback", false}, // failure by definition
		{"integration_conflict", false}, // failure by definition
		{"artifact_verify", true},       // green:true
	}
	if len(m.Checks) != len(wants) {
		t.Fatalf("got %d checks, want %d: %+v", len(m.Checks), len(wants), m.Checks)
	}
	for i, w := range wants {
		got := m.Checks[i]
		if got.Family != w.family || got.Passed != w.passed {
			t.Errorf("check[%d] = {Family:%q Passed:%v}, want {Family:%q Passed:%v}",
				i, got.Family, got.Passed, w.family, w.passed)
		}
		if got.Family != got.Name {
			t.Errorf("check[%d] Name %q != Family %q", i, got.Name, got.Family)
		}
		if got.Seq == 0 && i != 0 {
			t.Errorf("check[%d] Seq not populated", i)
		}
	}

	// project_verify with unmet>0 must read as NOT passed (fail-closed).
	ev2 := []eventlog.Event{{Task: "P", Kind: "project_verify", Detail: map[string]any{"unmet": 2}}}
	m2, err := ReplayReport(writeLog(t, ev2), "")
	if err != nil {
		t.Fatalf("ReplayReport (unmet>0): %v", err)
	}
	if len(m2.Checks) != 1 || m2.Checks[0].Passed {
		t.Fatalf("project_verify unmet>0 should not pass: %+v", m2.Checks)
	}
}

// testGracefulDegradation: a log WITHOUT any claim_* / requeue kinds still yields a
// valid model with an empty retry list (the auditor blocker).
func testGracefulDegradation(t *testing.T) {
	events := []eventlog.Event{
		{Task: "A", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "A", Kind: "subagent_report", Detail: map[string]any{"passed": true, "branch": "task/a"}}, // un-enriched
		{Task: "A", Kind: "final_verify", Detail: map[string]any{"passed": true}},
	}
	m, err := ReplayReport(writeLog(t, events), "")
	if err != nil {
		t.Fatalf("ReplayReport: %v", err)
	}
	if len(m.Retries) != 0 {
		t.Fatalf("expected no retries from an un-enriched log, got %+v", m.Retries)
	}
	if !m.ChainVerified || !m.FinalPass {
		t.Fatalf("clean all-pass log should be ChainVerified+FinalPass, got chain=%v final=%v", m.ChainVerified, m.FinalPass)
	}
}

// testRetryHistory: claim_* kinds (primary) plus an enriched subagent_report
// (secondary) project to RetryAttempts ordered by Seq.
func testRetryHistory(t *testing.T) {
	events := []eventlog.Event{
		{Task: "rev", Kind: "claim_requeue", Detail: map[string]any{"attempt": 1, "claim_id": "co-revenue"}},
		{Task: "rev", Kind: "subagent_report", Detail: map[string]any{"passed": false, "branch": "task/rev", "continue_from": "co-revenue", "base": "main"}},
		{Task: "rev", Kind: "claim_resolved", Detail: map[string]any{"attempt": 2, "claim_id": "co-revenue"}},
		{Task: "rev", Kind: "requeue_exhausted", Detail: map[string]any{"attempt": 3, "claim_id": "co-margin"}},
	}
	m, err := ReplayReport(writeLog(t, events), "")
	if err != nil {
		t.Fatalf("ReplayReport: %v", err)
	}
	if len(m.Retries) != 4 {
		t.Fatalf("expected 4 retry rows, got %d: %+v", len(m.Retries), m.Retries)
	}
	// Ordered by Seq.
	for i := 1; i < len(m.Retries); i++ {
		if m.Retries[i].Seq < m.Retries[i-1].Seq {
			t.Fatalf("retries not Seq-ordered: %+v", m.Retries)
		}
	}
	if !m.Retries[2].Passed { // claim_resolved
		t.Errorf("claim_resolved should be Passed")
	}
	if m.Retries[3].Passed { // requeue_exhausted
		t.Errorf("requeue_exhausted should not be Passed")
	}
	if m.Retries[1].ContinueFrom != "co-revenue" || m.Retries[1].BaseBranch != "main" {
		t.Errorf("enriched subagent_report not projected: %+v", m.Retries[1])
	}
}

// testSeededArtifact: for an artifact_verify id, the persisted artifact folds into
// an ArtifactView with 1:1 ClaimRows and Green == artifact.Green().
func testSeededArtifact(t *testing.T) {
	root := t.TempDir()
	a := &artifact.Artifact{
		ID:        "co-041",
		Kind:      artifact.KindReport,
		Title:     "Acme FY2024",
		CreatedAt: time.Now().UTC(),
		Claims: []artifact.Claim{
			{ID: "co-041-rev", Field: "revenue_fy2024", Evidence: artifact.Evidence{
				Value: "1.2B", SourceURL: "https://sec.gov/x", Verifier: "finance.sec_fact", Status: artifact.StatusPass, Detail: "ok"}},
			{ID: "co-041-eps", Field: "eps_fy2024", Evidence: artifact.Evidence{
				Value: "3.10", SourceURL: "https://sec.gov/y", Verifier: "finance.sec_fact", Status: artifact.StatusPass, Detail: "ok"}},
		},
	}
	if err := artifact.Write(root, a); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	events := []eventlog.Event{
		{Task: "fin", Kind: "artifact_verify", Detail: map[string]any{"id": "co-041", "green": true, "pass": 2}},
	}
	m, err := ReplayReport(writeLog(t, events), root)
	if err != nil {
		t.Fatalf("ReplayReport: %v", err)
	}
	if len(m.Artifacts) != 1 {
		t.Fatalf("expected 1 folded artifact, got %d", len(m.Artifacts))
	}
	av := m.Artifacts[0]
	if av.ID != "co-041" || av.Title != "Acme FY2024" || av.Kind != artifact.KindReport {
		t.Errorf("artifact view header wrong: %+v", av)
	}
	if av.Green != a.Green() {
		t.Errorf("ArtifactView.Green=%v want artifact.Green()=%v", av.Green, a.Green())
	}
	if len(av.Claims) != len(a.Claims) {
		t.Fatalf("claim rows %d != claims %d", len(av.Claims), len(a.Claims))
	}
	if av.Claims[0].ClaimID != "co-041-rev" || av.Claims[0].Value != "1.2B" ||
		av.Claims[0].SourceURL != "https://sec.gov/x" || av.Claims[0].Status != artifact.StatusPass {
		t.Errorf("claim row 0 mismatch: %+v", av.Claims[0])
	}

	// A fail status makes Green false (mirrors artifact.Green()).
	a.Claims[1].Evidence.Status = artifact.StatusFail
	root2 := t.TempDir()
	if err := artifact.Write(root2, a); err != nil {
		t.Fatalf("seed artifact 2: %v", err)
	}
	m2, err := ReplayReport(writeLog(t, events), root2)
	if err != nil {
		t.Fatalf("ReplayReport 2: %v", err)
	}
	if m2.Artifacts[0].Green {
		t.Errorf("artifact with a fail claim must not be Green")
	}

	// A missing artifact file is omitted, not fatal.
	m3, err := ReplayReport(writeLog(t, events), t.TempDir())
	if err != nil {
		t.Fatalf("ReplayReport (missing file): %v", err)
	}
	if len(m3.Artifacts) != 0 {
		t.Errorf("missing artifact should be omitted, got %+v", m3.Artifacts)
	}
}

// testBrokenChain: a tampered log ⇒ ChainVerified=false AND FinalPass=false, with
// the model still populated (not an error that hides it).
func testBrokenChain(t *testing.T) {
	events := []eventlog.Event{
		{Task: "A", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "A", Kind: "final_verify", Detail: map[string]any{"passed": true}},
	}
	path := writeLog(t, events)

	// Corrupt a Detail byte in the first line so the hash no longer matches.
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

	m, err := ReplayReport(path, "")
	if err != nil {
		t.Fatalf("ReplayReport must not error on a broken chain: %v", err)
	}
	if m.ChainVerified {
		t.Errorf("broken chain should set ChainVerified=false")
	}
	if m.FinalPass {
		t.Errorf("broken chain must force FinalPass=false (green-over-broken-chain)")
	}
	if len(m.Checks) == 0 {
		t.Errorf("model should still be populated over a broken chain")
	}
}

// testFinalPassGate: FinalPass requires ChainVerified AND every check passed.
func testFinalPassGate(t *testing.T) {
	// All pass + clean chain ⇒ FinalPass true.
	pass := []eventlog.Event{
		{Task: "A", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "A", Kind: "final_verify", Detail: map[string]any{"passed": true}},
	}
	mp, _ := ReplayReport(writeLog(t, pass), "")
	if !mp.FinalPass {
		t.Errorf("all-pass clean chain should be FinalPass")
	}

	// One failing check ⇒ FinalPass false even on a clean chain.
	mixed := []eventlog.Event{
		{Task: "A", Kind: "verify", Detail: map[string]any{"passed": true}},
		{Task: "A", Kind: "final_verify", Detail: map[string]any{"passed": false}},
	}
	mm, _ := ReplayReport(writeLog(t, mixed), "")
	if !mm.ChainVerified {
		t.Errorf("chain should verify")
	}
	if mm.FinalPass {
		t.Errorf("a failing check must force FinalPass=false")
	}
}

// testEmptyLog: an empty (but present) log is valid — no checks, ChainVerified
// true (Verify treats empty as intact), but FinalPass false (nothing earned green).
func testEmptyLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write empty log: %v", err)
	}
	m, err := ReplayReport(path, "")
	if err != nil {
		t.Fatalf("ReplayReport empty: %v", err)
	}
	if len(m.Checks) != 0 || m.FinalPass {
		t.Errorf("empty log: want no checks and FinalPass=false, got %+v", m)
	}
	if m.Run != "empty" {
		t.Errorf("run name = %q, want %q", m.Run, "empty")
	}
}

// testMissingLog: a genuinely unreadable log is an error (distinct from a broken
// chain, which is a populated model).
func testMissingLog(t *testing.T) {
	_, err := ReplayReport(filepath.Join(t.TempDir(), "nope.jsonl"), "")
	if err == nil {
		t.Fatalf("missing log should return an error")
	}
}

// tamperFirstPassed rewrites the first `"passed":true` key to `"passad":true`
// (same byte length, still valid JSON) in the log bytes. This is the realistic
// "someone edited the log" attack: the line still parses, but the hash chain no
// longer matches, so eventlog.Verify trips and only the chain check fails — which
// is exactly what ReplayReport must surface as ChainVerified=false.
func tamperFirstPassed(data []byte) []byte {
	return replaceFirst(data, []byte(`"passed":true`), []byte(`"passad":true`))
}

func replaceFirst(data, old, new []byte) []byte {
	for i := 0; i+len(old) <= len(data); i++ {
		if string(data[i:i+len(old)]) == string(old) {
			out := make([]byte, 0, len(data))
			out = append(out, data[:i]...)
			out = append(out, new...)
			out = append(out, data[i+len(old):]...)
			return out
		}
	}
	return data
}
