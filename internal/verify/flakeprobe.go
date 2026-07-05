package verify

// flakeprobe.go — a decorator that gives a FLAKY test failure exactly one more
// chance to prove itself, without ever weakening what "done" means.
//
// WHY. The loop frequently re-verifies content that has not changed (requeues,
// convergence on an integration tip). When such a run fails in the TEST phase even
// though the identical content verified moments before, the most likely cause is a
// nondeterministic test (timing, ordering, port contention) — not the change. A
// flaky red wastes a whole loop iteration steering the agent at a phantom failure.
//
// WHY it is I2-safe. Both the original run and the probe re-run are executions of
// the REAL inner verifier — the sole authority on "done" (CLAUDE.md §2 I2). The
// probe never invents, upgrades, or replays a verdict: if the re-run passes, that
// pass was produced by the verifier itself; if it fails (or errors), the original
// failure stands untouched. The decorator can therefore only ever trade one extra
// verifier run for a recovered iteration — it can never green work the verifier
// did not green.
//
// WHY the conditions are narrow. A probe fires only when ALL hold:
//
//   - the inner verifier FAILED and the failure looks like a TEST failure — either
//     FailClass == test (enrich.go's structural first-line label) OR, when the recipe
//     was opaque to first-line sniffing (FailClass == "other"), the full output
//     carries a test-runner signature (isProbableTestFailure). A DETERMINISTIC
//     build/lint/browser red is never a flake candidate and is never probed;
//   - the worktree content hash equals the hash recorded at the IMMEDIATELY
//     preceding Check (nothing changed between runs, so "the change broke it" is
//     ruled out and "the test is flaky" is plausible);
//   - at most ONE probe per Check call (bounded — a persistently red flaky test
//     costs one extra run, never a retry loop).
//
// A confirmed flake (fail then pass on identical content) is surfaced through the
// nil-safe OnFlaky callback so the wiring layer can append an additive
// `verify_flaky` event (I5) — the audit trail records that a pass was reached via
// a probe, it is never silent.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// FlakeProbe decorates a Verifier with a single bounded re-run on a suspected
// flaky test failure. The zero value is not usable without Inner; Hash and OnFlaky
// are optional (nil Hash disables probing entirely — without a content hash we
// cannot prove "nothing changed", so we never probe on a guess).
type FlakeProbe struct {
	// Inner is the real verifier — the sole authority on "done". Every verdict this
	// decorator returns was produced by Inner. Required.
	Inner Verifier

	// Hash computes the content hash over everything Inner reads (the wiring layer
	// injects ContentHashWorktree over the worktree). It is an injected seam so the
	// package stays a leaf and tests stay hermetic. Nil ⇒ probing disabled.
	Hash func(ctx context.Context) (string, error)

	// OnFlaky, when non-nil, is called once per CONFIRMED flake (original run failed
	// as class `test`, probe re-run passed on identical content) with the structural
	// fail-class and the content hash. The wiring layer connects it to the event log
	// as an additive `verify_flaky` event (I5); nil means no observation is recorded
	// but the verdict is unchanged.
	OnFlaky func(failClass, contentHash string)

	// mu guards the previous-Check hash. One FlakeProbe instance is wired per
	// worktree verifier, but nothing stops concurrent Checks, so the memory of "the
	// immediately preceding Check" must be race-free.
	mu       sync.Mutex
	lastHash string
	hasLast  bool
}

// Check runs Inner and, on a test-class failure over content identical to the
// immediately preceding Check, re-runs Inner exactly once. A passing re-run IS the
// verdict (it came from the real verifier); a failing or erroring re-run leaves
// the original failure standing.
func (p *FlakeProbe) Check(ctx context.Context) (Report, error) {
	if p.Inner == nil {
		return Report{}, fmt.Errorf("flakeprobe: Inner verifier is required")
	}

	// Hash BEFORE running Inner: the verdict is about this content, so this is the
	// hash the NEXT Check compares against. A hash error is conservative: probing is
	// disabled for this call AND the stored hash is invalidated, so a later call can
	// never match against a hash we failed to compute.
	var (
		hash   string
		hashOK bool
	)
	if p.Hash != nil {
		if h, err := p.Hash(ctx); err == nil && h != "" {
			hash, hashOK = h, true
		}
	}
	p.mu.Lock()
	prev, hadPrev := p.lastHash, p.hasLast
	p.lastHash, p.hasLast = hash, hashOK
	p.mu.Unlock()

	rep, err := p.Inner.Check(ctx)
	if err != nil {
		return rep, err
	}
	if rep.Passed {
		return rep, nil
	}

	// Probe gate: identical content to the preceding Check + a test-class failure.
	// Anything else (first call, changed content, unhashable tree, build/lint/other
	// red) returns the real failure untouched.
	if !hashOK || !hadPrev || prev != hash {
		return rep, nil
	}
	if !isProbableTestFailure(rep) {
		return rep, nil
	}

	// The single bounded probe: one more run of the REAL verifier, nothing else.
	rep2, err2 := p.Inner.Check(ctx)
	if err2 != nil || !rep2.Passed {
		// The probe never worsens the outcome: an erroring or still-red re-run leaves
		// the ORIGINAL failure as the verdict (the fail stands).
		return rep, nil
	}
	if p.OnFlaky != nil {
		p.OnFlaky(FailClassTest, hash)
	}
	return rep2, nil
}

