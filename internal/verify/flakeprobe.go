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
//   - the inner verifier FAILED with FailClass == test (a structural label from
//     enrich.go's fixed vocabulary — build/lint/browser reds are deterministic
//     compiler/analyzer verdicts, not flake candidates);
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
	if FailClass(rep) != FailClassTest {
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
