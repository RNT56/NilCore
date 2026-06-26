package policy

import (
	"strings"
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

// TestGateStructuredFreeTextPathByteIdentical proves a non-StructuredApprover
// (here a ConsoleApprover) still reaches the exact prior behaviour: the free-text
// Approve is consulted and its verdict governs, for every GateActionType, with a
// nil approver default-denying. This is the byte-identical default-off proof.
func TestGateStructuredFreeTextPathByteIdentical(t *testing.T) {
	for _, ty := range allGateActionTypes {
		a := GateAction{Type: ty, Branch: "feature/x", Detail: "ctx"}

		// "y" ⇒ approve, anything else ⇒ deny — the ConsoleApprover contract.
		if got := GateStructured(a, NewConsoleApprover(strings.NewReader("y\n"), &strings.Builder{})); !got {
			t.Errorf("%s: console 'y' ⇒ %v, want true", ty, got)
		}
		if got := GateStructured(a, NewConsoleApprover(strings.NewReader("n\n"), &strings.Builder{})); got {
			t.Errorf("%s: console 'n' ⇒ %v, want false", ty, got)
		}
		// nil approver default-denies (no ambient authority), unchanged.
		if got := GateStructured(a, nil); got {
			t.Errorf("%s: nil approver ⇒ %v, want false", ty, got)
		}
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
