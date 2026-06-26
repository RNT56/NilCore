// Package capability computes ONE legible descriptor of what a single drive may
// do — its tool set, the shell switch, the command-policy guard, the egress
// allowlist, and the Rule-of-Two trifecta axes — in one pure function (Phase 16,
// docs/ROADMAP-CLOSED-LOOP.md Pillar 1). It replaces the scattered, ad-hoc
// capability wiring (cmd/nilcore's capabilityForMode, the egress-profile
// resolution, and the hand-built capguard.Capabilities at the browse/desktop
// call sites) with a single struct that is legible (one value you can print),
// auditable (one metadata-only `capability` event), and runtime-negotiable (one
// place to widen/narrow).
//
// The package is a pure leaf: it imports only policy / capguard / egressprofile
// (all leaves) and stdlib — never the orchestrator, the model, the tool
// registry, or the session (deps_test.go enforces this). The descriptor's DEFAULT
// computation reproduces today's exact choices, so wiring it in is byte-identical
// (the cmd-level golden in EXP-T06 is the proof against the live function). The
// ToolSet is a SWITCH (ReadOnly bool), not the *tools.Registry, precisely so this
// package needn't import internal/tools; the call site maps it to the read-only
// or full registry.
package capability

import (
	"nilcore/internal/capguard"
	"nilcore/internal/policy"
)

// ToolSet is the registry-selection switch. ReadOnly true ⇒ the write-free loop
// registry; false ⇒ the full registry. The registry itself is held by the call
// site, keeping this package a leaf.
type ToolSet struct {
	ReadOnly bool
}

// Request is the harness-supplied input to For. Every field is available at the
// chat / browse / desktop call sites today.
type Request struct {
	Mode           string // session.Mode.String(): auto | discuss | plan | execute
	ReadRepo       bool   // repo / private context is readable (capguard axis B)
	UntrustedInput bool   // the drive ingests untrusted content (axis A; always true for browse/desktop)
	ProfileName    string // named egress preset ("" = deny-all)
	EgressFile     string // project-local .nilcore/egress.json ("" = none)
	GateAvailable  bool   // a human approver is wired
}

// Descriptor is the single legible record of a drive's permissions.
type Descriptor struct {
	Mode          string
	Tools         ToolSet
	ShellEnabled  bool
	CommandPolicy policy.CommandPolicy
	Egress        policy.Egress
	EgressSources []string
	Rule          capguard.Capabilities
	Approver      bool // whether a human gate is wired (feeds Evaluate + gate selection)
}

// Evaluate applies the Rule of Two from this descriptor's axes and gate
// availability — the one place the trifecta verdict is derived, instead of each
// call site re-invoking capguard.Evaluate.
func (d Descriptor) Evaluate() capguard.Decision {
	return capguard.Evaluate(d.Rule, d.Approver)
}

// Event projects the descriptor to a metadata-only map for ONE `capability`
// audit event (I5). It carries the mode, the read-only/shell switches, the egress
// SOURCE LABELS (never the host allowlist itself), and the trifecta verdict +
// active axes — never a secret, never the host list, never a model field
// (I3/I7). It deliberately does NOT include the capguard Detail string, which
// embeds the host list.
func (d Descriptor) Event() map[string]any {
	dec := d.Evaluate()
	return map[string]any{
		"mode":           d.Mode,
		"read_only":      d.Tools.ReadOnly,
		"shell":          d.ShellEnabled,
		"egress_sources": d.EgressSources,
		"verdict":        string(dec.Verdict),
		"axes":           dec.Axes,
	}
}
