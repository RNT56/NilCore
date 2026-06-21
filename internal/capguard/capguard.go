// Package capguard enforces the "Rule of Two" (a.k.a. the lethal-trifecta
// containment) for an agent session, in Go code rather than the prompt. It is the
// structural form of NilCore's standing invariants: until prompt injection can be
// reliably detected, a single session must never simultaneously hold all THREE of
//
//	A — ingests UNTRUSTED input (reads arbitrary web/page/screen content),
//	B — accesses PRIVATE data (mounted secrets / repo files / prior context),
//	C — has OPEN external communication (egress beyond the task's tight allowlist).
//
// Any two are tolerable; all three is an exfiltration path even when each looks
// benign (Willison's "lethal trifecta", Meta's "Agents Rule of Two"). NilCore
// already denies C-to-arbitrary-hosts (default-deny egress) and B-as-plaintext
// (secrets stay in the SecretStore, never in a prompt) — this leaf makes the
// composite rule explicit and decisive at session setup: it refuses to GRANT all
// three at once, and tells the caller when a human gate is the only way forward.
//
// It is a stdlib-only leaf (zero nilcore imports): it returns a Decision the
// wiring layer maps onto policy.Gate, so the policy package need not know about
// browser sessions and this package need not import the orchestrator.
package capguard

import (
	"sort"
	"strings"
)

// Capabilities describes what a session would be granted. The three booleans are
// the trifecta axes; the extra fields explain *why* each axis is set, for the
// audit log and the human gate prompt.
type Capabilities struct {
	UntrustedInput bool // A: the session reads arbitrary/untrusted content (a browse agent always does)
	PrivateData    bool // B: the session can read secrets / repo files / private context
	OpenEgress     bool // C: egress is wider than a tight, task-scoped allowlist

	// EgressHosts is the resolved allowlist; an empty or default-deny list is NOT
	// "open". A wildcard entry (e.g. "*", "*.com") OR a host count above OpenEgressThreshold
	// is treated as open communication for the purpose of axis C.
	EgressHosts []string
	Reasons     map[string]string // axis ("A"/"B"/"C") → human-readable reason (optional)
}

// OpenEgressThreshold is the host count above which an allowlist is considered
// "open" external communication for axis C. A handful of task-scoped hosts is
// tolerable; a broad list is an exfiltration channel. Wildcards count as open
// regardless of length.
const OpenEgressThreshold = 12

// Verdict is the outcome of a Rule-of-Two evaluation.
type Verdict string

const (
	// Allow — at most two axes are active; the session may proceed unattended.
	Allow Verdict = "allow"
	// GateRequired — all three axes are active but the combination is not
	// intrinsically forbidden; it may proceed ONLY through the human gate
	// (policy.Gate). The caller must not auto-approve.
	GateRequired Verdict = "gate-required"
	// Refuse — all three axes are active AND no gate is available (e.g. headless,
	// nil approver). Fail closed: do not run.
	Refuse Verdict = "refuse"
)

// Decision is the structured result the wiring layer acts on.
type Decision struct {
	Verdict Verdict
	Axes    []string // the active axes, sorted ("A","B","C")
	Detail  string   // a one-line explanation suitable for the audit log / gate prompt
}

// Trifecta reports whether all three axes are active.
func (c Capabilities) Trifecta() bool {
	return c.UntrustedInput && c.PrivateData && c.openEgress()
}

// openEgress decides axis C from the resolved allowlist: a wildcard, or more than
// OpenEgressThreshold hosts, or the explicit OpenEgress flag.
func (c Capabilities) openEgress() bool {
	if c.OpenEgress {
		return true
	}
	if len(c.EgressHosts) > OpenEgressThreshold {
		return true
	}
	for _, h := range c.EgressHosts {
		if strings.Contains(h, "*") {
			return true
		}
	}
	return false
}

// activeAxes returns the active axes in sorted order for stable logging.
func (c Capabilities) activeAxes() []string {
	var axes []string
	if c.UntrustedInput {
		axes = append(axes, "A")
	}
	if c.PrivateData {
		axes = append(axes, "B")
	}
	if c.openEgress() {
		axes = append(axes, "C")
	}
	sort.Strings(axes)
	return axes
}

// Evaluate applies the Rule of Two. gateAvailable reports whether a human gate
// (policy.Gate with a real approver) is wired; when the trifecta is present and no
// gate exists, the verdict is Refuse (fail closed) rather than GateRequired.
//
// The function is pure and total: it never blocks and never mutates anything. The
// caller maps GateRequired onto policy.Gate and Refuse onto an aborted session.
func Evaluate(c Capabilities, gateAvailable bool) Decision {
	axes := c.activeAxes()
	d := Decision{Axes: axes}

	if !c.Trifecta() {
		d.Verdict = Allow
		d.Detail = "rule-of-two satisfied: at most two of {untrusted-input, private-data, open-egress} active (" + describe(c) + ")"
		return d
	}

	if gateAvailable {
		d.Verdict = GateRequired
		d.Detail = "lethal trifecta assembled (" + describe(c) + "): untrusted input + private data + open egress — requires the human gate"
		return d
	}
	d.Verdict = Refuse
	d.Detail = "lethal trifecta assembled (" + describe(c) + ") with no human gate available — refusing (fail closed)"
	return d
}

// describe renders the active axes with their reasons for the gate prompt / log.
func describe(c Capabilities) string {
	var parts []string
	add := func(axis, def string) {
		if c.Reasons != nil {
			if r, ok := c.Reasons[axis]; ok && r != "" {
				parts = append(parts, axis+"="+r)
				return
			}
		}
		parts = append(parts, axis+"="+def)
	}
	if c.UntrustedInput {
		add("A", "untrusted-input")
	}
	if c.PrivateData {
		add("B", "private-data")
	}
	if c.openEgress() {
		if len(c.EgressHosts) > 0 {
			add("C", "open-egress["+strings.Join(c.EgressHosts, ",")+"]")
		} else {
			add("C", "open-egress")
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}
