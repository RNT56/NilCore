// Package self freezes the agent's own self-evaluation suite (SIF-T01).
//
// The self-improvement flywheel (Pillar 4) runs a FIXED set of eval cases on the
// agent to measure whether a candidate self-improvement actually improved pass
// rate, not vibes. For that signal to be honest the eval set itself must be
// immutable: a candidate that could silently rewrite the cases — drop the ones it
// fails, or weaken a goal — would game its own standing. That is the C6
// feedback-loop pathology the roadmap calls out ("no self-modification of the eval
// set"). So this package pins the suite as in-binary data and pairs it with a
// content hash over the canonical serialization of every case. SIF-T02/T08 fold
// the *run's* verifier-judged outcomes to trust; this leaf only freezes the suite
// and its identity — it makes NO model or network call.
//
// This is a LEAF: it imports only the eval package (for the Case shape) and the
// standard library. deps_test.go enforces that closure.
package self

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"nilcore/eval"
)

// Suite is a frozen, content-hashed set of self-eval cases plus a human-readable
// version label. The zero value is not meaningful; obtain a Suite via Load.
type Suite struct {
	// Version is a stable label for this frozen revision of the suite. It is part
	// of the hashed content, so bumping it (intentionally) yields a new identity.
	Version string `json:"version"`
	// Cases is the immutable, ordered set of self-eval cases. Callers MUST treat
	// the returned slice as read-only; Load hands out a fresh copy so a caller
	// cannot mutate the frozen original.
	Cases []eval.Case `json:"cases"`
}

// frozenVersion identifies this revision of the self-eval suite. Bump it only
// when deliberately re-freezing the set (which by design changes Hash).
const frozenVersion = "selfeval-v1"

// frozenCases is the fixed, deterministic self-eval set. Each case is a goal the
// agent must achieve, scored later by an OBJECTIVE verifier (SIF-T02 wires the
// run) — never by the model's self-report. The goals are deliberately small,
// self-contained, and verifier-checkable so the suite is a stable yardstick. This
// is data only: editing this list is the ONLY sanctioned way to change the suite,
// and doing so changes Hash, which is exactly the tamper-evidence we want.
var frozenCases = []eval.Case{
	{
		Name: "add-pure-function",
		Goal: "Add an exported Go function Add(a, b int) int that returns a+b, with a passing table-driven unit test. The package must build and `go test` must pass.",
	},
	{
		Name: "fix-off-by-one",
		Goal: "A loop iterates `for i := 0; i <= len(s); i++` and panics with an index-out-of-range. Fix the bound so the existing failing test passes; change nothing else.",
	},
	{
		Name: "wrap-error-context",
		Goal: "A function returns a bare error from an I/O call. Wrap it with `fmt.Errorf(\"...: %w\", err)` so the cause is preserved, and make the errors.Is test pass.",
	},
	{
		Name: "honor-context-cancel",
		Goal: "A blocking helper ignores its context. Make it return ctx.Err() promptly on cancellation so the test using a cancelled context passes.",
	},
	{
		Name: "table-driven-refactor",
		Goal: "Refactor three near-duplicate test functions into one table-driven test with named subtests, preserving coverage; `go test` must still pass.",
	},
	{
		Name: "gofmt-clean",
		Goal: "A source file has inconsistent indentation and import grouping. Make it gofmt- and goimports-clean without changing behavior; the build and tests stay green.",
	},
}

// Load returns the frozen self-eval suite together with its content hash.
//
// The returned Suite holds a defensive copy of the cases, so mutating it never
// affects the in-binary original or a later Load. Hash returns the same value on
// every call for the same frozen content; any edit to frozenCases or
// frozenVersion changes it. Load makes no model or network call and never fails
// for the in-binary suite — err is returned only to keep the seam honest for a
// future externally-loaded suite and is always nil today.
func Load() (Suite, string, error) {
	cases := make([]eval.Case, len(frozenCases))
	copy(cases, frozenCases)
	s := Suite{Version: frozenVersion, Cases: cases}
	h, err := s.Hash()
	if err != nil {
		return Suite{}, "", fmt.Errorf("hashing frozen self-eval suite: %w", err)
	}
	return s, h, nil
}

// Hash returns a stable, content-addressed identity for the suite: the SHA-256 of
// its canonical JSON serialization, hex-encoded. It is deterministic across calls
// and across processes (Go preserves struct-field order and slice order, and the
// suite is fully ordered), so it can pin the suite's identity in the event log.
// Mutating any case — name, goal, ordering, or the version label — changes Hash,
// which is the C6 tamper guard: a candidate cannot silently alter the eval set.
func (s Suite) Hash() (string, error) {
	// json.Marshal is deterministic here: struct fields serialize in declaration
	// order and the Cases slice preserves its order, so the bytes are stable.
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshaling suite for hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
