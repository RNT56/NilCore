package policy

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]Class{
		"edit main.go and run go test":   Reversible,
		"git push --force origin main":   Irreversible,
		"kubectl apply -f deploy.yaml":   Irreversible,
		"go build ./...":                 Reversible,
		"delete from users where id=1":   Irreversible,
		"write a new file internal/x.go": Reversible,
	}
	for action, want := range cases {
		if got := Classify(action); got != want {
			t.Errorf("Classify(%q) = %v, want %v", action, got, want)
		}
	}
}

// TestClassifyWordBoundary proves signals match on word boundaries, so a larger word
// that merely CONTAINS a signal does not spuriously gate (the old substring match
// flagged "display"→pay, "merger"→merge, "curly"→curl, "recharge"→charge).
func TestClassifyWordBoundary(t *testing.T) {
	reversible := []string{
		"update the display settings",         // contains "pay"? no — but "display" historically tripped substr
		"write the merger analysis doc",       // "merger" must NOT match "merge"
		"render the payload schema",           // "payload" must NOT match "pay"
		"recharge is documented in README",    // "recharge" must NOT match "charge"
		"the curly braces need fixing",        // "curly" must NOT match "curl"
		"mark the field transferable in docs", // "transferable" must NOT match "transfer"
	}
	for _, a := range reversible {
		if got := Classify(a); got != Reversible {
			t.Errorf("Classify(%q) = %v, want Reversible (no whole-word signal)", a, got)
		}
	}
	// The bare signals still gate when they appear as whole words.
	irreversible := []string{
		"git merge feature into main",
		"pay the invoice now",
		"curl https://example.com/data",
		"transfer the funds",
	}
	for _, a := range irreversible {
		if got := Classify(a); got != Irreversible {
			t.Errorf("Classify(%q) = %v, want Irreversible (whole-word signal present)", a, got)
		}
	}
}

// TestClassifyWhitespaceEvasion proves padded irreversible signals are still
// caught (audit L4).
func TestClassifyWhitespaceEvasion(t *testing.T) {
	for _, action := range []string{
		"git  push origin main",
		"git\tpush --force",
		"please  deploy   to prod",
		"kubectl   apply -f x.yaml",
	} {
		if got := Classify(action); got != Irreversible {
			t.Errorf("Classify(%q) = %v, want irreversible", action, got)
		}
	}
}
