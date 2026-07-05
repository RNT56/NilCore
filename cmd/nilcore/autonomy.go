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
//     Serve's own runWaker also fires wakes; under NILCORE_AUTONOMY it is SUPPRESSED
//     (server.SuppressWaker) so this feeder is the sole firer and every due wake is
//     re-engaged through the verified, headless-gated queue rather than the server's
//     direct re-Turn (no double-delivery, no gate bypass).
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

// autonomyQueueCap bounds the unified priority queue's in-flight depth so the
// daemon honors its "bounded priority queue" contract: a burst of dropped signals,
// due wakes, or backlog objectives cannot grow the queue without limit. A source
// that outruns the (serial) handler gets fast-fail back-pressure (ErrQueueFull),
// which the daemon logs and the feeder retries on the next tick, rather than an
// unbounded memory climb. The cap is generous — background self-service is not a
// high-throughput path — while still being a real fence.
const autonomyQueueCap = 256

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

	d := autosrc.New(handler, autosrc.Config{Concurrency: 1, QueueCap: autonomyQueueCap, Log: log})
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
// passed, re-engaging durable self-timers in serve (they were armed but never fired
// before). A registry read error is skipped, never fatal.
//
// Every due wake is dispatched through Registry.Claim — the durable single-fire
// primitive: Claim atomically takes the wake AND durably disarms it in the store, so a
// wake fires AT MOST ONCE even across a restart or two concurrent pollers (a second
// serve process, or serve's own runWaker, cannot also fire a claimed wake). Only the
// winner of Claim delivers onto the queue; a lost claim (someone else won, or it was
// already fired) is skipped silently. This replaces the previous process-local inFlight
// map, whose at-most-once was only in-memory — a restart or a second process could
// re-fire the same wake, bypassing the single-fire guarantee the registry exists to
// provide (durable at-most-once, the finding's fix).
//
// A Claim failure (store error) leaves the wake armed for a later tick to retry, never
// swallowed. On ctx cancel after a winning Claim the wake is already durably disarmed,
// so it does not re-fire — the accepted at-most-once trade (a rare crash between Claim
// and enqueue drops that one wake rather than risking a double-fire).
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
			// Claim is the durable, atomic single-fire gate: it disarms the wake in the
			// store before returning won=true, so no restart or concurrent poller can
			// re-fire it. A lost claim (won=false) or a store error means someone else
			// owns it (or it will retry next tick) — skip without delivering.
			won, cerr := reg.Claim(ctx, w.ThreadID)
			if cerr != nil || !won {
				continue
			}
			// We durably own this fire. Deliver it; a cancel here drops only this single
			// already-disarmed wake (at-most-once), never a re-fire.
			select {
			case ch <- autosrc.Wake{ThreadID: w.ThreadID, Note: w.Note}:
			case <-ctx.Done():
				return
			}
		}
	}
}
