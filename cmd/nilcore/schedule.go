package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/cron"
	"nilcore/internal/policy"
	"nilcore/internal/trigger"
	"nilcore/internal/worktree"
)

// scheduleMain implements `nilcore schedule` — the time-driven counterpart to
// `nilcore watch` (P9-T06/P9-T07). On a fixed interval it emits a goal as a
// trigger.Signal through the SAME reversible-auto-start / irreversible-gate
// machinery: reversible scheduled work runs as a normal verified task; anything
// classified irreversible routes to the human gate first.
//
// Headless posture: a scheduled run typically has no human at the console, so the
// ConsoleApprover's gate reads a non-"y" answer (no stdin) as DENY. Irreversible
// scheduled work therefore deny-defaults and does not start — by design, and
// audited via the trigger_gated event. Reversible maintenance (e.g. "bump
// dependencies and open nothing") auto-starts and is verified the usual way.
func scheduleMain(args []string) {
	fs := flag.NewFlagSet("schedule", flag.ExitOnError)
	every := fs.Duration("every", time.Hour, "interval between runs (used when -at is empty)")
	at := fs.String("at", "", "wall-clock schedule (local): @hourly | @daily | HH:MM (24h); overrides -every")
	goal := fs.String("goal", "", "the goal to self-start on each tick (required)")
	name := fs.String("name", "scheduled", "job name (for the audit log)")
	enabled := fs.Bool("enabled", true, "master on/off for self-start")
	openPR := fs.Bool("open-pr", false, "after a verified scheduled run, offer a gated draft PR (needs NILCORE_FORGE_TOKEN + an origin remote)")
	c := registerCommon(fs)
	_ = fs.Parse(args)
	if *goal == "" {
		fatal(fmt.Errorf("schedule: -goal is required"))
	}
	if *at != "" && !cron.ValidAt(*at) {
		fatal(fmt.Errorf("schedule: invalid -at %q (want @hourly, @daily, or HH:MM)", *at))
	}

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))
	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()

	orch := buildRunOrchestrator(c, b, log, absDir)
	if *openPR {
		orch.KeepBranch = true // preserve the verified branch so a gated PR can push it (D4)
	}
	// Graduated auto-approval when an envelope is configured; else the console
	// approver unchanged (byte-identical default-off). See watch.go.
	gate := wrapAutoApprove(policy.NewConsoleApprover(os.Stdin, os.Stdout), b.cfg, *c.logPath, log, nil)
	trig := &trigger.Trigger{
		Enabled: *enabled,
		Gate:    gate.Approve,
		Start: func(ctx context.Context, goal string) error {
			// UnixNano so two ticks in the same second never collide on the kept branch.
			out, err := orch.Execute(ctx, backend.Task{ID: fmt.Sprintf("cron-%d", time.Now().UnixNano()), Goal: goal})
			if err != nil {
				return err
			}
			fmt.Printf("scheduled run: verified=%v — %s\n", out.Verified, out.Summary)
			if *openPR && out.Verified && out.Branch != "" {
				emitBoundaryOutcome(log, "open-pr", out.Branch, out.Verified)
				openGatedPR(ctx, absDir, out.Branch, goal, gate, b.cred, log)
				// Reclaim the now-redundant local task/<id> branch (see watch.go).
				worktree.DeleteBranch(ctx, absDir, out.Branch)
			}
			return nil
		},
		Log: log,
	}

	job := cron.Job{Name: *name, Goal: *goal, Source: "cron"}
	when := fmt.Sprintf("every %s", *every)
	if *at != "" {
		job.At = *at // wall-clock spec overrides the interval
		when = "at " + *at
	} else {
		job.Every = *every
	}
	sched := &cron.Scheduler{
		Jobs: []cron.Job{job},
		Fire: trig.Handle,
		Log:  log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "nilcore schedule: %q %s (enabled=%v, Ctrl-C to stop)\n", *goal, when, *enabled)

	if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
}
