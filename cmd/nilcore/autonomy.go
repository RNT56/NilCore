package main

// autonomy.go folds the autonomy daemon into serve (Phase 16, Pillar 7 / AUTO-T06):
// when NILCORE_AUTONOMY is set, serve drains ONE bounded priority queue (internal/
// autosrc) that unifies every self-start funnel — the operator standing-objectives
// backlog, dropped file signals, and fired durable wakes — through the SAME verified
// run-orchestrator, executing each goal REVERSIBLY (a disposable worktree, discarded)
// and gating only at the irreversible edge.
//
// Unified sources (AUTO-T04 adapters, now wired):
//   - BacklogSource — operator standing objectives, pulled only when serve is IDLE
//     (a reactive conversation always preempts background self-service).
//   - FileSource    — files dropped in the -autonomy-signals dir (reactive; the same
//     "drop a goal, it runs" funnel as `nilcore watch`, but on the unified queue).
//   - WakeSource    — durable self-timers (the `sleep` tool's wakes) that come due.
//     Before this, serve armed wakes but NEVER fired them; the wake feeder closes
//     that gap, re-engaging each due wake's note through the queue.
// (CronSource and the webhook push path feed the same queue and remain available;
// cron needs a configured job set — the `nilcore schedule` verb's domain — so it is
// not auto-wired here.)
//
// Safety stance (unchanged): the daemon holds NO authority — it forwards an inert goal
// (I7) to the verified orchestrator, which owns verification (I2) and the HEADLESS gate
// (irreversible work deny-defaults; I3). DEFAULT-OFF: unset NILCORE_AUTONOMY ⇒ never
// started; an empty backlog + empty signals dir + no wakes ⇒ the queue stays empty,
// byte-identical.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nilcore/internal/agent"
	"nilcore/internal/autosrc"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/objective"
	"nilcore/internal/store"
	"nilcore/internal/trigger"
	"nilcore/internal/wake"
)

// autonomyPollInterval is how often the file + wake feeders scan for new work. A
// modest cadence keeps a long-running serve cheap while staying responsive enough for
// background self-service (reactive chat is unaffected — it never goes through here).
const autonomyPollInterval = 15 * time.Second

// runAutonomyDaemon drives the unified autosrc daemon until ctx is cancelled (serve
// shutdown), draining gracefully. The orchestrator, store, and wake registry are owned
// by the caller (serve, at startup) so a missing model key fails loudly at boot, and
// the whole serve process shares ONE *sql.DB. wakeReg may be nil (no `sleep` durability
// ⇒ no wake funnel); signalsDir may be "" (no file funnel).
func runAutonomyDaemon(ctx context.Context, orch *agent.Orchestrator, log *eventlog.Log, s *store.Store, idle func() bool, wakeReg *wake.Registry, signalsDir string) {
	if s == nil {
		fmt.Fprintln(os.Stderr, "nilcore: autonomy daemon disabled (no store)")
		return
	}

	handler := func(ctx context.Context, sig trigger.Signal) error {
		// Run the inert goal (objective / file signal / wake note) through the verified
		// orchestrator: reversible by construction (a disposable worktree), with every
		// irreversible step hitting the headless gate the caller wired onto orch.Approver.
		_, err := runViaKernel(ctx, orch, backend.Task{
			ID:   fmt.Sprintf("auto-%d", time.Now().UnixNano()),
			Goal: sig.Goal,
		})
		return err
	}

	// Source 1: the operator standing-objectives backlog (idle-gated).
	backlog := objective.New(s.ObjectiveStore())
	sources := []autosrc.Source{autosrc.NewBacklogSource(backlog, autosrc.BacklogConfig{Idle: idle})}

	// Source 2: dropped file signals (reactive). A feeder goroutine owns the directory
	// poll + once-only removal; the FileSource adapter only maps each into the queue.
	if strings.TrimSpace(signalsDir) != "" {
		fileCh := make(chan autosrc.FileSignal)
		go fileSignalFeeder(ctx, signalsDir, fileCh, autonomyPollInterval)
		sources = append(sources, autosrc.FileSource{Signals: fileCh})
	}

	// Source 3: due durable wakes (reactive). A feeder polls the registry, fires each
	// wake whose instant has passed, and disarms it so it never re-fires.
	if wakeReg != nil {
		wakeCh := make(chan autosrc.Wake)
		go wakeFeeder(ctx, wakeReg, wakeCh, autonomyPollInterval)
		sources = append(sources, autosrc.WakeSource{Fires: wakeCh})
	}

	d := autosrc.New(handler, autosrc.Config{Concurrency: 1, Log: log})
	if err := d.Run(ctx, sources...); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "nilcore: autonomy daemon stopped: %v\n", err)
	}
}

// fileSignalFeeder polls dir on a ticker, reading each file as a FileSignal (name +
// trimmed contents) and removing it (processed once), then sending it onto ch for the
// FileSource adapter. It owns the host I/O so the adapter stays a pure mapper. It
// closes ch and returns when ctx is cancelled (the source then reports DONE).
func fileSignalFeeder(ctx context.Context, dir string, ch chan<- autosrc.FileSignal, interval time.Duration) {
	defer close(ch)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a missing/transient dir is not fatal — try again next tick
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(dir, e.Name())
			body, rerr := os.ReadFile(path)
			_ = os.Remove(path) // processed once, regardless of outcome
			if rerr != nil {
				continue
			}
			goal := strings.TrimSpace(string(body))
			if goal == "" {
				continue
			}
			select {
			case ch <- autosrc.FileSignal{Name: e.Name(), Goal: goal}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// wakeFeeder polls the wake registry on a ticker and fires every wake whose instant has
// passed: it DISARMS the wake first (so a slow/busy queue can never re-queue it on the
// next tick — at-most-once), then sends it onto ch for the WakeSource adapter. This is
// what makes durable self-timers actually re-engage in serve (they were armed but never
// fired before). A registry read error is skipped, never fatal.
func wakeFeeder(ctx context.Context, reg *wake.Registry, ch chan<- autosrc.Wake, interval time.Duration) {
	defer close(ch)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pending, err := reg.Pending(ctx)
		if err != nil {
			continue
		}
		now := time.Now()
		for _, w := range pending {
			if w.WakeAt.After(now) {
				continue // not due yet
			}
			// Disarm before sending so a long, busy queue cannot re-queue it next tick.
			if derr := reg.Disarm(ctx, w.ThreadID); derr != nil {
				continue // could not disarm ⇒ skip rather than risk a re-fire loop
			}
			select {
			case ch <- autosrc.Wake{ThreadID: w.ThreadID, Note: w.Note}:
			case <-ctx.Done():
				return
			}
		}
	}
}
