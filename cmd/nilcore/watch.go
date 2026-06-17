package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/policy"
	"nilcore/internal/trigger"
	"nilcore/internal/worktree"
)

// watchMain implements `nilcore watch` — the self-start loop (P3-T07). It polls a
// signals directory; each file dropped there is a Signal (its name is the source,
// its trimmed contents the goal). Reversible work AUTO-STARTS as a normal verified
// task; anything classified irreversible routes to the HUMAN GATE first. Every
// signal is audited and the file is removed once read, so a signal fires exactly
// once. Configurable on/off and bounded by the poll interval; Ctrl-C stops. The
// agent can never bypass the gate or any invariant — self-started work runs through
// the same orchestrator path as `nilcore -goal`.
func watchMain(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	signalsDir := fs.String("signals", "./signals", "directory polled for signal files")
	interval := fs.Duration("interval", 5*time.Second, "poll interval")
	enabled := fs.Bool("enabled", true, "master on/off for self-start")
	openPR := fs.Bool("open-pr", false, "after a verified self-start, offer a gated draft PR (needs NILCORE_FORGE_TOKEN + an origin remote)")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))
	absDir := mustAbs(*c.dir)
	sigDir := mustAbs(*signalsDir)
	log := openLog(*c.logPath)
	defer log.Close()

	orch := buildRunOrchestrator(c, b, log, absDir)
	if *openPR {
		orch.KeepBranch = true // preserve the verified branch so a gated PR can push it (D4)
	}
	gate := policy.NewConsoleApprover(os.Stdin, os.Stdout)
	trig := &trigger.Trigger{
		Enabled: *enabled,
		Gate:    gate.Approve,
		Start: func(ctx context.Context, goal string) error {
			// UnixNano (not Unix-second) so two ticks in the same second never collide
			// on the kept task/<id> branch under --open-pr.
			out, err := orch.Execute(ctx, backend.Task{ID: fmt.Sprintf("trig-%d", time.Now().UnixNano()), Goal: goal})
			if err != nil {
				return err
			}
			fmt.Printf("self-started: verified=%v — %s\n", out.Verified, out.Summary)
			if *openPR && out.Verified && out.Branch != "" {
				openGatedPR(ctx, absDir, out.Branch, goal, gate, b.cred, log)
				// Reclaim the now-redundant LOCAL branch: an approved PR references the
				// pushed REMOTE branch, a denial pushed nothing — either way the local
				// task/<id> ref serves no purpose. Without this it leaks unboundedly.
				worktree.DeleteBranch(ctx, absDir, out.Branch)
			}
			return nil
		},
		Log: log,
	}

	if err := os.MkdirAll(sigDir, 0o755); err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "nilcore watch: polling %s every %s (enabled=%v, Ctrl-C to stop)\n", sigDir, *interval, *enabled)

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pollSignals(ctx, trig, sigDir)
		}
	}
}

// pollSignals reads each file in dir as a Signal{Source: name, Goal: contents},
// hands it to the trigger, and removes it (processed once). A read error on one
// entry skips it; a directory read error is logged and the tick is skipped, never
// fatal — the watcher must survive a transient filesystem hiccup.
func pollSignals(ctx context.Context, trig *trigger.Trigger, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore watch: %v\n", err)
		return
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
		if _, herr := trig.Handle(ctx, trigger.Signal{Source: "file:" + e.Name(), Goal: goal}); herr != nil {
			fmt.Fprintf(os.Stderr, "nilcore watch: signal %q: %v\n", e.Name(), herr)
		}
	}
}
