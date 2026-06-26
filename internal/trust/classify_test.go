package trust

import "testing"

// classify_test.go — table-driven proof that Classify is a deterministic,
// case-insensitive, first-match-wins bucketer over the fixed label set, with
// "other" as the default. Classify is a pure new export; it changes no existing
// trust behaviour, so the "default path" golden here is simply: an unmatched
// goal (and the empty string) yields ClassOther, exactly as today's callers —
// which do not call Classify at all — would see no behaviour change.

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		goal string
		want string
	}{
		// refactor
		{"refactor verb", "Refactor the auth package", ClassRefactor},
		{"clean up phrase", "clean up the orchestrator imports", ClassRefactor},
		{"rename", "rename Foo to Bar across the repo", ClassRefactor},
		{"simplify", "simplify the retry loop", ClassRefactor},

		// bugfix
		{"bug fix phrase", "Bug fix: nil pointer in Replay", ClassBugfix},
		{"fix the", "fix the off-by-one in the cursor", ClassBugfix},
		{"regression", "track down the regression in routing", ClassBugfix},
		{"crash", "the server crash on empty input", ClassBugfix},

		// test
		{"add a test", "add a test for the ledger fold", ClassTest},
		{"unit test", "write a unit test covering Order", ClassTest},
		{"tests for", "tests for the selector seam", ClassTest},
		{"testing", "improve testing of the replay path", ClassTest},

		// docs
		{"document", "document the trust ledger invariants", ClassDocs},
		{"readme", "update the README with usage", ClassDocs},
		{"changelog", "add a changelog entry", ClassDocs},
		{"comment", "add a comment explaining the prior", ClassDocs},

		// greenfield (must beat feature on shared smell words)
		{"from scratch", "build a new CLI from scratch", ClassGreenfield},
		{"greenfield", "greenfield service for metrics", ClassGreenfield},
		{"scaffold", "scaffold a fresh module", ClassGreenfield},
		{"new project", "set up a new project for the daemon", ClassGreenfield},

		// feature
		{"implement", "implement a cost-aware router", ClassFeature},
		{"feature", "feature: add a /save command", ClassFeature},
		{"add support", "add support for OpenRouter", ClassFeature},
		{"introduce", "introduce a new pricing tier", ClassFeature},

		// research
		{"research", "research the best smoothing prior", ClassResearch},
		{"investigate", "investigate why the chain breaks", ClassResearch},
		{"compare", "compare Wilson vs Laplace scoring", ClassResearch},
		{"explore", "explore options for egress allowlists", ClassResearch},

		// default
		{"no keyword", "make it nice", ClassOther},
		{"empty", "", ClassOther},
		{"whitespace only", "   \t\n ", ClassOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.goal); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.goal, got, tt.want)
			}
		})
	}
}

// TestClassifyCaseInsensitive proves the same goal in different cases and with
// irregular whitespace maps to the same label (normalize lower-cases + collapses
// whitespace before matching).
func TestClassifyCaseInsensitive(t *testing.T) {
	variants := []string{
		"Refactor the package",
		"REFACTOR THE PACKAGE",
		"refactor the package",
		"\tRefactor\n\n  the   package  ",
	}
	const want = ClassRefactor
	for _, v := range variants {
		if got := Classify(v); got != want {
			t.Errorf("Classify(%q) = %q, want %q (case/whitespace insensitivity)", v, got, want)
		}
	}
}

// TestClassifyDeterministic proves Classify is a pure function: repeated calls on
// the same input return an identical result (no state, no IO, no model).
func TestClassifyDeterministic(t *testing.T) {
	goals := []string{
		"refactor and add a test and document it",
		"fix the bug and write a test",
		"build a new service from scratch with tests",
		"make it nice",
		"",
	}
	for _, g := range goals {
		first := Classify(g)
		for i := 0; i < 100; i++ {
			if got := Classify(g); got != first {
				t.Fatalf("Classify(%q) not deterministic: call %d = %q, first = %q", g, i, got, first)
			}
		}
	}
}

// TestClassifyFirstMatchWins proves a goal carrying several class smells resolves
// to the FIRST matching rule in table order, not whichever keyword appears first
// in the goal text — that is what makes the bucket a single function of the goal.
func TestClassifyFirstMatchWins(t *testing.T) {
	tests := []struct {
		name string
		goal string
		want string
	}{
		// "test" appears textually before "refactor", but refactor's rule is
		// earlier in the table, so refactor wins.
		{"test-word-first-but-refactor-rule-wins", "add a test after you refactor the module", ClassRefactor},
		// "document" comes textually first but bugfix's rule precedes docs.
		{"document-word-first-but-bugfix-rule-wins", "document the fix the regression introduced", ClassBugfix},
		// greenfield precedes feature: "from scratch" wins over "implement".
		{"greenfield-beats-feature", "implement a parser from scratch", ClassGreenfield},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.goal); got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.goal, got, tt.want)
			}
		})
	}
}

// TestClassifyLabelSetClosed proves every value Classify can return is one of the
// documented, closed label set — a tripwire if a future table edit introduces a
// stray label that the per-class routing cell would not recognize.
func TestClassifyLabelSetClosed(t *testing.T) {
	allowed := map[string]bool{
		ClassRefactor:   true,
		ClassBugfix:     true,
		ClassTest:       true,
		ClassDocs:       true,
		ClassGreenfield: true,
		ClassFeature:    true,
		ClassResearch:   true,
		ClassOther:      true,
	}
	for _, rule := range classTable {
		if !allowed[rule.class] {
			t.Errorf("classTable has rule with label %q not in the closed allowed set", rule.class)
		}
	}
	// And the default is in the set.
	if !allowed[Classify("nothing matches here at all")] {
		t.Errorf("default class is outside the closed allowed set")
	}
}
