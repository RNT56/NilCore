package main

import (
	"fmt"
	"os"
	"strings"

	"nilcore/internal/blastbudget"
	"nilcore/internal/eventlog"
	"nilcore/internal/graapprove"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
)

// autoApprovePresetEnv names a graduated-auto-approval preset (conservative|standard|
// trusted) when no explicit envelope is configured — the documented opt-in shortcut
// (ARCHITECTURE.md §0).
const autoApprovePresetEnv = "NILCORE_AUTOAPPROVE_PRESET"

// applyAutoApprovePreset seeds cfg.AutoApprove from NILCORE_AUTOAPPROVE_PRESET when the
// config does not already define an envelope. This makes the documented env opt-in real
// and gives graapprove.Preset a production consumer. An EXPLICIT config envelope always
// wins; an unknown preset name is reported and ignored (fail-closed — auto-approval
// stays OFF rather than activating a wrong envelope).
func applyAutoApprovePreset(cfg onboard.Config) onboard.Config {
	name := strings.TrimSpace(os.Getenv(autoApprovePresetEnv))
	if name == "" || !cfg.AutoApprove.Empty() {
		return cfg
	}
	env, err := graapprove.Preset(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: %v (auto-approval stays off)\n", err)
		return cfg
	}
	cfg.AutoApprove = &env
	return cfg
}

// autoapprove.go is the cmd-side wiring for graduated auto-approval (Phase 16,
// GAA-T07). It is the ONLY place the graapprove policy is constructed, so every
// gate-site approver activates it the same way — and, crucially, the same way
// when it is OFF: with no operator envelope configured the human approver is
// returned UNCHANGED, so every existing flow is byte-identical (default-off).

// autoApproveSink adapts the run's append-only event log to graapprove.Sink, so
// every auto_approve / auto_deny / boundary_outcome decision lands in the hash-
// chained audit trail (I5). The detail carries only the metadata-only evidence
// object — no secret, no model field (I3/I7); redact() in eventlog.Append is the
// backstop.
type autoApproveSink struct{ log *eventlog.Log }

func (s autoApproveSink) Emit(kind string, detail map[string]any) {
	s.log.Append(eventlog.Event{Kind: kind, Detail: detail})
}

// wrapAutoApprove wraps a human approver with the graduated-auto-approval policy
// WHEN an operator envelope is configured (cfg.AutoApprove non-empty); otherwise
// it returns the human approver UNCHANGED — the byte-identical default-off path.
// logPath feeds the earned-trust replay (fail-closed over a broken chain); blast
// is the shared blast-radius meter minted from -blast-radius (nil when "off" — the
// default — so the run is unfenced). A nil human (e.g. the swarm approver, which
// never auto-approves) is returned as-is so we never construct a graded approver
// around a missing human.
func wrapAutoApprove(human policy.Approver, cfg onboard.Config, logPath string, log *eventlog.Log, blast *blastbudget.Budget) policy.Approver {
	if human == nil || cfg.AutoApprove.Empty() {
		return human
	}
	return graapprove.MaybeWrap(human, cfg.AutoApprove, logPath, blast, graapprove.WithSink(autoApproveSink{log}))
}

// emitBoundaryOutcome records that a verifier-judged boundary (promote-to-base /
// open-pr / deploy) was reached, so graapprove.TrustView can fold it into earned
// trust (GAA-T04). `passed` MUST be the verifier verdict on the work, never a
// backend self-report (I2). It is additive — a metadata-only event alongside the
// existing promote/open_pr events. A nil log is a no-op (Append is nil-safe).
func emitBoundaryOutcome(log *eventlog.Log, action, scope string, passed bool) {
	log.Append(eventlog.Event{Kind: "boundary_outcome", Detail: map[string]any{
		"action": action,
		"scope":  scope,
		"passed": passed,
		"chain":  true,
	}})
}