// testRunnerSignatures are output shapes that mark a TEST-phase failure regardless of
// what recipe line drove it. FailClass sniffs only the first command token of the
// first line, so a test flake behind an opaque recipe (e.g. `make verify` whose first
// output line is `make: *** [Makefile:12: test] Error 1`, or a wrapper script) is
// classified as `other`/`build` and never probed. These substrings — the canonical
// banners of go test, pytest, jest/vitest, and the make/target envelope around a test
// target — recover those cases. They are matched as OPAQUE DATA to decide WHETHER to
// re-run the real verifier; no byte is ever interpreted as an instruction (I7), and a
// match only ever GRANTS one extra verifier run — the verifier alone still decides the
// verdict (I2).
var testRunnerSignatures = []string{
	"--- fail:",     // go test failing test banner
	"--- FAIL:",     // go test (as-emitted case)
	"=== run",       // go test run marker
	"=== RUN",       // (as-emitted case)
	"panic: test",   // a panic inside a test binary
	"failures=",     // pytest short summary ("2 passed, 1 failed" also below)
	" failed,",      // pytest / jest summary fragment ("1 failed, 3 passed")
	" failed]",      // make target envelope "[test] failed]"-style
	"tests:",        // jest/vitest "Tests: 1 failed"
	"test suites:",  // jest "Test Suites: 1 failed"
	"pytest",        // pytest invocation echoed anywhere in the output
	"go test",       // the go test command echoed on a later (non-first) line
	"npm test",      // npm test echoed anywhere
	"[test] error",  // make: *** [Makefile:NN: test] Error 1
	"target `test'", // make "No rule to make target" / target-scoped errors
	"running tests", // generic runner banner
}

// testTargetRe matches a make/just/task envelope that names a *test* target, e.g.
// "make: *** [test] Error 1", "[Makefile:12: unit-test] Error 2", so a test failure
// hidden behind a make recipe (whose first line is the make wrapper, not `go test`)
// is still recognized. It is anchored to the bracketed-target + Error-code shape make
// emits, not free prose, to keep it from matching arbitrary output.
var testTargetRe = regexp.MustCompile(`(?i)\[[^\]]*test[^\]]*\]\s+error\b`)

// isProbableTestFailure reports whether rep is a red that should be treated as a
// (potentially flaky) TEST failure. It first trusts the structural FailClass; when
// that is unavailable (the failing command hid behind an opaque recipe, so FailClass
// returned "other"), it falls back to scanning the whole output for a test-runner
// signature. It deliberately does NOT override a DETERMINISTIC non-test class
// (build/lint/browser) — those verdicts are compiler/analyzer facts, never flakes, so
// a build red that merely mentions the word "test" is not probed.
func isProbableTestFailure(rep Report) bool {
	if rep.Passed {
		return false
	}
	switch FailClass(rep) {
	case FailClassTest:
		return true
	case FailClassBuild, FailClassLint, FailClassBrowser:
		// A deterministic, structurally-classified non-test red — never a flake.
		return false
	default:
		// FailClassUnknown ("other"): the recipe was opaque to first-line sniffing.
		// Scan the full output for a test-runner signature before giving up.
		return outputLooksLikeTestFailure(rep.Output)
	}
}

// outputLooksLikeTestFailure scans the ENTIRE output (not just the first recipe line)
// for a test-runner signature. It is the broadened detection the fix adds: a test
// flake behind an opaque `make verify` recipe is now probed, where first-line sniffing
// alone would never have seen the `go test` banner buried below the make wrapper.
func outputLooksLikeTestFailure(output string) bool {
	lower := strings.ToLower(output)
	for _, sig := range testRunnerSignatures {
		if strings.Contains(lower, strings.ToLower(sig)) {
			return true
		}
	}
	return testTargetRe.MatchString(lower)
}
