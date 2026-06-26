package main

import (
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
	b.SetSink(blastSink{log})
	return b
}

// blastSink adapts the run's append-only log to blastbudget.Sink: metadata-only
// blast_charge / blast_breach events (axis/used/ceiling/host) — no secret, no
// model body (I3/I7); eventlog's redact() is the backstop. A nil log is a no-op.
type blastSink struct{ log *eventlog.Log }

func (s blastSink) Emit(kind string, detail map[string]any) {
	s.log.Append(eventlog.Event{Kind: kind, Detail: detail})
}
