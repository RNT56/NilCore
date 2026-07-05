package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"nilcore/internal/blastbudget"
	"nilcore/internal/eventlog"
)

// blast.go is the cmd-side wiring for the blast-radius capability budget (Phase
// 16, BR-T05): the hard runtime fence that bounds an unattended / auto-approval
// run beyond dollars. The `-blast-radius` preset expands to a vetted envelope;
// "off" (the default) mints NO budget, so an unfenced run is byte-identical.

// blastEnvelope is a vetted per-preset ceiling tuple. The values are operator-
// approved defaults (docs/ROADMAP-CLOSED-LOOP.md §10); no preset is unbounded.
type blastEnvelope struct {
	hosts, irrev int
	wall         time.Duration
	dollarsDay   float64
}

var blastPresets = map[string]blastEnvelope{
	"tight":    {hosts: 4, irrev: 2, wall: 10 * time.Minute, dollarsDay: 1},
	"standard": {hosts: 8, irrev: 5, wall: 20 * time.Minute, dollarsDay: 5},
}

// mintBlastBudget builds the run's blast-radius budget from the -blast-radius
// preset, or returns nil for "off"/unknown (the default ⇒ the run is unfenced).
// A nil *blastbudget.Budget is a no-op at every Charge* site, so an unset preset
// changes nothing (byte-identical default-off). The budget is the SINGLE shared
// meter the graduated-auto-approval policy reads for its $/rate ceiling.
func mintBlastBudget(preset string, log *eventlog.Log) *blastbudget.Budget {
	switch preset {
	case "", "off":
		return nil // intentional: unfenced (the default), byte-identical
	}
	env, ok := blastPresets[preset]
	if !ok {
		// A typo/unknown value on a SAFETY flag must NEVER silently disable the fence
		// (fail-open is the dangerous direction for an unattended/auto-approval run).
		// Fail CLOSED to the tightest envelope and warn loudly so the operator notices.
		fmt.Fprintf(os.Stderr, "nilcore: unknown -blast-radius %q; falling back to \"tight\" (valid: off|tight|standard)\n", preset)
		env = blastPresets["tight"]
		if log != nil {
			log.Append(eventlog.Event{Kind: "blast_radius_unknown", Detail: map[string]any{"value": preset, "fallback": "tight"}})
		}
	}
	b := blastbudget.New()
	b.SetHostCeiling(env.hosts)
	b.SetIrreversibleCeiling(env.irrev)
	b.SetWallCeiling(env.wall)
	b.SetAutoApprovalDollarCeiling(env.dollarsDay)
	// XC-T04 rebuild-on-boot: re-establish TODAY's prior auto-approval $ from the
	// durable log BEFORE the sink is attached (so the pre-charge emits no event), so a
	// restart cannot reset the per-UTC-day $ ceiling (no fail-open on restart — I5).
	rebuildBlastDay(b, log.Path())
	b.SetSink(blastSink{log})
	return b
}

// rebuildBlastDay re-loads the current UTC day's already-spent auto-approval dollars
// from the append-only log into a fresh budget, so the per-day $ ceiling survives a
// process restart (the rate window and trust view already rebuild from the log per
// decision; this closes the same gap for the $ axis). It sums each `auto_approve`
// event's ACTUAL charged spend whose Time is today and pre-charges that total.
// Best-effort and READ-ONLY: a missing/unreadable log or a malformed line just
// contributes nothing (a fresh install has no prior spend). A nil budget or empty path
// is a no-op.
//
// ACTUAL SPEND, not the ceiling. graapprove now records the ACTUAL per-action dollar
// cost it charged in `dollars.charged` (previously it charged — and logged — the whole
// clause CEILING per action, which both self-exhausted a smaller blast day budget and
// made per-day $ accounting coarse). We sum that actual value. For forward/backward
// compatibility with logs written before that change, we prefer an explicit
// `dollars.actual_usd` when present and otherwise fall back to `dollars.charged`; both
// now carry the actual spend, so the sum reflects real dollars either way.
//
// CONSERVATIVE BY DESIGN: it counts EVERY auto-approval that occurred today, including
// any taken while a prior run was unfenced (`-blast-radius off`, so no budget was
// charged at the time). The spend genuinely happened, so a newly-fenced run accounts
// for the day's full auto-approval $ rather than under-counting it — the fail-safe
// direction for a ceiling. (There is no over-count within a process: the live charge
// happens for NEW decisions, the rebuild only re-establishes PAST ones at mint time.)
func rebuildBlastDay(b *blastbudget.Budget, logPath string) {
	if b == nil || logPath == "" {
		return
	}
	f, err := os.Open(logPath)
	if err != nil {
		return // no log yet ⇒ nothing to rebuild
	}
	defer f.Close()

	today := time.Now().UTC().Format("2006-01-02")
	var sum float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e struct {
			Time   time.Time      `json:"time"`
			Kind   string         `json:"kind"`
			Detail map[string]any `json:"detail"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != "auto_approve" {
			continue
		}
		if e.Time.UTC().Format("2006-01-02") != today {
			continue
		}
		if d, ok := e.Detail["dollars"].(map[string]any); ok {
			sum += autoApprovalActualUSD(d)
		}
	}
	if sum > 0 {
		_ = b.ChargeAutoApprovalDollars(context.Background(), today, sum)
	}
}

// autoApprovalActualUSD reads the ACTUAL dollars an auto_approve event charged from its
// `dollars` sub-object. It prefers the explicit `actual_usd` field (emitted on a
// dollar-bearing auto-approval) and otherwise falls back to `charged` — which, after
// the graapprove actual-spend fix, also carries the actual amount. A missing/non-float
// value contributes 0. This is the single place the per-day $ accounting reads a
// spend figure, so the "which field is the actual spend" decision lives in one spot.
func autoApprovalActualUSD(dollars map[string]any) float64 {
	if v, ok := dollars["actual_usd"].(float64); ok {
		return v
	}
	if v, ok := dollars["charged"].(float64); ok {
		return v
	}
	return 0
}

// blastSink adapts the run's append-only log to blastbudget.Sink: metadata-only
// blast_charge / blast_breach events (axis/used/ceiling/host) — no secret, no
// model body (I3/I7); eventlog's redact() is the backstop. A nil log is a no-op.
type blastSink struct{ log *eventlog.Log }

func (s blastSink) Emit(kind string, detail map[string]any) {
	s.log.Append(eventlog.Event{Kind: kind, Detail: detail})
}
