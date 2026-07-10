package graapprove

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
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
	// logPath is the append-only event log; the trust view, the per-day rate window,
	// and the per-day auto-approved-$ total are all seeded/rebuilt from it per decision
	// (read-only, fail-closed).
	logPath string
	// blast is the SHARED $/rate/irreversible meter. When non-nil the per-day dollar
	// charge routes through it so the effective ceiling is min(clause.MaxDollarsDay,
	// blast day budget). nil (default -blast-radius off) is fully REACHABLE: the clause's
	// own MaxDollarsDay still bounds the per-day auto-approved total in-process (dollarsDay).
	blast *blastbudget.Budget
	// trust is the memoizing incremental trust builder over logPath: each decision
	// folds only the log's appended suffix (and skips scan+Verify when the log is
	// unchanged) instead of rescanning+re-Verifying the whole log every time.
	trust *TrustBuilder
	// sink receives auto_approve / auto_deny audit events; nil ⇒ silent.
	sink Sink
	// now is injected for deterministic recency/rate; nil ⇒ time.Now.
	now func() time.Time
	// root resolves the kill-switch sentinel (normally the worktree).
	root string

	// rateMu guards rateCount AND dollarsDay. It makes the per-day rate cap and the
	// per-day dollar ceiling ATOMIC across concurrent decisions (the autonomy daemon /
	// swarm can share one approver over one log): the log replay
	// (countAutoApprovalsToday / sumAutoApprovalDollarsToday) is only the
	// restart-recovery SEED, and these in-process counters are the authority — two
	// concurrent decisions for the same (type,scope,day) can never both slip past a
	// MaxPerDay cap or both over-run a MaxDollarsDay ceiling.
	rateMu sync.Mutex
	// rateCount maps a per-(type|scope|yyyy-mm-dd) key to the count of auto-approvals
	// granted (or seeded from the log) for that window. A key that is present has been
	// seeded from the durable log exactly once (lazily, on first access), so a restart
	// recovers the day's prior approvals; a key absent from a new day is seeded fresh
	// (the window rolls at midnight UTC because the day is part of the key).
	rateCount map[string]int

	// dollarsDay maps a per-(type|scope|yyyy-mm-dd) key to the cumulative auto-approval
	// DOLLARS granted for that window. It is the per-day $ authority that enforces
	// clause.MaxDollarsDay against ACTUAL auto-approved spend — NOT a per-action delta
	// derived from the run ledger. It is seeded once per window from the durable log
	// (sumAutoApprovalDollarsToday, which sums each auto_approve event's dollars.actual_usd
	// for that day), guarded by rateMu, and rolls at midnight UTC because the day is part
	// of the key. There is no chargedUSD watermark: Evidence.SpentUSD is the run ledger's
	// CUMULATIVE total (build.go sets it to ledger.Total() on EVERY gated action), so it
	// is NOT this action's incremental cost and must never be charged as such — the prior
	// fix's `SpentUSD - chargedUSD` premise (that actual per-action cost is ≈0 because no
	// site populates SpentUSD) was false and, under the default -blast-radius off, denied
	// every action once the run had spent any money. A single boundary action's own
	// incremental $ is not isolable from the cumulative ledger, so we meter the per-day
	// auto-approved total (which is what MaxDollarsDay means) and charge each action its
	// own declared cost (perActionUSD) — $0 today, since no signal carries it.
	dollarsDay map[string]float64
}

// Option configures a GradedApprover.
type Option func(*GradedApprover)

// WithSink installs the audit sink.
func WithSink(s Sink) Option { return func(g *GradedApprover) { g.sink = s } }

// WithClock injects the clock used for recency and the rate window (tests).
func WithClock(now func() time.Time) Option { return func(g *GradedApprover) { g.now = now } }

// WithRoot sets the directory the kill-switch sentinel is resolved against.
func WithRoot(root string) Option { return func(g *GradedApprover) { g.root = root } }

