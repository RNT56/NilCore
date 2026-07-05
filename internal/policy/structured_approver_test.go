package policy

import (
	"testing"
)

// fakeStructured records that the structured path was taken and returns a fixed
// verdict, so we can prove an opting-in approver bypasses the free-text Approve.
type fakeStructured struct {
	calledStructured bool
	calledFreeText   bool
	verdict          bool
}

func (f *fakeStructured) Approve(string) bool { f.calledFreeText = true; return f.verdict }
func (f *fakeStructured) ApproveStructured(GateAction) bool {
	f.calledStructured = true
	return f.verdict
}

var allGateActionTypes = []GateActionType{PromoteToBase, Push, Deploy, OpenPR}

// flatApprover is a free-text-only approver (it deliberately does NOT implement
// StructuredApprover), standing in for any legacy/third-party approver. It
// records the exact string it was handed so tests can pin the flattened line.
type flatApprover struct {
	verdict bool
	got     string
}

func (f *flatApprover) Approve(action string) bool { f.got = action; return f.verdict }

// TestGateStructuredFreeTextPathByteIdentical proves a non-StructuredApprover
// still reaches the exact prior behaviour: the free-text Approve is consulted
// with the flattened Describe() string and its verdict governs, for every
// GateActionType, with a nil approver default-denying. This is the
// byte-identical default-off proof. (ConsoleApprover no longer serves as the
// exemplar here: it opts in to StructuredApprover to render gate evidence.)
func TestGateStructuredFreeTextPathByteIdentical(t *testing.T) {
	for _, ty := range allGateActionTypes {
		a := GateAction{Type: ty, Branch: "feature/x", Detail: "ctx"}

		yes := &flatApprover{verdict: true}
		if got := GateStructured(a, yes); !got {
			t.Errorf("%s: approving flat approver ⇒ %v, want true", ty, got)
		}
		if yes.got != a.Describe() {
			t.Errorf("%s: flat approver got %q, want Describe() %q", ty, yes.got, a.Describe())
		}
		if got := GateStructured(a, &flatApprover{verdict: false}); got {
			t.Errorf("%s: denying flat approver ⇒ %v, want false", ty, got)
		}
		// nil approver default-denies (no ambient authority), unchanged.
		if got := GateStructured(a, nil); got {
			t.Errorf("%s: nil approver ⇒ %v, want false", ty, got)
		}
	}
}

// TestGateStructuredEvidenceInvisibleToUnawareApprover pins the optional
// carriage: an action CARRYING an Evidence payload hands an unaware approver the
// byte-identical flattened Describe() string — the payload never leaks into, or
// alters, the legacy path.
func TestGateStructuredEvidenceInvisibleToUnawareApprover(t *testing.T) {
	bare := GateAction{Type: PromoteToBase, Branch: "main", Detail: "ctx"}
	loaded := bare
	loaded.Evidence = &GateEvidence{
		Diffstat:    "1 file(s) changed, +2 −1",
		DiffExcerpt: "diff --git a/x b/x",
		VerifyTail:  "ok",
		SpentUSD:    1.25,
	}

	f := &flatApprover{verdict: true}
	if !GateStructured(loaded, f) {
		t.Fatal("verdict of the flat approver must govern")
	}
	if f.got != bare.Describe() {
		t.Errorf("unaware approver got %q, want the evidence-free Describe() %q", f.got, bare.Describe())
	}
	if loaded.Describe() != bare.Describe() {
		t.Errorf("Describe() must not change when Evidence is set: %q vs %q", loaded.Describe(), bare.Describe())
	}
}

// TestGateStructuredOptInDispatch proves an approver implementing
// StructuredApprover receives the structured action and the free-text path is NOT
// taken.
func TestGateStructuredOptInDispatch(t *testing.T) {
	f := &fakeStructured{verdict: true}
	if got := GateStructured(GateAction{Type: PromoteToBase, Branch: "staging"}, f); !got {
		t.Fatalf("structured approver verdict not returned: got %v", got)
	}
	if !f.calledStructured {
		t.Errorf("ApproveStructured was not called")
	}
	if f.calledFreeText {
		t.Errorf("free-text Approve must NOT be called when StructuredApprover is implemented")
	}
}
