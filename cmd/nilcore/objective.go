package main

// objective.go implements `nilcore objective` — the OPERATOR-ONLY management verb for
// the standing-objectives backlog (Phase 16, Pillar 7 / AUTO-T07). A standing objective
// is a durable operator intent ("keep CI green", "keep deps current") the autonomy
// daemon self-services when idle, executed reversibly through the verified orchestrator,
// gating only at the irreversible edge. This verb is the SOLE write path to the backlog
// and is a HOST verb — it is NEVER registered as a sandboxed model tool (XC-T06), so a
// model can never enqueue, edit, or re-prioritize its own objectives; it may only DO the
// work one selected objective names, and every irreversible step still passes the gate.
//
// Usage:
//
//	nilcore objective list
//	nilcore objective add -id keep-ci-green -goal "keep CI green" [-priority 10] [-period 24h] [-retry-period 1h]
//	nilcore objective disable <id>
//	nilcore objective enable <id>

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"nilcore/internal/objective"
	"nilcore/internal/paths"
	"nilcore/internal/store"
)

// objectiveMain is the `nilcore objective` entrypoint. It opens the durable store and
// dispatches the management subverb. Every path is operator-typed on the host; nothing
// here is reachable from a model tool (XC-T06).
func objectiveMain(args []string) {
	if len(args) == 0 {
		objectiveUsage()
		os.Exit(2)
	}
	s, err := openObjectiveStore()
	if err != nil {
		fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	switch args[0] {
	case "list":
		runObjectiveList(ctx, s)
	case "add":
		runObjectiveAdd(ctx, s, args[1:])
	case "disable":
		runObjectiveSetEnabled(ctx, s, args[1:], false)
	case "enable":
		runObjectiveSetEnabled(ctx, s, args[1:], true)
	default:
		objectiveUsage()
		os.Exit(2)
	}
}

func objectiveUsage() {
	fmt.Fprintln(os.Stderr, "usage: nilcore objective <list | add | disable <id> | enable <id>>\n"+
		"  add: nilcore objective add -id <id> -goal \"<text>\" [-priority N] [-period 24h] [-retry-period 1h]")
}

// openObjectiveStore opens the same durable store the rest of the persistence backbone
// uses (the data-dir SQLite db). It is the operator-only host store — never the model's.
func openObjectiveStore() (*store.Store, error) {
	dir, err := paths.EnsureDir(paths.DataDir())
	if err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, "nilcore.db"))
}

// runObjectiveList prints the backlog deterministically (highest priority first).
func runObjectiveList(ctx context.Context, s *store.Store) {
	objs, err := s.ListObjectives(ctx)
	if err != nil {
		fatal(err)
	}
	if len(objs) == 0 {
		fmt.Fprintln(os.Stdout, "no standing objectives. add one with: nilcore objective add -id <id> -goal \"<text>\"")
		return
	}
	for _, o := range objs {
		state := "enabled"
		if !o.Enabled {
			state = "disabled"
		}
		last := "never"
		if !o.LastRun.IsZero() {
			last = o.LastRun.UTC().Format("2006-01-02T15:04:05Z")
		}
		success := "never"
		if !o.LastSuccess.IsZero() {
			success = o.LastSuccess.UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(os.Stdout, "  %-24s [%s] priority=%d period=%s retry=%s last_run=%s last_success=%s\n    goal: %s\n",
			o.ID, state, o.Priority, o.MinPeriod, o.RetryPeriod, last, success, o.Goal)
	}
}

// runObjectiveAdd inserts or replaces an objective (operator-authored intent).
func runObjectiveAdd(ctx context.Context, s *store.Store, args []string) {
	fs := flag.NewFlagSet("objective add", flag.ExitOnError)
	id := fs.String("id", "", "stable objective id (required)")
	goal := fs.String("goal", "", "operator-authored goal text (required)")
	priority := fs.Int("priority", 0, "higher runs first among due objectives")
	period := fs.Duration("period", 0, "minimum spacing between SUCCESSFUL runs (e.g. 24h; 0 = always due once enabled)")
	retryPeriod := fs.Duration("retry-period", 0, "shorter spacing after an unverified run (e.g. 1h; 0 = fall back to -period)")
	_ = fs.Parse(args)
	if *id == "" || *goal == "" {
		fmt.Fprintln(os.Stderr, "objective add: -id and -goal are required")
		os.Exit(2)
	}
	if err := s.PutObjective(ctx, objective.Objective{
		ID: *id, Goal: *goal, Priority: *priority, Enabled: true,
		MinPeriod: *period, RetryPeriod: *retryPeriod,
	}); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stdout, "added objective %q (priority %d, period %s, retry-period %s, enabled)\n",
		*id, *priority, *period, *retryPeriod)
}

// runObjectiveSetEnabled disables (pauses) or re-enables an objective by id. A disabled
// objective is a paused intent, not a delete (it is retained, never selected).
func runObjectiveSetEnabled(ctx context.Context, s *store.Store, args []string, enabled bool) {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintf(os.Stderr, "usage: nilcore objective %s <id>\n", verb)
		os.Exit(2)
	}
	id := args[0]
	if !enabled {
		if err := s.DisableObjective(ctx, id); err != nil {
			fatal(err)
		}
		fmt.Fprintf(os.Stdout, "disabled objective %q (paused, not deleted)\n", id)
		return
	}
	// Re-enable: read-modify-write through the typed store (the seam has no partial update).
	o, err := s.GetObjective(ctx, id)
	if err != nil {
		fatal(err)
	}
	o.Enabled = true
	if err := s.PutObjective(ctx, o); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stdout, "enabled objective %q\n", id)
}
