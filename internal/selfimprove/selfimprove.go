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
		if ok, reason := s.CheckPath(path); !ok {
			return false, reason
		}
	}
	return true, ""
}

// CheckPath reports whether a single repo-relative path is in scope (allow-listed
// and not deny-listed). It is the per-path law Check folds over, exported so the
// EXECUTION-time guard (allowedRun) can screen every file the run actually touched
// against the SAME allow/deny rule the proposal was pre-screened with — closing the
// gap between "the proposal declared only these paths" and "the run only wrote
// these paths".
func (s Scope) CheckPath(path string) (ok bool, reason string) {
	for _, d := range s.Deny {
		if strings.HasPrefix(path, d) {
			return false, "denied: " + path + " is a protected core/contract file"
		}
	}
	for _, a := range s.Allow {
		if strings.HasPrefix(path, a) {
			return true, ""
		}
	}
	return false, "out of scope: " + path + " is not in the self-edit allow-list"
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
	// Run executes the proposal's goal as a verified task (worktree + verify) and
	// reports the branch holding the verified work. An empty branch means the run kept
	// nothing to land, and Propose refuses to claim a merge over it.
	Run func(ctx context.Context, goal string) (verified bool, branch string, err error)
	// Merge lands the verified branch into the repo. It runs ONLY after the human gate
	// approves, and its error is the sole authority on whether the edit shipped.
	//
	// This seam exists because Propose used to append `self_edit_merged` and return
	// merged=true while nothing merged anything: the orchestrator's KeepBranch only
	// PRESERVED the branch. Verified self-edit branches accumulated unmerged and
	// unsurfaced, the flywheel's Summary.Merged counted ships that never happened, and
	// the double opt-in NILCORE_SELFIMPROVE_AUTOAPPROVE auto-approved a merge that did
	// not exist. A nil Merge now means the flow CANNOT land an edit, and Propose says so
	// rather than reporting a phantom success.
	Merge func(ctx context.Context, branch string) error
	// Changed, when set, reports the repo-relative paths the run actually modified (e.g.
	// a `git diff --name-only` over the run's worktree). Propose screens EVERY changed
	// path against the scope AND the proposal's declared Paths and REFUSES to gate a run
	// that touched anything outside them (fail-closed) — so the verifier of record and
	// every other denied/undeclared file stay structurally unmodifiable at EXECUTION,
	// closing the gap where the free-text Goal could otherwise steer the model to edit a
	// file the proposal never declared. This is the enforcement the scope check alone
	// cannot provide: Check validates what the proposal DECLARED; Changed validates what
	// the run actually WROTE. Nil ⇒ the screen is skipped (byte-identical to before), so
	// an unwired flow behaves exactly as today; a production wiring supplies it from the
	// run's worktree diff (see cmd/nilcore/selfimprove.go).
	Changed func(ctx context.Context) (paths []string, err error)
	Gate    func(action string) bool // human gate before merge
	Log     *eventlog.Log
}

// Propose runs the pipeline: scope-check → run as a verified task → execution-time
// path screen → human gate → merge. The returned bool means the edit ACTUALLY LANDED:
// a nil Merge seam, an absent branch, or a failed merge all report false with an error.
func (f *Flow) Propose(ctx context.Context, p Proposal) (merged bool, err error) {
	if ok, reason := f.Scope.Check(p); !ok {
		f.Log.Append(eventlog.Event{Kind: "self_edit_rejected", Detail: map[string]any{"reason": reason}})
		return false, fmt.Errorf("self-edit rejected: %s", reason)
	}
	f.Log.Append(eventlog.Event{Kind: "self_edit_accepted", Detail: map[string]any{"reason": p.Reason, "goal": p.Goal}})

	verified, branch, err := f.Run(ctx, p.Goal)
	if err != nil {
		return false, fmt.Errorf("self-edit run: %w", err)
	}
	if !verified {
		f.Log.Append(eventlog.Event{Kind: "self_edit_unverified"})
		return false, nil // the checks must pass — green is non-negotiable
	}

	// EXECUTION-time path enforcement (fail-closed): if the executor can report what the
	// run actually modified, refuse to gate a run that touched ANY path outside the
	// declared, in-scope allow-list. This keeps the verifier of record (internal/verify/,
	// denied by DefaultScope) and every other undeclared/denied file structurally
	// unmodifiable at execution, closing the gap where the free-text Goal could otherwise
	// steer the model to edit a file the proposal never declared.
	if f.Changed != nil {
		changed, cerr := f.Changed(ctx)
		if cerr != nil {
			// Cannot prove the run stayed in scope ⇒ do NOT gate it (fail-closed).
			f.Log.Append(eventlog.Event{Kind: "self_edit_scope_indeterminate", Detail: map[string]any{"error": cerr.Error()}})
			return false, fmt.Errorf("self-edit scope check: %w", cerr)
		}
		if bad, why := outOfScope(f.Scope, p.Paths, changed); bad != "" {
			f.Log.Append(eventlog.Event{Kind: "self_edit_scope_violation", Detail: map[string]any{"path": bad, "reason": why}})
			return false, fmt.Errorf("self-edit rejected: %s", why)
		}
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

	// The gate approved. Now actually land it — and report merged=true only if we did.
	if f.Merge == nil {
		f.Log.Append(eventlog.Event{Kind: "self_edit_merge_unwired"})
		return false, fmt.Errorf("self-edit approved but no merge is wired: the edit was NOT landed")
	}
	if branch == "" {
		f.Log.Append(eventlog.Event{Kind: "self_edit_no_branch"})
		return false, fmt.Errorf("self-edit approved but the run kept no verified branch to merge")
	}
	if merr := f.Merge(ctx, branch); merr != nil {
		f.Log.Append(eventlog.Event{Kind: "self_edit_merge_failed", Detail: map[string]any{"error": merr.Error()}})
		return false, fmt.Errorf("self-edit merge: %w", merr)
	}
	f.Log.Append(eventlog.Event{Kind: "self_edit_merged", Detail: map[string]any{"goal": p.Goal, "branch": branch}})
	return true, nil
}

// outOfScope returns the first changed path that is NOT permitted, with a reason,
// or ("","") when every changed path is in scope. A path is permitted only when it
// (a) passes the scope allow/deny law AND (b) was declared in the proposal's Paths —
// so the run may write ONLY the files the proposal committed to up front. declared
// is matched exactly (the proposal names concrete files); scope is the prefix law.
func outOfScope(s Scope, declared, changed []string) (path, reason string) {
	decl := make(map[string]bool, len(declared))
	for _, d := range declared {
		decl[d] = true
	}
	for _, c := range changed {
		if c == "" {
			continue
		}
		if ok, why := s.CheckPath(c); !ok {
			return c, why
		}
		if !decl[c] {
			return c, "out of scope: " + c + " was modified but not declared in the proposal's paths"
		}
	}
	return "", ""
}
