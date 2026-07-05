package main

// Tests for the promote-gate evidence wiring (build.go): buildGateFuncEv attaches
// the payload for opting-in approvers only, the legacy flat path stays
// byte-identical, and the verify recorder / evidence assembler capture what the
// stack could reach and nothing more.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/budget"
	"nilcore/internal/policy"
	"nilcore/internal/verify"
)

// structuredCapture records the full GateAction it was consulted with.
type structuredCapture struct {
	got     policy.GateAction
	verdict bool
}

func (s *structuredCapture) Approve(string) bool { return s.verdict }
func (s *structuredCapture) ApproveStructured(a policy.GateAction) bool {
	s.got = a
	return s.verdict
}

// flatCapture is a legacy free-text approver recording the exact string handed to it.
type flatCapture struct {
	got     string
	verdict bool
}

func (f *flatCapture) Approve(action string) bool { f.got = action; return f.verdict }

func TestBuildGateFuncEvAttachesEvidence(t *testing.T) {
	evFn := func(a policy.GateAction) *policy.GateEvidence {
		return &policy.GateEvidence{Diffstat: "1 file(s) changed, +1 −0", VerifyTail: "[verify passed]\nok"}
	}
	action := policy.GateAction{Type: policy.PromoteToBase, Branch: "main", Detail: "tip"}

	t.Run("structured approver receives the payload", func(t *testing.T) {
		ap := &structuredCapture{verdict: true}
		if !buildGateFuncEv(ap, nil, evFn)(action) {
			t.Fatal("approver verdict must govern")
		}
		if ap.got.Evidence == nil || ap.got.Evidence.Diffstat != "1 file(s) changed, +1 −0" {
			t.Errorf("evidence not attached: %+v", ap.got.Evidence)
		}
	})

	t.Run("legacy approver stays byte-identical", func(t *testing.T) {
		ap := &flatCapture{verdict: true}
		if !buildGateFuncEv(ap, nil, evFn)(action) {
			t.Fatal("approver verdict must govern")
		}
		if ap.got != action.Describe() {
			t.Errorf("flat approver got %q, want %q", ap.got, action.Describe())
		}
	})

	t.Run("nil evidence source is the byte-identical buildGateFunc", func(t *testing.T) {
		ap := &structuredCapture{verdict: true}
		if !buildGateFunc(ap, nil)(action) {
			t.Fatal("approver verdict must govern")
		}
		if ap.got.Evidence != nil {
			t.Errorf("no evidence source must attach nothing: %+v", ap.got.Evidence)
		}
	})

	t.Run("caller-supplied evidence wins", func(t *testing.T) {
		pre := action
		pre.Evidence = &policy.GateEvidence{Diffstat: "caller"}
		ap := &structuredCapture{verdict: true}
		buildGateFuncEv(ap, nil, evFn)(pre)
		if ap.got.Evidence.Diffstat != "caller" {
			t.Errorf("pre-attached evidence overwritten: %+v", ap.got.Evidence)
		}
	})
}

func TestGateEvidenceFunc(t *testing.T) {
	rec := &verifyRecorder{}
	rec.record(verify.Report{Passed: true, Output: "all green"})
	ledger := budget.New()
	if err := ledger.Charge(context.Background(), "project", 100, 1.25); err != nil {
		t.Fatalf("charge: %v", err)
	}
	differ := func(branch string) (string, error) {
		if branch != "integrate/x" {
			t.Errorf("differ got branch %q", branch)
		}
		return "diff --git a/f.go b/f.go\n+added\n", nil
	}

	ev := gateEvidenceFunc(differ, rec, ledger)(policy.GateAction{Type: policy.PromoteToBase, Branch: "integrate/x"})
	if ev == nil {
		t.Fatal("expected evidence")
	}
	if !strings.Contains(ev.Diffstat, "1 file(s) changed, +1 −0") {
		t.Errorf("diffstat = %q", ev.Diffstat)
	}
	if !strings.Contains(ev.VerifyTail, "[verify passed]") || !strings.Contains(ev.VerifyTail, "all green") {
		t.Errorf("verify tail = %q", ev.VerifyTail)
	}
	if ev.SpentUSD != 1.25 {
		t.Errorf("spend = %v", ev.SpentUSD)
	}

	// A failing differ / empty branch leaves the diff sections empty but still
	// surfaces the rest — evidence is best-effort and never blocks the gate.
	badDiffer := func(string) (string, error) { return "", errors.New("boom") }
	ev = gateEvidenceFunc(badDiffer, rec, ledger)(policy.GateAction{Type: policy.PromoteToBase, Branch: "b"})
	if ev == nil || ev.Diffstat != "" || ev.DiffExcerpt != "" {
		t.Fatalf("diff sections must stay empty on differ error: %+v", ev)
	}
	if ev.VerifyTail == "" || ev.SpentUSD != 1.25 {
		t.Errorf("reachable sections must survive: %+v", ev)
	}

	// Nothing reachable at all ⇒ nil payload (legacy rendering everywhere).
	if got := gateEvidenceFunc(nil, nil, nil)(policy.GateAction{Type: policy.PromoteToBase}); got != nil {
		t.Errorf("expected nil evidence, got %+v", got)
	}
}

// fakeVerifier scripts one report for the recorder wrap tests.
type fakeVerifier struct {
	rep verify.Report
	err error
}

func (f fakeVerifier) Check(context.Context) (verify.Report, error) { return f.rep, f.err }

func TestVerifyRecorder(t *testing.T) {
	rec := &verifyRecorder{}
	if rec.last() != "" {
		t.Errorf("fresh recorder must be empty, got %q", rec.last())
	}
	var nilRec *verifyRecorder
	if nilRec.last() != "" {
		t.Error("nil recorder must be safe and empty")
	}

	// wrap records a pass with the explicit verdict line.
	if _, err := rec.wrap(fakeVerifier{rep: verify.Report{Passed: true, Output: "ok"}}).Check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := rec.last(); !strings.Contains(got, "[verify passed]") || !strings.Contains(got, "ok") {
		t.Errorf("recorded = %q", got)
	}

	// wrapFunc records a fail; the verdict line flips.
	fn := rec.wrapFunc(func(context.Context) (verify.Report, error) {
		return verify.Report{Passed: false, Output: "3 tests failed"}, nil
	})
	if _, err := fn(context.Background()); err != nil {
		t.Fatalf("wrapFunc: %v", err)
	}
	if got := rec.last(); !strings.Contains(got, "[verify FAILED]") || !strings.Contains(got, "3 tests failed") {
		t.Errorf("recorded = %q", got)
	}

	// An erroring Check records nothing (the previous report stands).
	before := rec.last()
	_, _ = rec.wrap(fakeVerifier{err: errors.New("sandbox down")}).Check(context.Background())
	if rec.last() != before {
		t.Errorf("an erroring check must not overwrite the last report")
	}
}
