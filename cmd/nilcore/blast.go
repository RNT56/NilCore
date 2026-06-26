package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	env, ok := blastPresets[preset]
	if !ok {
		return nil // "off" or unknown ⇒ no fence
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
// decision; this closes the same gap for the $ axis). It sums every `auto_approve`
// event's `dollars.charged` whose Time is today and pre-charges that total. Best-effort
// and READ-ONLY: a missing/unreadable log or a malformed line just contributes nothing
// (a fresh install has no prior spend). A nil budget or empty path is a no-op.
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
			if charged, ok := d["charged"].(float64); ok {
				sum += charged
			}
		}
	}
	if sum > 0 {
		_ = b.ChargeAutoApprovalDollars(context.Background(), today, sum)
	}
}

// blastSink adapts the run's append-only log to blastbudget.Sink: metadata-only
// blast_charge / blast_breach events (axis/used/ceiling/host) — no secret, no
// model body (I3/I7); eventlog's redact() is the backstop. A nil log is a no-op.
type blastSink struct{ log *eventlog.Log }

func (s blastSink) Emit(kind string, detail map[string]any) {
	s.log.Append(eventlog.Event{Kind: kind, Detail: detail})
}