// newGraded constructs a GradedApprover. Callers go through MaybeWrap so the
// default-off (return-human-unchanged) discipline is enforced in one place.
func newGraded(human policy.Approver, env Envelope, logPath string, blast *blastbudget.Budget, opts ...Option) *GradedApprover {
	g := &GradedApprover{human: human, env: env, logPath: logPath, blast: blast, now: time.Now, rateCount: map[string]int{}, dollarsDay: map[string]float64{}, trust: &TrustBuilder{}}
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

// scopeFor derives the scope string a clause is matched against. Today every action
// scopes on Branch: for Deploy that Branch carries the target environment name (the
// structured action has no Environment field), for every other type it is the git
// branch. The value is pure DATA (I7) — matched by glob/equality, never executed. (If a
// type ever needs a different scope source, special-case it here.)
func scopeFor(a policy.GateAction) string {
	return a.Branch
}

// fallThrough delegates a non-auto-approved structured action to the human. When
// the wrapped human approver is itself structured (console/TUI/channel gates), the
// FULL GateAction is forwarded so the decision Evidence (diffstat, verify tail,
// spend) survives the graded wrapper — without this, every graded fall-through
// flattened to Describe() and the operator decided the irreversible step from one
// line. A plain approver keeps the exact prior flat path.
func (g *GradedApprover) fallThrough(a policy.GateAction) bool {
	if sa, ok := g.human.(policy.StructuredApprover); ok {
		return sa.ApproveStructured(a)
	}
	return g.human.Approve(a.Describe())
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
	if killSwitchEngaged(g.root) {
		g.emitDeny("killswitch", typ, scope, nil)
		return g.fallThrough(a)
	}

	// (2) Eligibility — no clause for this Type ⇒ human.
	clause, ok := g.clauseFor(typ)
	if !ok {
		g.emitDeny("not_eligible", typ, scope, nil)
		return g.fallThrough(a)
	}

	// (3) Blast radius — protected bases (prod*, main/master/release*), DenyBranches
	// ALWAYS win; AllowBranches must admit the scope; for Deploy the Environments
	// allowlist must admit it too. isProtectedBase is the STRUCTURAL floor that holds
	// even if a custom envelope's DenyBranches omits main/master/release (charter:
	// graduated auto-approval never auto-approves main/prod).
	if isProd(scope) || isProtectedBase(scope) || matchAny(scope, clause.DenyBranches) {
		g.emitDeny("out_of_scope", typ, scope, map[string]any{"protected": true})
		return g.fallThrough(a)
	}
	if !matchAny(scope, clause.AllowBranches) {
		g.emitDeny("out_of_scope", typ, scope, nil)
		return g.fallThrough(a)
	}
	// Deploy branch: DORMANT until a deploy flow constructs a policy.GateAction{Type:
	// Deploy} (docs/ROADMAP-DEPLOY.md). No production code emits one today, so this arm is
	// never reached in a real run — the only gated action the live paths produce is
	// PromoteToBase. It is kept (with the matching trusted-preset deploy clause in
	// presets.go) as tested scaffolding so the Environments allowlist is enforced the moment
	// the roadmapped deploy flow lands: a Deploy is auto-approved only into an env the
	// clause explicitly allowlists (staging), never a bare/absent Environments set.
	if a.Type == policy.Deploy {
		if len(clause.Environments) == 0 || !matchAny(scope, clause.Environments) {
			g.emitDeny("out_of_scope", typ, scope, map[string]any{"environment": scope})
			return g.fallThrough(a)
		}
	}

	// (4) Trust bar — rebuilt from the log per decision via the memoizing incremental
	// builder (folds only new events, skips scan+Verify when unchanged); a chain error
	// denies EXPLICITLY (a tampered log earns nothing).
	view, err := g.trust.Build(g.logPath)
	if err != nil || !view.ChainOK {
		g.emitDeny("chain_broken", typ, scope, map[string]any{"chain_ok": view.ChainOK})
		return g.fallThrough(a)
	}
	// Trust accrues over the scope FAMILY (see trustScope): every live scope is unique
	// per run, so an exact-scope tally could never reach MinSuccesses. Tally normalizes
	// the concrete scope to the family BuildTrust bucketed it under.
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
		return g.fallThrough(a)
	}

	// (5) Rate — per-UTC-day auto_approve count for (type,scope) must be < MaxPerDay.
	// reserveRate is the ATOMIC authority: under a lock it seeds the in-process count
	// from the durable log once per window (so a restart never resets it), then does a
	// single check-and-increment so two concurrent decisions cannot both slip past the
	// cap. It returns the count observed BEFORE the reservation (for the evidence
	// object) and reserved=true only when a slot was taken. A seed read fault fails
	// closed (reserved=false, a non-nil error) rather than silently under-counting.
	today := dayKey(now)
	rate, reserved, rerr := g.reserveRate(typ, scope, today, clause.MaxPerDay)
	if !reserved {
		g.emitDeny("rate_exceeded", typ, scope, map[string]any{
			"rate": rate, "max_per_day": clause.MaxPerDay, "seed_error": rerr != nil,
		})
		return g.fallThrough(a)
	}

	// (5b) Blast irreversible fence — every auto-approval consumes one slot of the
	// SHARED blast-radius meter's irreversible axis. This is the composition law in
	// code (min(P5, blast)): a P5 grant proceeds ONLY within the remaining blast
	// envelope, and a breach is FINAL — fall through to the human. A nil g.blast (no
	// -blast-radius preset) is a no-op, so an unfenced run is byte-identical (the
	// irreversible axis bites only when an operator sets a ceiling).
	ctx := context.Background()
	if cerr := g.blast.ChargeIrreversible(ctx, 1); cerr != nil {
		g.releaseRate(typ, scope, today) // this action did not proceed — release its rate slot
		g.emitDeny("blast_radius", typ, scope, map[string]any{"axis": "irreversible"})
		return g.fallThrough(a)
	}

	// (5c) Dollars — MaxDollarsDay is the per-UTC-day CEILING for this class, enforced
	// against the ACTUAL per-day auto-approved-dollar total (dollarsDay), NOT against a
	// per-action delta of the run ledger. Evidence.SpentUSD is the run ledger's CUMULATIVE
	// total (build.go sets it to ledger.Total() on every gated action), so it is NOT this
	// action's incremental cost; a boundary action's own $ is not isolable from that
	// cumulative figure, and no site carries a per-action figure today, so this action's
	// charge (perActionCost) is $0 by default. chargeDay checks whether adding that charge
	// to the day's running auto-approved total would exceed clause.MaxDollarsDay — and, when
	// a blast budget is present, routes the same charge through the SHARED meter so the
	// effective ceiling is min(clause.MaxDollarsDay, blast day ceiling). It commits the
	// in-process day total only when the whole charge lands, so a denied action consumes
	// nothing. On a breach, roll back the irreversible slot and the rate slot we already
	// took, then deny and delegate.
	//
	// The default -blast-radius off (g.blast==nil) is REACHABLE and metered by the clause's
	// OWN MaxDollarsDay: a $0-cost action always fits, and a positive per-day total is bounded
	// by the clause ceiling in-process — we do NOT blanket-deny as "unmetered" merely because
	// the run has spent money (the prior bug, which disabled graduated auto-approval in its
	// default mode). A clause with MaxDollarsDay==0 means "$-unbounded not allowed" and stays
	// deny for any positive charge; today's $0-cost actions still pass a zero ceiling.
	charged, cerr := g.chargeDay(ctx, a, today, clause.MaxDollarsDay)
	if cerr != nil {
		g.blast.CreditIrreversible(1)    // this action did not proceed — release its slot
		g.releaseRate(typ, scope, today) // …and its rate slot
		g.emitDeny("over_ceiling", typ, scope, map[string]any{
			"max_dollars_day": clause.MaxDollarsDay, "actual_usd": charged,
		})
		return g.fallThrough(a)
	}

	// Full pass ⇒ emit auto_approve with the full evidence object and return true.
	// dollars.charged and the explicit dollars.actual_usd both carry the ACTUAL amount
	// charged for this action (NOT the clause ceiling), so per-day $ accounting
	// (cmd/nilcore/blast.go rebuildBlastDay) can sum real spend; max_dollars_day carries
	// the ceiling. actual_usd is emitted as the unambiguous, purpose-named field so a
	// reader never has to know that `charged` happens to equal the actual cost, and it is
	// the field sumAutoApprovalDollarsToday re-reads to reseed the per-day total after a
	// restart.
	g.emit("auto_approve", map[string]any{
		"action": typ, "scope": scope,
		"green": t.Green, "total": t.Total,
		"last_green": t.LastGreen.UTC().Format(time.RFC3339),
		"bar":        map[string]any{"min_successes": clause.MinSuccesses, "min_sample": clause.MinSample, "recency_days": clause.RecencyDays},
		"rate":       map[string]any{"count": rate, "max_per_day": clause.MaxPerDay},
		"dollars":    map[string]any{"charged": charged, "actual_usd": charged, "max_dollars_day": clause.MaxDollarsDay},
		"chain_ok":   view.ChainOK,
	})
	return true
}

