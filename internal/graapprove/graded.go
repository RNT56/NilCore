package graapprove

import (
	"context"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/policy"
)

// GradedApprover wraps a human policy.Approver and auto-approves a structured
// GateAction only when it clears every gate (kill-switch, eligibility, blast
// radius, earned trust, rate, dollars). It implements BOTH policy.StructuredApprover
// (graded, via ApproveStructured) and policy.Approver (Approve delegates straight
// to the human — free-text is NEVER auto-approved). Every state it reads — the
// envelope, the trust log, the blast budget — is operator-authored host-side data
// that never reaches the model (I3).
type GradedApprover struct {
	human policy.Approver // the fall-through; consulted on any non-pass
	env   Envelope        // operator policy (validated by the caller)
	// logPath is the append-only event log; the trust view and the per-day rate
	// window are rebuilt from it per decision (read-only, fail-closed).
	logPath string
	// blast is the SHARED $/rate/irreversible meter; nil ⇒ no dollar ceiling is
	// enforced through it (the clause MaxDollarsDay still gates structurally).
	blast *blastbudget.Budget
	// sink receives auto_approve / auto_deny audit events; nil ⇒ silent.
	sink Sink
	// now is injected for deterministic recency/rate; nil ⇒ time.Now.
	now func() time.Time
	// root resolves the kill-switch sentinel (normally the worktree).
	root string
	// sentinel overrides the default kill-switch path (tests).
	sentinel string
}

// Option configures a GradedApprover.
type Option func(*GradedApprover)

// WithSink installs the audit sink.
func WithSink(s Sink) Option { return func(g *GradedApprover) { g.sink = s } }

// WithClock injects the clock used for recency and the rate window (tests).
func WithClock(now func() time.Time) Option { return func(g *GradedApprover) { g.now = now } }

// WithRoot sets the directory the kill-switch sentinel is resolved against.
func WithRoot(root string) Option { return func(g *GradedApprover) { g.root = root } }

// WithSentinel overrides the kill-switch sentinel path (tests).
func WithSentinel(p string) Option { return func(g *GradedApprover) { g.sentinel = p } }

