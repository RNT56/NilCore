package verify

// tiered.go — a decorator that lets a CHEAP scoped check short-circuit a RED
// verdict before paying for the full project verify, while the full verifier
// remains the ONLY possible source of a PASS.
//
// WHY. Most loop iterations are red: the agent edits, verifies, reads the failure,
// edits again. Each of those reds today costs a full `make verify` of the whole
// world, even when the failure is confined to the one package that was touched. A
// targeted vet/test over just the touched packages finds the same red in a
// fraction of the time.
//
// WHY it is I2-safe. The asymmetry is the whole design:
//
//   - A scoped RED may ship as the verdict ONLY when it is a PROVABLE subset of the
//     full verify — a genuine test failure or compile error in a package the full
//     `go test ./...` would itself have compiled and run. The full run could only
//     have found the same red, slower. The output is prefixed with ScopedRedMarker
//     (and tail-bounded) so an operator can always tell a fast-check red from a
//     full-verify red.
//   - A scoped GREEN decides NOTHING. It falls through to Full, and only Full can
//     produce a PASS. The scoped check therefore cannot green anything the project
//     verifier would not green: the verifier stays the sole authority on "done"
//     (CLAUDE.md §2 I2).
//   - A scoped ERROR or AMBIGUOUS nonzero exit (git unavailable, unscopable change,
//     sandbox fault, a package-LOAD/resolution error a nested go.mod would raise, a
//     vet-only nit the full command would not gate) also falls through to Full — an
//     inconclusive fast path never becomes a verdict. Correctness beats latency:
//     when we cannot PROVE the red is the project's red, we pay for the full verify.
//
// WHY it is OPT-IN (NILCORE_TIERED_VERIFY=1, default OFF). A generically-sound
// subset check is not feasible — "make verify" is an opaque recipe (it might run no
// tests, or `go test -short`, or a bespoke script), so a scoped `go vet`/`go test`
// red can diverge from what that recipe actually gates. Rather than red-flakes ship
// as the verdict by default, the wrap arms ONLY when the operator opts in AND the
// resolved verify command is itself a full-module `go test ./...`-family invocation
// whose flags we can replicate (see cmd/nilcore/verifier.go: tieredSound).
//
// The seam is an injected func so this package stays a leaf (no sandbox, worktree,
// or orchestrator import); the wiring layer (cmd/nilcore/verifier.go) composes the
// touched-package discovery and the scoped commands through the same sandbox exec
// path the full verifier uses, replicates the full command's go-test flags, and
// gates the wrap on the resolved verify command being a full `go test ./...` run.

import (
	"context"
	"fmt"
)

// ScopedRedMarker is the first line of every Report produced by the scoped fast
// path. Operators (and the loop's failure feedback) can tell at a glance that the
// red came from the targeted check and the full verify was skipped.
const ScopedRedMarker = "[scoped fast-check red — full verify skipped]"

// TieredVerifier runs a cheap scoped red-detector before the full verifier.
// Nil ScopedRed ⇒ byte-identical passthrough to Full. Only Full can ever PASS.
type TieredVerifier struct {
	// Full is the project's real verifier — the sole authority on "done" and the
	// only possible source of a passing Report. Required.
	Full Verifier

	// ScopedRed is the injected fast check. failed=true means "a PROVABLE subset of
	// the full verify is already red — a genuine test failure or compile error in a
	// touched package" and short-circuits with output; failed=false OR err != nil
	// means "inconclusive" (green, unscopable, or an ambiguous nonzero exit such as a
	// package-load error) and falls through to Full.
	ScopedRed func(ctx context.Context) (failed bool, output string, err error)
}

// scopedOutputTail bounds the scoped red body fed back to the agent, matching the
// 4000-byte tail every full-verify red enforces (verify.go). WHY: the scoped body
// is a raw sandbox `go test` dump — unbounded, it would land verbatim in the model
// conversation and blow past the budget every other red respects. We keep the tail
// (the failing assertions/compile errors live at the end of go-test output) and
// clip the head, then re-prefix the marker so an operator still sees at a glance
// that this red came from the fast path.
const scopedOutputTail = 4000

// Check runs ScopedRed first and returns its red immediately (marked); otherwise
// the verdict is exactly Full's.
func (t *TieredVerifier) Check(ctx context.Context) (Report, error) {
	if t.Full == nil {
		return Report{}, fmt.Errorf("tiered: Full verifier is required")
	}
	if t.ScopedRed == nil {
		return t.Full.Check(ctx)
	}
	failed, out, err := t.ScopedRed(ctx)
	if err == nil && failed {
		return Report{Passed: false, Output: ScopedRedMarker + "\n" + tail(out, scopedOutputTail)}, nil
	}
	// Scoped green or scoped error: inconclusive either way — only the full
	// verifier may decide, and only it may PASS (I2).
	return t.Full.Check(ctx)
}