// perActionCost is THIS action's own incremental dollar cost — the amount a single
// auto-approval contributes to the per-day auto-approved total. It is deliberately NOT
// derived from a.Evidence.SpentUSD: that field is the run ledger's CUMULATIVE total (set
// by build.go to ledger.Total() on every gated action), so it is dominated by prior model
// work and is not this boundary action's cost. No GateAction field carries a per-action $
// figure today, so this returns 0 — which keeps today's auto-approvals reachable while the
// per-day CEILING (MaxDollarsDay) still bounds the running total the moment any real
// per-action signal is wired here. Isolating one action's $ from the cumulative ledger is
// out of scope for this package.
//
// It is a var, not a func, for one reason: it is the SINGLE place a real per-action $
// signal will be read when one exists (so the day-total metering below "just works"), and
// tests inject a positive cost through it to exercise the ceiling. Production never
// reassigns it.
var perActionCost = func(a policy.GateAction) float64 { return 0 }

// chargeDay enforces clause.MaxDollarsDay against the ACTUAL per-day auto-approved-dollar
// total. Under rateMu (which guards dollarsDay) it lazily SEEDS the day's total from the
// durable log once per (type,scope,day) window (sumAutoApprovalDollarsToday), so a restart
// recovers the day's prior auto-approved spend; then it does an atomic check-and-commit:
//
//   - cost := perActionCost(a) — this action's own $ (0 today; NEVER the cumulative ledger).
//   - if maxDollarsDay > 0 and dayTotal+cost would exceed it ⇒ refuse (over the clause
//     ceiling). maxDollarsDay == 0 means "$-unbounded not allowed": any positive cost
//     refuses, a $0 cost passes.
//   - when g.blast != nil, route the SAME cost through the shared meter
//     (ChargeAutoApprovalDollars), which enforces the blast day ceiling. The tighter of the
//     two bites first, so the effective ceiling is min(clause.MaxDollarsDay, blast dayCeil).
//     The blast charge is attempted only AFTER the clause check passes, and if it refuses
//     nothing is committed to dollarsDay — the two meters never disagree.
//   - g.blast == nil (default -blast-radius off) is REACHABLE: the clause ceiling alone
//     bounds the in-process day total. We never blanket-deny "unmetered" — a run spending
//     money on prior work does not disable graduated auto-approval.
//
// The in-process day total is committed ONLY on a landed charge, so a refused/denied action
// consumes no dollars and leaves the accounting exactly where it was. Two concurrent
// decisions sharing one approver serialize here and can never both push the same day over
// its ceiling.
func (g *GradedApprover) chargeDay(ctx context.Context, a policy.GateAction, today string, maxDollarsDay float64) (charged float64, err error) {
	cost := perActionCost(a)

	g.rateMu.Lock()
	defer g.rateMu.Unlock()

	// Key the per-day $ window on the scope FAMILY, exactly as the rate window does.
	// Every live scope is unique per run, so an exact-branch key would open a fresh $
	// window on every decision and MaxDollarsDay would never bind — the same
	// ephemeral-scope defect, on the money axis.
	key := rateKey(a.Type.String(), trustScope(scopeFor(a)), today)
	dayTotal, seeded := g.dollarsDay[key]
	if !seeded {
		// First access of this window this process — seed the day's auto-approved $ from
		// the durable log so a restart recovers prior spend. A read/parse fault fails closed.
		seed, serr := sumAutoApprovalDollarsToday(g.logPath, a.Type.String(), scopeFor(a), today)
		if serr != nil {
			return 0, serr
		}
		dayTotal = seed
		g.dollarsDay[key] = dayTotal // record the seed so we never re-scan this window
	}

	// Clause ceiling: MaxDollarsDay <= 0 means "$-unbounded not allowed" ⇒ only a $0 cost
	// fits; a positive ceiling admits the charge only while the day total stays within it.
	if maxDollarsDay <= 0 {
		if cost > 0 {
			return cost, errors.New("graapprove: MaxDollarsDay is 0 (unbounded $ not allowed)")
		}
	} else if dayTotal+cost > maxDollarsDay+dollarEpsilon {
		return cost, errors.New("graapprove: over clause MaxDollarsDay")
	}

	// Shared blast meter (when present) enforces min(clause, blast dayCeil). Attempt it
	// only after the clause check passes; on refusal commit nothing.
	if g.blast != nil {
		if cerr := g.blast.ChargeAutoApprovalDollars(ctx, today, cost); cerr != nil {
			return cost, cerr
		}
	}

	// The charge landed — commit it to the day total so a later action in the same window
	// is bounded by the remaining headroom.
	g.dollarsDay[key] = dayTotal + cost
	return cost, nil
}

