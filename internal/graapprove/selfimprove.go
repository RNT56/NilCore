package graapprove

import "os"

// EnvSelfImproveAutoApprove is the SEPARATE, double-opt-in env that lets the agent
// merge an edit to its OWN prompts/skills without a human (Phase 16, SIF-T07 — the
// docs/ROADMAP-CLOSED-LOOP.md §10 decision-2 relaxation, DISTINCT from enabling the
// flywheel). It is deliberately NOT implied by any other flag or env: each powerful
// relaxation needs its OWN recorded gate (no transitive opt-in — XC-T02).
const EnvSelfImproveAutoApprove = "NILCORE_SELFIMPROVE_AUTOAPPROVE"

// SelfImproveGate wraps the human gate the self-improvement flow consults before
// merging a self-edit. It is the self-improve auto-approval CLASS — a narrow,
// single-boundary sibling of the GradedApprover, NOT a widening of it (the
// GradedApprover NEVER auto-approves a free-text gate, §5; this is a dedicated class
// for the one self-edit boundary, gated by its own env).
//
// WHY auto-approving here is safe ONLY behind the double opt-in:
//   - By the time this gate runs, selfimprove.Flow has ALREADY proven the edit is
//     verifier-green (I2), and in the flywheel the loop's measured-delta fence has
//     accepted it; nothing ships that the verifier did not pass.
//   - selfimprove.DefaultScope GUARANTEES the edit cannot touch the verifier of
//     record, the core loop, or any contract file (it permits only prompts/skills/
//     tools), so this class can only ever merge a prompt/skill edit that earned its
//     place. The flywheel may NEVER author the verifier-of-record (charter §0).
//   - It is reversible: a merged prompt/skill edit is a normal commit that can be
//     reverted.
//
// DEFAULT-OFF byte-identical: with EnvSelfImproveAutoApprove unset it delegates to the
// human gate UNCHANGED. When set, it auto-approves the earned edit and emits an
// audited auto_approve_selfimprove event (via the optional sink) so a merged self-edit
// is NEVER silent.
func SelfImproveGate(human func(action string) bool, sink Sink) func(action string) bool {
	if human == nil {
		// Deny-default for a nil human: never panic, never auto-grant authority to a
		// missing approver (mirrors the nil-human discipline elsewhere in the package).
		human = func(string) bool { return false }
	}
	return func(action string) bool {
		if os.Getenv(EnvSelfImproveAutoApprove) == "" {
			return human(action) // default-off: the human decides
		}
		if sink != nil {
			sink.Emit("auto_approve_selfimprove", map[string]any{"action": action})
		}
		return true
	}
}
