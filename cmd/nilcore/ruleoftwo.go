package main

import (
	"fmt"
	"os"
	"strings"

	"nilcore/internal/capguard"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

// envDisabled reports whether env var name is explicitly turned OFF (0/false/no/off,
// any case, whitespace-trimmed). Unset or any other value ⇒ NOT disabled. It is the
// negative twin of envOptIn, for DEFAULT-ON gates with a documented escape hatch
// (mirrors the NILCORE_KERNEL idiom: on unless explicitly opted out).
func envDisabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

// ruleOfTwoEnforced reports whether the Rule-of-Two trifecta gate is enforced on the
// main agent paths. DEFAULT-ON; NILCORE_RULE_OF_TWO=0 (or false/no/off) is the
// documented operator opt-out — an acknowledged relaxation of the §2 Rule-of-Two, to
// be used only where the operator has independently accepted the risk.
func ruleOfTwoEnforced() bool { return !envDisabled("NILCORE_RULE_OF_TWO") }

// enforceRuleOfTwo evaluates the capguard LETHAL TRIFECTA (untrusted web input ∧
// private repo data ∧ open egress) for a MAIN agent path (do/run/chat/serve/swarm) —
// the tiers that, unlike the browse/desktop tiers, historically never evaluated it even
// though they compute the same axes. It ALWAYS records a metadata-only `capguard` audit
// event (the verdict + the active axes + the axis LABELS — never the resolved host
// list, which stays out of the append-only log per I3/I7, exactly as capability.Event
// does).
//
// When enforce is true and all three axes hold:
//   - a real human approver (attended chat/tui/run): prompt once; a denial aborts.
//   - a headless approver (serve/swarm/batch): the trifecta with NO human present is
//     precisely the lethal combination the Rule of Two exists to refuse — the headless
//     deny-default approver fails closed, UNLESS a configured graduated-auto-approval
//     envelope + earned trust auto-approves inside its blast fence.
//
// The shipped default egress is deny-all / narrow, so axis C is false and the verdict is
// Allow — byte-identical to before for every normal run. Only a genuinely wide egress (a
// wildcard, or more than capguard.OpenEgressThreshold hosts) combined with web tools and
// a mounted repo trips the gate, which is exactly the configuration §2's Rule of Two
// targets. With enforce=false (the opt-out) the verdict is still logged but never blocks.
//
// approver MUST be a concrete non-nil policy.Approver when a gate is intended (a typed-nil
// interface would satisfy != nil and then panic on Approve); pass a nil interface ONLY to
// model "no gate at all" (⇒ Refuse when the trifecta holds). Callers pass a
// NewConsoleApprover (attended) or a wrapAutoApprove(deny-default) (headless).
func enforceRuleOfTwo(log *eventlog.Log, enforce, untrusted, repoMounted bool, egress policy.Egress, approver policy.Approver, prompt string) error {
	caps := capguard.Capabilities{
		UntrustedInput: untrusted,
		PrivateData:    repoMounted,
		EgressHosts:    egress.Allowed,
		Reasons: map[string]string{
			"A": ternary(untrusted, "web-tools", ""),
			"B": ternary(repoMounted, "repo-mounted", ""),
			"C": ternary(len(egress.Allowed) > 0, "runtime-egress", ""),
		},
	}
	dec := capguard.Evaluate(caps, approver != nil)
	if log != nil {
		// Metadata-only (I3/I7): verdict + active axes + axis LABELS. The resolved host
		// list lives only in dec.Detail, which is DELIBERATELY not logged here (parity
		// with capability.Descriptor.Event) — and, for the same reason, is kept out of the
		// error strings below too.
		log.Append(eventlog.Event{Kind: "capguard", Detail: map[string]any{
			"verdict":  string(dec.Verdict),
			"axes":     dec.Axes,
			"reasons":  caps.Reasons,
			"enforced": enforce,
		}})
	}
	if !enforce || dec.Verdict == capguard.Allow {
		return nil
	}
	switch dec.Verdict {
	case capguard.GateRequired:
		if approver != nil && approver.Approve(prompt) {
			return nil
		}
		return fmt.Errorf("Rule of Two: the lethal trifecta (untrusted web input ∧ private repo data ∧ open egress) was denied at the gate; narrow the egress allowlist, or set NILCORE_RULE_OF_TWO=0 to opt out")
	default: // Refuse: the lethal trifecta with no gate available (headless, no envelope).
		return fmt.Errorf("Rule of Two: refusing the lethal trifecta (untrusted web input ∧ private repo data ∧ open egress) with no human gate; narrow the egress allowlist, run attended, configure a graduated-auto-approval envelope, or set NILCORE_RULE_OF_TWO=0 to opt out")
	}
}
