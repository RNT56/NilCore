package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify/selfacc"
)

// recordingBox is a sandbox that records every command run and returns a per-command
// exit code (default 0), so a self-acceptance check's bind/run/skip can be asserted
// hermetically.
type recordingBox struct {
	mu   sync.Mutex
	ran  []string
	exit map[string]int // trimmed command -> exit code
}

func (b *recordingBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ran = append(b.ran, cmd)
	return sandbox.Result{ExitCode: b.exit[cmd]}, nil
}
func (b *recordingBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *recordingBox) Workdir() string { return "" }
func (b *recordingBox) didRun(cmd string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.ran {
		if c == cmd {
			return true
		}
	}
	return false
}

func approveGate(policy.GateAction) bool { return true }
func denyGate(policy.GateAction) bool    { return false }

func testLog(t *testing.T) *eventlog.Log {
	t.Helper()
	log, err := eventlog.Open(filepath.Join(t.TempDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	return log
}

// writeApproved writes an approved-candidates file and returns its path.
func writeApproved(t *testing.T, cands ...approvedCandidate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approved.json")
	data, err := json.Marshal(approvedFile{Candidates: cands})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func cand(id, cmd string) selfacc.Candidate { return selfacc.Candidate{VerifierID: id, Command: cmd} }

// TestSelfAcceptHookOff: NILCORE_SELFACC unset ⇒ the hook is nil (orchestrator
// byte-identical); set ⇒ the hook is installed.
func TestSelfAcceptHookOff(t *testing.T) {
	t.Setenv(selfAcceptanceEnv, "")
	if selfAcceptHook(nil) != nil {
		t.Error("hook must be nil when self-acceptance is off")
	}
	t.Setenv(selfAcceptanceEnv, "1")
	if selfAcceptHook(nil) == nil {
		t.Error("hook must be installed when self-acceptance is on")
	}
}

// TestRunCandidatesNoCandidates: no checks ⇒ green (nothing to add to the bar).
func TestRunCandidatesNoCandidates(t *testing.T) {
	passed, detail := runCandidates(context.Background(), &recordingBox{}, approveGate, testLog(t), nil, nil)
	if !passed || detail != "" {
		t.Fatalf("no candidates must pass clean; got passed=%v detail=%q", passed, detail)
	}
}

// TestRunCandidatesPreApprovedPass: an operator pre-approved check that exits 0 passes
// and runs WITHOUT consulting the gate.
func TestRunCandidatesPreApprovedPass(t *testing.T) {
	box := &recordingBox{exit: map[string]int{"test -f built": 0}}
	gateCalled := false
	gate := func(policy.GateAction) bool { gateCalled = true; return true }
	passed, _ := runCandidates(context.Background(), box, gate, testLog(t), []selfacc.Candidate{cand("candidate.ok", "test -f built")}, nil)
	if !passed {
		t.Error("a pre-approved check that exits 0 must pass")
	}
	if !box.didRun("test -f built") {
		t.Error("the pre-approved check must have run in the box")
	}
	if gateCalled {
		t.Error("a pre-approved (operator-file) check must NOT consult the gate")
	}
}

// TestRunCandidatesPreApprovedFail: a non-zero exit reddens the verdict and names the id.
func TestRunCandidatesPreApprovedFail(t *testing.T) {
	box := &recordingBox{exit: map[string]int{"false": 1}}
	passed, detail := runCandidates(context.Background(), box, approveGate, testLog(t), []selfacc.Candidate{cand("candidate.fails", "false")}, nil)
	if passed {
		t.Error("a failing check must redden the verdict")
	}
	if detail == "" || !contains(detail, "candidate.fails") {
		t.Errorf("detail must name the failing check, got %q", detail)
	}
}

// TestRunCandidatesProposedApproved: an agent-proposed check is gated; approved + exit 0
// passes and runs.
func TestRunCandidatesProposedApproved(t *testing.T) {
	box := &recordingBox{exit: map[string]int{"grep -q ok out": 0}}
	passed, _ := runCandidates(context.Background(), box, approveGate, testLog(t), nil, []selfacc.Candidate{cand("candidate.proposed", "grep -q ok out")})
	if !passed {
		t.Error("an approved, passing proposed check must pass")
	}
	if !box.didRun("grep -q ok out") {
		t.Error("an approved proposed check must run")
	}
}

// TestRunCandidatesProposedDenied: a gate-denied proposed check does NOT run and does
// NOT redden (denial ≠ failure — it just does not participate).
func TestRunCandidatesProposedDenied(t *testing.T) {
	box := &recordingBox{exit: map[string]int{"false": 1}}
	passed, detail := runCandidates(context.Background(), box, denyGate, testLog(t), nil, []selfacc.Candidate{cand("candidate.denied", "false")})
	if !passed {
		t.Errorf("a denied check must not redden the verdict; got detail=%q", detail)
	}
	if box.didRun("false") {
		t.Error("a denied check must never run")
	}
}

// TestRunCandidatesUnadmissibleSkipped: a host/in-process candidate is skipped (never
// bound, never run) and does not redden.
func TestRunCandidatesUnadmissibleSkipped(t *testing.T) {
	box := &recordingBox{}
	passed, _ := runCandidates(context.Background(), box, approveGate, testLog(t), []selfacc.Candidate{cand("candidate.evil", "go:in-process hack")}, nil)
	if !passed {
		t.Error("an un-admissible candidate must be skipped, not a failure")
	}
	if len(box.ran) != 0 {
		t.Errorf("an un-admissible candidate must never run, ran=%v", box.ran)
	}
}

// TestRunSelfAcceptanceOperatorFile: end-to-end with nil model (no authoring) + an
// operator file — the pre-approved check runs and governs.
func TestRunSelfAcceptanceOperatorFile(t *testing.T) {
	t.Setenv(selfAcceptanceFileEnv, writeApproved(t, approvedCandidate{VerifierID: "candidate.file", Command: "true"}))
	box := &recordingBox{exit: map[string]int{"true": 0}}
	passed, _ := runSelfAcceptance(context.Background(), nil, 5, "a goal", box, approveGate, testLog(t))
	if !passed || !box.didRun("true") {
		t.Errorf("operator-file check must run and pass; passed=%v ran=%v", passed, box.ran)
	}
}

// TestParseSelfaccCandidates: a valid authoring reply parses; junk errors.
func TestParseSelfaccCandidates(t *testing.T) {
	reply := "sure!\n{\"candidates\":[{\"verifier_id\":\"candidate.a\",\"command\":\"go build ./...\",\"rationale\":\"compiles\"},{\"verifier_id\":\"candidate.b\",\"command\":\"go test ./...\"}]}\nthanks"
	cands, err := parseSelfaccCandidates(reply)
	if err != nil {
		t.Fatalf("valid reply: %v", err)
	}
	if len(cands) != 2 || cands[0].VerifierID != "candidate.a" || cands[1].Command != "go test ./..." {
		t.Fatalf("unexpected parse: %+v", cands)
	}
	if _, err := parseSelfaccCandidates("no json here"); err == nil {
		t.Error("a reply with no JSON object must error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
