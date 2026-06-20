package swarm

// invariant.go — the executable invariant guards for the swarm leaf.
//
// These three guards turn the swarm's safety claims into runnable predicates so
// the proofs are tested, not asserted in prose:
//
//   - ShipGate (I2): no shard ships on a vacuous verifier. A nil verifier or the
//     always-true verify.Pass{} is refused at construction, so the sole authority
//     on "done" can never be a stub.
//   - ClassifyCeiling (budget): a budget.ErrCeiling caught at the shard boundary
//     is classified as per-shard vs global by a tiny headroom probe, so the
//     runner knows whether to fail one shard or stop the whole run.
//   - ProjectTrusted (I7): the scoreboard/trace projection carries ONLY
//     verifier-set, key-free fields — never the model-authored Value/Statement —
//     so an injection phrase in a claim's Value can never reach a renderer.

import (
	"context"
	"errors"

	"nilcore/internal/artifact"
	"nilcore/internal/budget"
	"nilcore/internal/verify"
)

// ErrNoShipVerifier is returned by NewShipGate when the supplied verifier cannot
// be a real authority on "done": a nil interface or the vacuous verify.Pass{}.
var ErrNoShipVerifier = errors.New("swarm: ship gate requires a real verifier (nil or verify.Pass refused)")

// ShipGate is the per-shard ship decision. It is a TRANSPARENT pass-through of
// the underlying verifier's verdict — it adds no judgment of its own — but it can
// only be CONSTRUCTED around a verifier that actually decides something. That
// construction check is the I2 guard: a shard cannot ship on a stub.
type ShipGate struct {
	v verify.Verifier
}

// NewShipGate wraps v in a ShipGate, refusing the two verifiers that would make
// "done" vacuous: a nil interface (no authority at all) and verify.Pass{} (the
// always-true read-only verifier, which ships nothing and must never gate work
// that SHIPS). verify.Pass is detected by a type assertion rather than by calling
// Check — a stub that always returns Passed must be refused at construction, not
// trusted at runtime. Fail-closed: any verifier we cannot positively accept is
// rejected with ErrNoShipVerifier.
func NewShipGate(v verify.Verifier) (ShipGate, error) {
	if v == nil {
		return ShipGate{}, ErrNoShipVerifier
	}
	// A typed-nil interface (e.g. a (*CommandVerifier)(nil)) is non-nil at the
	// interface level but would panic on Check; reject the concrete nil pointer
	// types we know about is out of scope here — the documented refusal is the
	// vacuous verify.Pass value, caught by this assertion.
	if _, isPass := v.(verify.Pass); isPass {
		return ShipGate{}, ErrNoShipVerifier
	}
	return ShipGate{v: v}, nil
}

// Check forwards verbatim to the underlying verifier. The gate's only safety
// contribution is the construction-time refusal in NewShipGate; once built, it
// neither softens nor strengthens the verdict (I2 — the verifier is the sole
// authority, and this gate just guarantees the authority is real).
func (g ShipGate) Check(ctx context.Context) (verify.Report, error) {
	return g.v.Check(ctx)
}

// BudgetScope classifies a budget.ErrCeiling caught at the shard boundary so the
// runner can react proportionally.
type BudgetScope string

const (
	// ScopeNone means the run error was not a ceiling breach (or no exhaustion
	// could be reproduced) — nothing budget-related to do.
	ScopeNone BudgetScope = "none"
	// ScopeShard means the shard's own per-task ceiling is exhausted while global
	// headroom remains: fail/maybe-requeue THIS shard, keep the run going.
	ScopeShard BudgetScope = "shard"
	// ScopeGlobal means the global ceiling is exhausted: stop the whole run, no
	// shard can make progress.
	ScopeGlobal BudgetScope = "global"
)

// ceilingProbe is the smallest dollar amount we charge to test for headroom. The
// budget.Ledger refuses a charge that would push spend strictly past the ceiling
// (it tolerates landing exactly on it, within epsilon), so a positive probe is
// what makes "no headroom left" observable: at a ceiling, spend+probe > ceiling
// trips ErrCeiling. We keep it as small as the ledger's own epsilon tolerance so
// a SUCCESSFUL probe perturbs the ledger by a negligible, sub-cent amount.
//
// Limitation (documented per the spec): the shipped budget.Ledger has no
// non-recording headroom check — Charge is the only ceiling-aware entry, and a
// SUCCEEDING charge records its amount. A FAILING probe records nothing (the
// ErrCeiling path rejects before recording), which is the case ClassifyCeiling
// acts on; a succeeding probe leaves this negligible residue and is otherwise
// ignored. The probe is zero-token throughout, so it never distorts the token
// tally that drives cost reporting.
const ceilingProbe = 2e-9 // just above budget's 1e-9 epsilon

