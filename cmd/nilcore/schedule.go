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
	every := fs.Duration("every", time.Hour, "interval between runs")
	goal := fs.String("goal", "", "the goal to self-start on each tick (required)")
	name := fs.String("name", "scheduled", "job name (for the audit log)")
	enabled := fs.Bool("enabled", true, "master on/off for self-start")
	c := registerCommon(fs)
	_ = fs.Parse(args)
	if *goal == "" {
		fatal(fmt.Errorf("schedule: -goal is required"))
	}

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))
	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()

	orch := buildRunOrchestrator(c, b, log, absDir)
	gate := policy.NewConsoleApprover(os.Stdin, os.Stdout)
	trig := &trigger.Trigger{
		Enabled: *enabled,
		Gate:    gate.Approve,
		Start: func(ctx context.Context, goal string) error {
			out, err := orch.Execute(ctx, backend.Task{ID: fmt.Sprintf("cron-%d", time.Now().Unix()), Goal: goal})
			if err != nil {
				return err
			}
			fmt.Printf("scheduled run: verified=%v — %s\n", out.Verified, out.Summary)
			return nil
		},
		Log: log,
	}

	sched := &cron.Scheduler{
		Jobs: []cron.Job{{Name: *name, Every: *every, Goal: *goal, Source: "cron"}},
		Fire: trig.Handle,
		Log:  log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "nilcore schedule: %q every %s (enabled=%v, Ctrl-C to stop)\n", *goal, *every, *enabled)

	if err := sched.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fatal(err)
	}
}
