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
//   - A scoped RED may ship as the verdict, because (under the wiring layer's
//     soundness gate) a scoped failure is a strict subset of what the full verify
//     would run — the full run could only have found the same red, slower. The
//     output is prefixed with ScopedRedMarker so an operator can always tell a
//     fast-check red from a full-verify red.
//   - A scoped GREEN decides NOTHING. It falls through to Full, and only Full can
//     produce a PASS. The scoped check therefore cannot green anything the project
//     verifier would not green: the verifier stays the sole authority on "done"
//     (CLAUDE.md §2 I2).
//   - A scoped ERROR (git unavailable, unscopable change, sandbox fault) also
//     falls through to Full — an inconclusive fast path never becomes a verdict.
//
// The seam is an injected func so this package stays a leaf (no sandbox, worktree,
// or orchestrator import); the wiring layer (cmd/nilcore/verifier.go) composes the
// touched-package discovery and the scoped commands through the same sandbox exec
// path the full verifier uses, and gates the wrap on the resolved verify command
// actually running Go tests (the soundness condition above).

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

	// ScopedRed is the injected fast check. failed=true means "a strict subset of
	// the full verify is already red" and short-circuits with output; failed=false
	// or err != nil means "inconclusive" and falls through to Full.
	ScopedRed func(ctx context.Context) (failed bool, output string, err error)
}

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
		return Report{Passed: false, Output: ScopedRedMarker + "\n" + out}, nil
	}
	// Scoped green or scoped error: inconclusive either way — only the full
	// verifier may decide, and only it may PASS (I2).
	return t.Full.Check(ctx)
}
