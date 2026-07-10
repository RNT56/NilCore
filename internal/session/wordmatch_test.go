package session

import (
	"testing"

	"nilcore/internal/inbox"
)

// referencesGoal must not re-enter the active driver on words that merely CONTAIN
// a continuation verb — the substring footgun ("format" ⊂ "information") this
// codebase has already had to fix in policy and desktopagent.
func TestReferencesGoalContinuationVerbsAreWordBounded(t *testing.T) {
	const goal = "refactor the payment ledger"

	// These share NO significant word with the goal, so the only rule that could
	// fire is the continuation-verb rule — which must not match a mere substring.
	notContinuations := []string{
		"discontinue everything", // ⊃ "continue"
		"put the logo online",    // ⊃ "go on"
		"presume nothing",        // ⊃ "resume"
	}
	for _, text := range notContinuations {
		if referencesGoal(text, goal) {
			t.Errorf("referencesGoal(%q) = true, want false (substring match, not a word)", text)
		}
	}

	continuations := []string{
		"continue",
		"keep going",
		"carry on",
		"go on",
		"finish it",
		"resume please",
	}
	for _, text := range continuations {
		if !referencesGoal(text, goal) {
			t.Errorf("referencesGoal(%q) = false, want true (explicit continuation verb)", text)
		}
	}
}

// A steer's control prefix addresses the harness; the model must receive the
// instruction without it.
func TestStripInterruptMarker(t *testing.T) {
	cases := map[string]string{
		"! stop and run the tests": "stop and run the tests",
		"!stop":                    "stop",
		"  !  stop":                "stop",
		"/steer stop":              "stop",
		"/steer":                   "",
		"!":                        "",
		"no marker here":           "no marker here",
	}
	for in, want := range cases {
		if got := stripInterruptMarker(in); got != want {
			t.Errorf("stripInterruptMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

// The classifier and the stripper must agree on what a marker is: every text the
// classifier calls a Steer must have its marker removed, and no Queue text may be
// altered.
func TestStripAgreesWithClassify(t *testing.T) {
	steers := []string{"! go", "!go", "/steer go", "/steer"}
	for _, s := range steers {
		if classifyInterrupt(s) != inbox.Steer {
			t.Fatalf("classifyInterrupt(%q) is not Steer; test premise broken", s)
		}
		if got := stripInterruptMarker(s); got == s {
			t.Errorf("stripInterruptMarker(%q) left the marker in place", s)
		}
	}
	queues := []string{"go", "run the tests", "/save notes.md"}
	for _, q := range queues {
		if classifyInterrupt(q) == inbox.Steer {
			t.Fatalf("classifyInterrupt(%q) is Steer; test premise broken", q)
		}
	}
}
