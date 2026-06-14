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
