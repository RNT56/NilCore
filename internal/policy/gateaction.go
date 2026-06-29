package policy

// Structured gate actions (P2-T03).
//
// WHY: the legacy free-text Classify substring-matches bare words like "merge",
// "reset --hard" and "transfer". That is correct for host-level commands the
// model proposes, but it is dangerous for the multi-agent integrator: throwaway
// worktree merges and `git reset --hard <sha>` rollbacks are *reversible by
// construction* (they only ever touch a disposable integration worktree, never
// the real branch), yet their descriptions contain exactly those signal words.
// Routing such a description through the free-text Gate would spuriously classify
// it Irreversible and either gate it or — since integrator subagents hold a nil
// Approver — deadlock the auto-integration.
//
// The fix is to gate the integration boundary by a *structured* action whose
// reversibility is decided by its Type, not by scanning free text. Only the final
// promote of a converged, verified tree onto a real branch is Irreversible; every
// reversible throwaway step has no GateAction and therefore never reaches the gate
// at all. A description string can still be carried for the human prompt and the
// audit log, but it is pure data — it never participates in classification.

// GateActionType enumerates the irreversible boundary operations the supervisor
// and project loop may attempt. The set is intentionally small and closed: a new
// boundary action must be added here deliberately rather than inferred from text.
type GateActionType int

const (
	// PromoteToBase lands a converged, verified integration tree onto the real
	// base branch. This is the single irreversible step of a supervised run.
	PromoteToBase GateActionType = iota
	// Push publishes commits to a remote.
	Push
	// Deploy ships to a running environment.
	Deploy
	// OpenPR opens (or updates) a pull request on a remote forge from a verified
	// working branch. It publishes the agent's work for human review and never
	// merges, but it is outward-facing and irreversible (it creates state on a
	// remote), so it is always gated. Added last so the existing iota values are
	// unchanged.
	OpenPR
	// BindSelfAuthored binds a MODEL-AUTHORED acceptance verifier so it becomes a
	// real, sandboxed check that gates the run (internal/verify/selfacc, the
	// closed-loop "the agent raises its own bar"). It is a trust escalation — the
	// agent is proposing how its own work is judged — so it is gated like any other
	// boundary action: an attended operator approves each, a headless run denies by
	// default, and graduated auto-approval (graapprove) admits only a self-check
	// that has EARNED trust within the operator envelope. The bound check can only
	// ADD a criterion that must also pass (it never lowers the bar — I2); the gate
	// governs only WHETHER the agent's proposed check is trusted to participate.
	// Added last so the existing iota values are unchanged.
	BindSelfAuthored
)

func (t GateActionType) String() string {
	switch t {
	case PromoteToBase:
		return "promote-to-base"
	case Push:
		return "push"
	case Deploy:
		return "deploy"
	case OpenPR:
		return "open-pr"
	case BindSelfAuthored:
		return "bind-self-authored"
	default:
		return "unknown"
	}
}

// GateAction is a structured, irreversible boundary operation. Its reversibility
// is derived from Type alone; Branch and Detail are carried for the human prompt
// and the audit trail only and never affect classification.
type GateAction struct {
	Type   GateActionType
	Branch string // target branch for PromoteToBase / Push (informational)
	Detail string // optional human-readable context for the approver / log
}

// Class reports the reversibility of a structured action. Every action in the
// closed GateActionType set is a boundary operation and therefore Irreversible;
// reversible steps are represented by the *absence* of a GateAction, so they are
// never constructed here. The method exists so callers can introspect/audit a
// classification without invoking the approver.
func (a GateAction) Class() Class { return Irreversible }

// describe renders a stable, human-readable line for the approver prompt and the
// event log. It is data, not an instruction, and is never fed back to Classify.
func (a GateAction) describe() string {
	d := a.Type.String()
	if a.Branch != "" {
		d += " " + a.Branch
	}
	if a.Detail != "" {
		d += " (" + a.Detail + ")"
	}
	return d
}

// GateStructured reports whether a structured boundary action may proceed now.
//
// Unlike the free-text Gate, classification here is by Type, so a reversible
// throwaway merge/reset described elsewhere as data can never be auto-gated:
// it simply has no GateAction and never calls this function. Every GateAction is
// Irreversible, so the approver is always consulted; a nil approver default-denies
// (no ambient authority for an irreversible step).
func GateStructured(a GateAction, ask Approver) bool {
	if a.Class() == Reversible {
		return true // unreachable today; kept so the policy mirrors free-text Gate
	}
	if ask == nil {
		return false
	}
	// An approver may OPT IN to receiving the structured action (so a graduated
	// auto-approval policy can decide by Type+scope rather than a flattened
	// string) by implementing StructuredApprover. This branch is additive and
	// sits ABOVE the existing free-text return: an approver that does NOT
	// implement it (e.g. ConsoleApprover) falls through to exactly the prior
	// behaviour, so the default path is byte-identical (proven by a golden test).
	if sa, ok := ask.(StructuredApprover); ok {
		return sa.ApproveStructured(a)
	}
	return ask.Approve(a.describe())
}

// StructuredApprover is the OPTIONAL extension an approver implements to receive
// the full GateAction (its Type, Branch, and Detail) instead of the flattened
// description string. It exists so the Phase-16 graduated-auto-approval policy
// (internal/graapprove, docs/ROADMAP-CLOSED-LOOP.md Pillar 5) can decide by the
// structured action — the frozen Approver interface and the closed GateActionType
// set are unchanged. An approver that does not implement it keeps today's
// free-text Approve behaviour exactly.
type StructuredApprover interface {
	ApproveStructured(a GateAction) bool
}
