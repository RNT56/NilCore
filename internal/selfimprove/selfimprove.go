// Package selfimprove is the gated self-edit flow (P5-T02): the agent may
// proactively propose changes to its own prompts, skills, and tools — but a scope
// check enforces an allow-list (those areas) and a deny-list (the invariants,
// contract files, and core loop). A proposal touching anything denied is rejected
// outright. An in-scope edit runs as a normal task in a worktree, must pass the
// verifier, and requires the human gate before it can merge. It never bypasses
// the gate or any invariant, and every step is audited.
package selfimprove

import (
	"context"
	"fmt"
	"strings"

	"nilcore/internal/eventlog"
)

// Proposal is a self-proposed change to the agent's own prompts/skills/tools.
type Proposal struct {
	Reason string   // why: recurring failure, repeated manual step, missing tool
	Paths  []string // repo-relative files it would touch
	Goal   string   // the task to run if accepted
}

// Scope is the editable surface: Allow prefixes may be edited; Deny prefixes
// (invariants, contract files, the core loop) never may. Deny wins over Allow.
type Scope struct {
	Allow []string
	Deny  []string
}

// DefaultScope permits only prompts/skills/tools and protects the core + contracts.
func DefaultScope() Scope {
	return Scope{
		Allow: []string{"internal/skills/", "skills/", "internal/tools/", "docs/PERSONA.md"},
		Deny: []string{
			"internal/backend/backend.go", "internal/agent/", "internal/sandbox/",
			"internal/policy/", "internal/verify/", "internal/eventlog/", "internal/guard/",
			"CLAUDE.md", "docs/ARCHITECTURE.md", "docs/TASKS.md", "go.mod", "Makefile",
		},
	}
}

// Check reports whether every path a proposal touches is in scope. A path is in
// scope only if it matches an Allow prefix and no Deny prefix.
func (s Scope) Check(p Proposal) (ok bool, reason string) {
	for _, path := range p.Paths {
		for _, d := range s.Deny {
			if strings.HasPrefix(path, d) {
				return false, "denied: " + path + " is a protected core/contract file"
			}
		}
		allowed := false
		for _, a := range s.Allow {
			if strings.HasPrefix(path, a) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false, "out of scope: " + path + " is not in the self-edit allow-list"
		}
	}
	return true, ""
}

// Flow runs the gated self-edit pipeline.
//
// The measured-delta regression fence (Phase 16, Pillar 4) deliberately lives at
// the LOOP level, not here: internal/flywheel/loop scores the frozen suite
// before/after a candidate and gates on measure.Fence.Improved BEFORE it ever
// calls Propose. That keeps selfimprove a lower leaf (it never imports the
// flywheel) and means a verifier-green, in-scope candidate that reaches Propose
// has already cleared the fence. There is intentionally no second per-Flow fence
// hook — one fence, one guarantee.
type Flow struct {
	Scope Scope
	Run   func(ctx context.Context, goal string) (verified bool, err error) // run as a task (worktree + verify)
	Gate  func(action string) bool                                          // human gate before merge
	Log   *eventlog.Log
}

// Propose runs the pipeline: scope-check → run as a verified task → human gate →
// merge. Returns whether the edit merged.
func (f *Flow) Propose(ctx context.Context, p Proposal) (merged bool, err error) {
	if ok, reason := f.Scope.Check(p); !ok {
		f.Log.Append(eventlog.Event{Kind: "self_edit_rejected", Detail: map[string]any{"reason": reason}})
		return false, fmt.Errorf("self-edit rejected: %s", reason)
	}
	f.Log.Append(eventlog.Event{Kind: "self_edit_accepted", Detail: map[string]any{"reason": p.Reason, "goal": p.Goal}})

	verified, err := f.Run(ctx, p.Goal)
	if err != nil {
		return false, fmt.Errorf("self-edit run: %w", err)
	}
	if !verified {
		f.Log.Append(eventlog.Event{Kind: "self_edit_unverified"})
		return false, nil // the checks must pass — green is non-negotiable
	}

	// The measured-delta regression fence already ran at the loop level (the
	// flywheel scores the frozen suite before/after the candidate and drops a
	// non-improving one BEFORE proposing), so a candidate that reaches here is
	// verifier-green AND measured-improving. The only step left is the human gate.

	// Merge is irreversible: always the human gate, no exceptions.
	if f.Gate == nil || !f.Gate("merge self-edit: "+p.Goal) {
		f.Log.Append(eventlog.Event{Kind: "self_edit_gated"})
		return false, nil
	}
	f.Log.Append(eventlog.Event{Kind: "self_edit_merged", Detail: map[string]any{"goal": p.Goal}})
	return true, nil
}
