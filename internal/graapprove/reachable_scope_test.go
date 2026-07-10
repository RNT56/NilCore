package graapprove

import "testing"

// The shipped presets allow "*". Every live gate scope is a slash-y branch, so if `*`
// does not admit them, graduated auto-approval is structurally dead for its two live
// classes (open-pr, promote-to-base).
func TestStarAdmitsRealBranchScopes(t *testing.T) {
	real := []string{
		"task/trig-1720512345", // watch/schedule open-pr
		"task/P1-T03",          // worktree.Create
		"integrate/abc1234",    // swarm promote tip
		"feat/x",
		"id@cmd-hash", // bind-self-authored
	}
	for _, s := range real {
		if !matchAny(s, []string{"*"}) {
			t.Errorf("matchAny(%q, [\"*\"]) = false — the preset envelope admits nothing and the feature is unreachable", s)
		}
	}
}

// An action with no target has no bounded blast radius and must never auto-approve.
// Whitespace counts as no target: isProd/isProtectedBase both trim, so a " " scope
// would otherwise clear the protected-base floor and then match a lone `*`.
func TestEmptyScopeNeverMatches(t *testing.T) {
	for _, scope := range []string{"", " ", "\t", "  \n "} {
		for _, pats := range [][]string{{"*"}, {"feat/*"}, {}} {
			if matchAny(scope, pats) {
				t.Errorf("matchAny(%q, %v) = true, want false (fail-closed: no target, no bounded blast)", scope, pats)
			}
		}
	}
}

// A deliberate, structured pattern keeps segment-local path.Match semantics — widening
// `*` must not silently widen "feat/*" into a cross-segment wildcard.
func TestStructuredPatternsStaySegmentLocal(t *testing.T) {
	if !matchAny("feat/x", []string{"feat/*"}) {
		t.Error(`matchAny("feat/x", ["feat/*"]) = false, want true`)
	}
	if matchAny("feat/x/y", []string{"feat/*"}) {
		t.Error(`matchAny("feat/x/y", ["feat/*"]) = true, want false (path.Match semantics preserved)`)
	}
	if matchAny("other/x", []string{"feat/*"}) {
		t.Error(`matchAny("other/x", ["feat/*"]) = true, want false`)
	}
}

// Trust and the rate window must accrue over a STABLE family, or a per-run-unique scope
// makes MinSuccesses unsatisfiable and MaxPerDay unenforceable.
func TestTrustScopeCollapsesEphemeralScopes(t *testing.T) {
	cases := map[string]string{
		"task/trig-1720512345": "task/*",
		"task/trig-1720599999": "task/*", // a later run lands in the SAME family
		"task/P1-T03":          "task/*",
		"feat/a/b":             "feat/a/*",
		"integrate/abc1234":    "integrate/*",
		"9f3c1ab":              "#commit",
		"9f3c1ab2d4e5f60718293a4b5c6d7e8f90123456": "#commit",
		"main":        "main",
		"id@cmd-hash": "id@cmd-hash",
		"":            "",
	}
	for in, want := range cases {
		if got := trustScope(in); got != want {
			t.Errorf("trustScope(%q) = %q, want %q", in, got, want)
		}
	}

	// Two distinct runs of the same shape must share a trust bucket — otherwise Green
	// can never reach MinSuccesses.
	if trustScope("task/trig-1") != trustScope("task/trig-2") {
		t.Fatal("two task branches key to different trust families; trust can never accrue")
	}
}

// The floor is independent of the envelope: `*` admitting everything must not let a
// protected base through.
func TestProtectedBasesStillDeniedUnderStar(t *testing.T) {
	for _, s := range []string{"main", "master", "release", "release/1.2", "release-1.2", "trunk", "stable"} {
		if !isProtectedBase(s) {
			t.Errorf("isProtectedBase(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"prod", "production", "prod-eu"} {
		if !isProd(s) {
			t.Errorf("isProd(%q) = false, want true", s)
		}
	}
	// And the family collapse must not launder a protected base into an allowed one.
	if trustScope("main") != "main" {
		t.Error("trustScope must not rewrite a protected base")
	}
}

func TestIsCommitSHA(t *testing.T) {
	yes := []string{"9f3c1ab", "abcdef1234", "9f3c1ab2d4e5f60718293a4b5c6d7e8f90123456"}
	no := []string{"", "abc", "main", "task", "9f3c1ag", "9f3c1ab2d4e5f60718293a4b5c6d7e8f901234567"}
	for _, s := range yes {
		if !isCommitSHA(s) {
			t.Errorf("isCommitSHA(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isCommitSHA(s) {
			t.Errorf("isCommitSHA(%q) = true, want false", s)
		}
	}
}