// probeTask is a reserved, ceiling-free task name used to test ONLY the global
// ceiling: it carries no per-task ceiling, so a charge against it can be refused
// only by the global wall. (A leading control-ish prefix keeps it from colliding
// with any real shard key, which are "swarm/<runID>/<n>".)
const probeTask = "\x00swarm-global-headroom-probe"

// ClassifyCeiling distinguishes a per-shard ceiling breach from a global one for
// a runErr caught at the shard boundary. It probes with a zero-token, tiny-dollar
// charge:
//
//  1. If runErr is not budget.ErrCeiling, there is nothing to classify ⇒ ScopeNone.
//  2. Probe the GLOBAL ceiling via a ceiling-free probe task. If that is refused,
//     the global wall is the binding constraint ⇒ ScopeGlobal.
//  3. Otherwise probe the SHARD key. Because step 2 just showed global headroom
//     exists, a refusal here can only be the shard's per-task ceiling ⇒ ScopeShard.
//  4. If neither probe reproduces exhaustion (e.g. the breach was transient or the
//     ceilings were since relaxed), fail-safe to ScopeNone rather than stopping
//     the run on an unproven global breach.
//
// A nil ledger has no ceilings to breach, so a ceiling error cannot be attributed
// ⇒ ScopeNone.
func ClassifyCeiling(ctx context.Context, led *budget.Ledger, shardKey string, runErr error) BudgetScope {
	if !errors.Is(runErr, budget.ErrCeiling) || led == nil {
		return ScopeNone
	}
	// Probe the global ceiling in isolation: the probe task has no per-task
	// ceiling, so only the global wall can refuse this charge.
	if err := led.Charge(ctx, probeTask, 0, ceilingProbe); errors.Is(err, budget.ErrCeiling) {
		return ScopeGlobal
	}
	// Global has headroom; a refusal on the shard key is therefore its own ceiling.
	if err := led.Charge(ctx, shardKey, 0, ceilingProbe); errors.Is(err, budget.ErrCeiling) {
		return ScopeShard
	}
	return ScopeNone
}

// TrustedClaim is the I7-safe projection of one claim for the scoreboard and the
// source–claim trace. It DELIBERATELY HAS NO Value (and no Statement) field: the
// model-authored datum and prose are untrusted and must never reach a renderer.
// What it carries is exactly the trusted/provenance set — the verifier-set Status
// and verifier-id, the claim identity, the semantic Field, and the key-free
// SourceURL. SourceURL is provenance only (it is required to be key-free, I3), so
// it is safe to display as "where this came from" without echoing any asserted
// value.
type TrustedClaim struct {
	ClaimID   string          // stable claim identity (requeue key)
	Field     string          // semantic label (e.g. "revenue_q4")
	Verifier  string          // verifier-id that produced the verdict
	Status    artifact.Status // verifier-set verdict — the only trusted pass/fail
	SourceURL string          // key-free provenance URL (NOT the asserted value)
}

// ProjectTrusted projects an artifact's claims to the I7-safe TrustedClaim set.
// It reads ONLY Status/Verifier/SourceURL from each claim's Evidence plus the
// claim's own ID/Field — and NEVER Value or Statement. This is the executable I7
// guard: a malicious or accidental injection phrase living in a claim's Value (or
// the Statement prose) is structurally unable to appear in the projection,
// because the projection has no field to hold it. A nil artifact projects to a
// nil slice.
func ProjectTrusted(a *artifact.Artifact) []TrustedClaim {
	if a == nil {
		return nil
	}
	out := make([]TrustedClaim, 0, len(a.Claims))
	for i := range a.Claims {
		c := a.Claims[i]
		out = append(out, TrustedClaim{
			ClaimID:   c.ID,
			Field:     c.Field,
			Verifier:  c.Evidence.Verifier,
			Status:    c.Evidence.Status,
			SourceURL: c.Evidence.SourceURL,
		})
	}
	return out
}
