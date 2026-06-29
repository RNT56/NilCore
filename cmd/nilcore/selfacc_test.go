package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// selfaccFakeBox is a minimal sandbox whose Exec returns a fixed exit code, so a
// registered candidate's check can be resolved hermetically (exit 0 ⇒ Pass).
type selfaccFakeBox struct{ exit int }

func (b selfaccFakeBox) Exec(context.Context, string) (sandbox.Result, error) {
	return sandbox.Result{ExitCode: b.exit}, nil
}
func (b selfaccFakeBox) ExecWithEnv(context.Context, string, map[string]string) (sandbox.Result, error) {
	return sandbox.Result{ExitCode: b.exit}, nil
}
func (b selfaccFakeBox) Workdir() string { return "" }

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

// resolveClaim resolves a claim bound to verifierID through reg against a fixed-exit box.
func resolveClaim(reg *evverify.Registry, verifierID string, exit int) artifact.Status {
	claim := artifact.Claim{Evidence: artifact.Evidence{Verifier: verifierID}}
	st, _ := reg.Resolve(context.Background(), selfaccFakeBox{exit: exit}, claim)
	return st
}

// TestSelfAcceptanceDefaultOff: with NILCORE_SELFACC unset, registration is a no-op even
// when a valid file is named — the capability is strictly opt-in.
func TestSelfAcceptanceDefaultOff(t *testing.T) {
	t.Setenv(selfAcceptanceEnv, "")
	t.Setenv(selfAcceptanceFileEnv, writeApproved(t, approvedCandidate{VerifierID: "candidate.ok", Command: "true"}))
	reg := evverify.Default()
	ids, err := registerSelfAcceptance(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("default-off must bind nothing, bound %v", ids)
	}
	if resolveClaim(reg, "candidate.ok", 0) != artifact.StatusUnverifiable {
		t.Error("an unbound candidate id must resolve Unverifiable")
	}
}

// TestSelfAcceptanceBindsAdmissible: an opted-in, admissible candidate binds a sandboxed,
// fail-closed check — Pass only on exit 0, Unverifiable otherwise.
func TestSelfAcceptanceBindsAdmissible(t *testing.T) {
	t.Setenv(selfAcceptanceEnv, "1")
	t.Setenv(selfAcceptanceFileEnv, writeApproved(t, approvedCandidate{
		VerifierID: "candidate.build_passes", Command: "test -f built.bin", Rationale: "artifact exists",
	}))
	reg := evverify.Default()
	ids, err := registerSelfAcceptance(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "candidate.build_passes" {
		t.Fatalf("expected one bound id candidate.build_passes, got %v", ids)
	}
	if got := resolveClaim(reg, "candidate.build_passes", 0); got != artifact.StatusPass {
		t.Errorf("exit 0 must resolve Pass, got %v", got)
	}
	if got := resolveClaim(reg, "candidate.build_passes", 1); got != artifact.StatusUnverifiable {
		t.Errorf("non-zero exit must resolve Unverifiable (never a silent pass), got %v", got)
	}
}

// TestSelfAcceptanceSkipsUnadmissible: a candidate the meta-check rejects (here, a
// host/in-process marker) is SKIPPED — never bound — so its id stays Unverifiable.
func TestSelfAcceptanceSkipsUnadmissible(t *testing.T) {
	t.Setenv(selfAcceptanceEnv, "1")
	t.Setenv(selfAcceptanceFileEnv, writeApproved(t,
		approvedCandidate{VerifierID: "candidate.evil", Command: "go:in-process run hack"},
		approvedCandidate{VerifierID: "candidate.empty", Command: "   "},
		approvedCandidate{VerifierID: "candidate.ok", Command: "true"},
	))
	reg := evverify.Default()
	ids, err := registerSelfAcceptance(reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "candidate.ok" {
		t.Fatalf("only the admissible candidate must bind, got %v", ids)
	}
	if resolveClaim(reg, "candidate.evil", 0) != artifact.StatusUnverifiable {
		t.Error("a host-marker candidate must never bind")
	}
	if resolveClaim(reg, "candidate.empty", 0) != artifact.StatusUnverifiable {
		t.Error("an empty-command candidate must never bind")
	}
}

// TestSelfAcceptanceMalformedFailsClosed: an opted-in but malformed approved file is a
// hard error (the caller reddens the verdict), never a silent fall-through.
func TestSelfAcceptanceMalformedFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(selfAcceptanceEnv, "1")
	t.Setenv(selfAcceptanceFileEnv, path)
	if _, err := registerSelfAcceptance(evverify.Default()); err == nil {
		t.Fatal("a malformed approved file must be a hard error (fail-closed)")
	}
}

// TestSelfAcceptanceOnButNoFile: opted in with no file named ⇒ nothing bound, no error.
func TestSelfAcceptanceOnButNoFile(t *testing.T) {
	t.Setenv(selfAcceptanceEnv, "1")
	t.Setenv(selfAcceptanceFileEnv, "")
	ids, err := registerSelfAcceptance(evverify.Default())
	if err != nil || len(ids) != 0 {
		t.Fatalf("opted-in with no file ⇒ (nil, no error); got ids=%v err=%v", ids, err)
	}
}