// dollarEpsilon absorbs float64 rounding when comparing a day total against the clause
// ceiling, mirroring blastbudget.epsilon so a charge meant to land exactly on the ceiling
// is not spuriously refused.
const dollarEpsilon = 1e-9

// sumAutoApprovalDollarsToday folds the append-only log READ-ONLY and sums the
// dollars.actual_usd each `auto_approve` event recorded for (action,scope) whose event-day
// equals today (UTC). This reseeds the per-day auto-approved-dollar total from the durable
// log on first access of a window, so a restart never resets it (mirroring
// countAutoApprovalsToday for the rate axis). A missing log is 0. A read/parse fault returns
// the sum so far plus the error so the caller can fail closed (deny) rather than under-count.
func sumAutoApprovalDollarsToday(logPath, action, scope, today string) (float64, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var sum float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e boundaryEvent // reuse: same {Time,Kind,Detail} shape
		if err := json.Unmarshal(line, &e); err != nil {
			return sum, err
		}
		if e.Kind != "auto_approve" {
			continue
		}
		a, _ := e.Detail["action"].(string)
		s, _ := e.Detail["scope"].(string)
		// The event records the CONCRETE scope; the day's $ window sums the family, to
		// match chargeDay's key. Comparing exact scopes would seed every fresh branch's
		// window at zero and the ceiling would never bind.
		if a != action || trustScope(s) != trustScope(scope) {
			continue
		}
		if dayKey(e.Time) != today {
			continue
		}
		// dollars.actual_usd is the purpose-named field the auto_approve evidence carries;
		// fall back to dollars.charged for older events that predate actual_usd.
		if d, ok := e.Detail["dollars"].(map[string]any); ok {
			if v, ok := d["actual_usd"].(float64); ok {
				sum += v
			} else if v, ok := d["charged"].(float64); ok {
				sum += v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return sum, err
	}
	return sum, nil
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

// rateKey renders the per-window counter key: (type|scope|yyyy-mm-dd). The day is
// part of the key, so the window rolls at midnight UTC by construction and a stale
// day's count is simply never read again.
func rateKey(typ, scope, today string) string {
	return typ + "|" + scope + "|" + today
}

// reserveRate is the atomic authority for the per-day rate cap. Under rateMu it
// lazily SEEDS the in-process count for the (type,scope,day) window from the durable
// log exactly once (on first access of the key), then does a single
// check-and-increment: if the effective count is already at/over maxPerDay it grants
// nothing (reserved=false), otherwise it increments and reserves a slot
// (reserved=true). It returns the count observed BEFORE any increment (for the
// evidence/deny object). A maxPerDay of 0 means UNCAPPED — every call reserves. A
// seed read fault fails closed (reserved=false, non-nil error) so a transient log
// error denies rather than silently under-counting.
//
// Holding the mutex is the whole point: two concurrent decisions for the same window
// serialize here, so they observe each other's increment and cannot both pass a cap
// of 1. The log's auto_approve event (emitted on a full pass) remains the durable
// record that re-seeds this counter after a restart.
func (g *GradedApprover) reserveRate(typ, scope, today string, maxPerDay int) (before int, reserved bool, err error) {
	if maxPerDay <= 0 {
		return 0, true, nil // uncapped: never blocks, never seeds (preserves prior behavior)
	}
	g.rateMu.Lock()
	defer g.rateMu.Unlock()

	// Key the in-process window on the scope FAMILY, matching the durable seed below.
	// A per-run-unique branch would otherwise mint a fresh window on every decision and
	// MaxPerDay would never bind.
	key := rateKey(typ, trustScope(scope), today)
	cur, seeded := g.rateCount[key]
	if !seeded {
		// First access of this window this process — seed from the durable log so a
		// restart recovers the day's prior approvals. A read/parse fault fails closed.
		seed, serr := countAutoApprovalsToday(g.logPath, typ, scope, today)
		if serr != nil {
			return seed, false, serr
		}
		cur = seed
		g.rateCount[key] = cur // record the seed so we never re-scan this window
	}
	if cur >= maxPerDay {
		return cur, false, nil // window exhausted — no slot
	}
	g.rateCount[key] = cur + 1 // reserve atomically under the lock
	return cur, true, nil
}

// releaseRate returns a reserved slot to its window. It is called only when a
// decision that already reserved a rate slot later falls through to the human (a
// blast-radius or dollar-ceiling breach), so a denied action consumes nothing and the
// in-process counter stays consistent with the durable log (which only records
// auto_approve on a full pass). A maxPerDay==0 window never reserved, so a missing key
// is a safe no-op (guarded so the count can never go negative).
func (g *GradedApprover) releaseRate(typ, scope, today string) {
	g.rateMu.Lock()
	defer g.rateMu.Unlock()
	key := rateKey(typ, trustScope(scope), today) // same family key reserveRate took
	if n := g.rateCount[key]; n > 0 {
		g.rateCount[key] = n - 1
	}
}
