package graapprove

import (
	"nilcore/internal/blastbudget"
	"nilcore/internal/policy"
)

// MaybeWrap is the single construction seam and the first default-off layer. It
// returns the human approver UNCHANGED (the identical value) when env is nil or
// empty — the GradedApprover is never allocated, so an unwired seam is byte-
// identical to today (proven by a test that compares the returned interface value).
//
// When an envelope IS present it constructs a GradedApprover wrapping the human. A
// nil, unparseable, or empty envelope NEVER auto-approves: the empty case returns
// the human here, and an on-but-unproven envelope (no earned trust / broken chain)
// is denied inside ApproveStructured. Callers are expected to have run
// env.Validate() already; an invalid envelope should be rejected at config load,
// not silently widened here.
func MaybeWrap(human policy.Approver, env *Envelope, logPath string, blast *blastbudget.Budget, opts ...Option) policy.Approver {
	if env.Empty() {
		return human // layer 1: no construction, byte-identical default-off
	}
	return newGraded(human, *env, logPath, blast, opts...)
}