// newGraded constructs a GradedApprover. Callers go through MaybeWrap so the
// default-off (return-human-unchanged) discipline is enforced in one place.
func newGraded(human policy.Approver, env Envelope, logPath string, blast *blastbudget.Budget, opts ...Option) *GradedApprover {
	g := &GradedApprover{human: human, env: env, logPath: logPath, blast: blast, now: time.Now}
	for _, o := range opts {
		o(g)
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g
}

// Approve satisfies policy.Approver. Free-text actions are NEVER auto-approved —
// graduated trust is defined over structured action+scope, so a flattened string
// carries no scope to gate on. It always delegates to the human.
func (g *GradedApprover) Approve(action string) bool {
	if g == nil || g.human == nil {
		return false // no ambient authority
	}
	return g.human.Approve(action)
}

// clauseFor returns the ClassClause matching the action type (ok=false if none).
func (g *GradedApprover) clauseFor(t string) (ClassClause, bool) {
	for _, c := range g.env.Classes {
		if c.Type == t {
			return c, true
		}
	}
	return ClassClause{}, false
}

// describe renders a stable, human-readable line for the human approver prompt.
// It mirrors policy.GateAction.describe (unexported) so the fall-through human sees
// the same string today's path would show. It is pure DATA (I7), never policy.
func describe(a policy.GateAction) string {
	d := a.Type.String()
	if a.Branch != "" {
		d += " " + a.Branch
	}
	if a.Detail != "" {
		d += " (" + a.Detail + ")"
	}
	return d
}

// scopeFor derives the scope string a clause is matched against: for Deploy the
// environment (Detail-carried, falling back to Branch), otherwise the target
// Branch. The value is pure DATA (I7) — matched by glob/equality, never executed.
func scopeFor(a policy.GateAction) string {
	if a.Type == policy.Deploy {
		// Deploy targets are carried in Branch by the gate site (the structured
		// action has no Environment field); treat it as the environment name.
		return a.Branch
	}
	return a.Branch
}

// ApproveStructured is the graded decision. It runs the gates in order and, on the
// first failure, emits auto_deny{reason} and delegates to the human. On a full pass
// it emits auto_approve with the full evidence object and returns true. The order
// is load-bearing: the kill-switch is consulted FIRST so revocation is instant.
func (g *GradedApprover) ApproveStructured(a policy.GateAction) bool {
	if g == nil || g.human == nil {
		return false
	}
	typ := a.Type.String()
	scope := scopeFor(a)

	// (1) Kill-switch first — instant, no restart.
	if killSwitchEngaged(g.root, g.sentinel) {
		g.emitDeny("killswitch", typ, scope, nil)
		return g.human.Approve(describe(a))
	}

	// (2) Eligibility — no clause for this Type ⇒ human.
	clause, ok := g.clauseFor(typ)
	if !ok {
		g.emitDeny("not_eligible", typ, scope, nil)
		return g.human.Approve(describe(a))
	}

	// (3) Blast radius — DenyBranches and prod* ALWAYS win; AllowBranches must admit
	// the scope; for Deploy the Environments allowlist must admit it too.
	if isProd(scope) || matchAny(scope, clause.DenyBranches) {
		g.emitDeny("out_of_scope", typ, scope, map[string]any{"protected": true})
		return g.human.Approve(describe(a))
	}
	if !matchAny(scope, clause.AllowBranches) {
		g.emitDeny("out_of_scope", typ, scope, nil)
		return g.human.Approve(describe(a))
	}
	if a.Type == policy.Deploy {
		if len(clause.Environments) == 0 || !matchAny(scope, clause.Environments) {
			g.emitDeny("out_of_scope", typ, scope, map[string]any{"environment": scope})
			return g.human.Approve(describe(a))
		}
	}

	// (4) Trust bar — rebuilt from the log per decision; a chain error denies
	// EXPLICITLY (a tampered log earns nothing).
	view, err := BuildTrust(g.logPath)
	if err != nil || !view.ChainOK {
		g.emitDeny("chain_broken", typ, scope, map[string]any{"chain_ok": view.ChainOK})
		return g.human.Approve(describe(a))
	}
	t := view.Tally(ScopeKey{Type: typ, Scope: scope})
	now := g.now().UTC()
	recentOK := !t.LastGreen.IsZero() &&
		now.Sub(t.LastGreen.UTC()) <= time.Duration(clause.RecencyDays)*24*time.Hour
	trustOK := t.Green >= clause.MinSuccesses && t.Total >= clause.MinSample && recentOK
	if !trustOK {
		g.emitDeny("below_bar", typ, scope, map[string]any{
			"green": t.Green, "total": t.Total,
			"min_successes": clause.MinSuccesses, "min_sample": clause.MinSample,
			"recency_days": clause.RecencyDays, "recent_ok": recentOK,
		})
		return g.human.Approve(describe(a))
	}

	// (5) Rate — per-UTC-day auto_approve count for (type,scope) must be < MaxPerDay.
	// Rebuilt from the durable log so a restart never resets the window.
	today := dayKey(now)
	rate, rerr := countAutoApprovalsToday(g.logPath, typ, scope, today)
	if rerr != nil || rate >= clause.MaxPerDay {
		g.emitDeny("rate_exceeded", typ, scope, map[string]any{
			"rate": rate, "max_per_day": clause.MaxPerDay,
		})
		return g.human.Approve(describe(a))
	}

	// (5b) Blast irreversible fence — every auto-approval consumes one slot of the
	// SHARED blast-radius meter's irreversible axis. This is the composition law in
	// code (min(P5, blast)): a P5 grant proceeds ONLY within the remaining blast
	// envelope, and a breach is FINAL — fall through to the human. A nil g.blast (no
	// -blast-radius preset) is a no-op, so an unfenced run is byte-identical (the
	// irreversible axis bites only when an operator sets a ceiling).
	ctx := context.Background()
	if cerr := g.blast.ChargeIrreversible(ctx, 1); cerr != nil {
		g.emitDeny("blast_radius", typ, scope, map[string]any{"axis": "irreversible"})
		return g.human.Approve(describe(a))
	}

	// (5c) Dollars — when a clause carries a $/day ceiling, charge it through the SAME
	// shared meter (never a second counter). On a breach, roll back the irreversible
	// slot we just took (CreditIrreversible) so a denied action consumes nothing, then
	// deny and delegate. A zero ceiling charges nothing.
	dollars := clause.MaxDollarsDay
	if dollars > 0 && g.blast != nil {
		if cerr := g.blast.ChargeAutoApprovalDollars(ctx, today, dollars); cerr != nil {
			g.blast.CreditIrreversible(1) // this action did not proceed — release its slot
			g.emitDeny("over_ceiling", typ, scope, map[string]any{
				"max_dollars_day": dollars,
			})
			return g.human.Approve(describe(a))
		}
	}

	// Full pass ⇒ emit auto_approve with the full evidence object and return true.
	g.emit("auto_approve", map[string]any{
		"action": typ, "scope": scope,
		"green": t.Green, "total": t.Total,
		"last_green": t.LastGreen.UTC().Format(time.RFC3339),
		"bar":        map[string]any{"min_successes": clause.MinSuccesses, "min_sample": clause.MinSample, "recency_days": clause.RecencyDays},
		"rate":       map[string]any{"count": rate, "max_per_day": clause.MaxPerDay},
		"dollars":    map[string]any{"charged": dollars, "max_dollars_day": clause.MaxDollarsDay},
		"chain_ok":   view.ChainOK,
	})
	return true
}

// emitDeny records an auto_deny with its reason plus the action/scope, then the
// extra evidence. The detail is metadata only — never the human prompt string.
func (g *GradedApprover) emitDeny(reason, typ, scope string, extra map[string]any) {
	d := map[string]any{"reason": reason, "action": typ, "scope": scope}
	for k, v := range extra {
		d[k] = v
	}
	g.emit("auto_deny", d)
}

func (g *GradedApprover) emit(kind string, detail map[string]any) {
	if g.sink == nil {
		return
	}
	g.sink.Emit(kind, detail)
}
