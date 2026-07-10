// Package trigger lets the agent self-start reversible work without being asked
// (P3-T07): it watches signals (failing CI, flagged issues) and, for reversible
// work, initiates a task; anything irreversible routes to the human gate. It can
// never bypass the gate or any invariant — every self-start is announced and
// fully audited, and it is configurable on/off.
package trigger

import (
	"context"

	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

// Signal is an observed condition that may warrant self-started work.
type Signal struct {
	Source string // e.g. "ci", "issue"
	Goal   string // the natural-language task it suggests
}

// Trigger decides whether to act on signals.
type Trigger struct {
	Enabled bool                                         // master on/off
	Gate    func(action string) bool                     // the orchestrator's gate (irreversible)
	Start   func(ctx context.Context, goal string) error // start a task
	// Limiter, when set, bounds how OFTEN self-starts may fire (a per-day cap plus a
	// cooldown) on top of any execution mutex the caller holds — the denial-of-wallet
	// fence for untrusted, headless triggers like the webhook intake, where a signed
	// delivery proves only that it was relayed, not that a human authorized a run. nil
	// disables the bound, so operator-initiated sources (cron, watch) stay unlimited.
	Limiter *RateLimiter
	Log     *eventlog.Log
}

// Handle acts on a signal. Reversible work auto-starts; irreversible work must
// pass the gate first. It returns whether a task was started — which is true
// only when a runnable Start was invoked, so a nil Start returns (false, nil)
// and emits no trigger_start event (the bool and the audit trail never claim
// work that did not begin).
func (t *Trigger) Handle(ctx context.Context, sig Signal) (started bool, err error) {
	if !t.Enabled {
		return false, nil
	}
	action := sig.Goal

	if policy.Classify(action) == policy.Irreversible {
		if t.Gate == nil || !t.Gate(action) {
			t.Log.Append(eventlog.Event{Kind: "trigger_gated",
				Detail: map[string]any{"source": sig.Source, "goal": action}})
			return false, nil
		}
	}

	// Nothing runs without a runnable Start. Returning started=true with a nil
	// Start would claim work began when none did and emit a misleading
	// trigger_start audit event — so guard BEFORE the event so the bool and the
	// audit trail both reflect reality.
	if t.Start == nil {
		return false, nil
	}

	// Rate fence (denial-of-wallet): the execution mutex only SERIALIZES runs; this
	// bounds how MANY self-starts fire (per-day cap + cooldown) so a stream of
	// validly-signed but unauthorized deliveries cannot spin up unbounded runs. A
	// rejected self-start is audited (I5) with its reason and reported as not-started,
	// so neither the bool nor the trail claims work that never began. A nil Limiter is
	// unbounded (checked LAST, so a denied or nil-Start action never charges budget).
	if ok, reason := t.Limiter.Allow(); !ok {
		t.Log.Append(eventlog.Event{Kind: "trigger_ratelimited",
			Detail: map[string]any{"source": sig.Source, "goal": action, "reason": reason}})
		return false, nil
	}

	t.Log.Append(eventlog.Event{Kind: "trigger_start",
		Detail: map[string]any{"source": sig.Source, "goal": action}})
	return true, t.Start(ctx, action)
}
