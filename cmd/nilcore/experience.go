package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"nilcore/internal/experience"
	"nilcore/internal/memory"
	"nilcore/internal/paths"
	"nilcore/internal/store"
)

// experienceMain is the read-only operator view of the unified learned-state
// projection (Phase 16, EXP-T07): the verifier-judged per-backend standings and the
// outcome rollup. By default it REPLAYS the append-only log (OverLog) — always correct,
// needs no store. With -warm it reads the store-backed projection (OverStore) instead —
// the warm read path the projector writes when NILCORE_EXPERIENCE is set, so a decision-
// maker (or this view) gets the standings without a full log replay. With -rebuild it
// re-derives that projection from the log and exits. Like `nilcore trust`, the replay
// path FAILS CLOSED on a broken hash chain.
func experienceMain(args []string) {
	fs := flag.NewFlagSet("experience", flag.ExitOnError)
	logPath := fs.String("log", "nilcore.events.jsonl", "append-only event log path")
	format := fs.String("format", "text", "text | json")
	class := fs.String("class", "", "task-class filter (reserved; default = global)")
	warm := fs.Bool("warm", false, "read the store-backed projection (OverStore) instead of replaying the log")
	rebuild := fs.Bool("rebuild", false, "re-derive the store-backed projection from the log, then exit")
	_ = fs.Parse(args)

	ctx := context.Background()

	// --rebuild: re-derive the projection tables from the authoritative log so a later
	// -warm read (or any consumer) reflects the full history. Idempotent.
	if *rebuild {
		s, err := openExperienceStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "experience: open store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()
		if err := experience.NewProjector(s).Rebuild(ctx, *logPath); err != nil {
			fmt.Fprintf(os.Stderr, "experience: rebuild: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("experience: projection rebuilt from the log")
		return
	}

	var reader experience.Reader
	if *warm {
		s, err := openExperienceStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "experience: open store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()
		reader = experience.OverStore(s, memory.New(s))
	} else {
		x, err := experience.OverLog(*logPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "experience: %v\n", err)
			os.Exit(1)
		}
		reader = x
	}

	stands, _ := reader.BackendStanding(ctx, *class)
	agg, _ := reader.Outcomes(ctx, *class)

	if *format == "json" {
		b, _ := json.MarshalIndent(map[string]any{"standings": stands, "outcomes": agg}, "", "  ")
		fmt.Println(string(b))
		return
	}
	src := "replayed from the log"
	if *warm {
		src = "warm projection (OverStore)"
	}
	fmt.Printf("verifier-judged outcomes (class %q, %s): %d races, %d passed, median cost $%.4f, median latency %.0fns\n",
		agg.Class, src, agg.Races, agg.Passes, agg.MedianCostUSD, agg.MedianLatency)
	if len(stands) == 0 {
		fmt.Println("(no backend standings yet — no race outcomes recorded)")
		return
	}
	fmt.Println("per-backend standing (verifier-judged):")
	for _, s := range stands {
		fmt.Printf("  %-14s races=%-4d wins=%-4d pass-rate=%.2f\n", s.Backend, s.Races, s.Wins, s.PassRate)
	}
}

// openExperienceStore opens the shared task store at the standard data-dir path (the
// same DB setupPersistence opens), so the -warm/-rebuild paths read/write the same
// projection the serve-time projector populates.
func openExperienceStore() (*store.Store, error) {
	dir, err := paths.EnsureDir(paths.DataDir())
	if err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, "nilcore.db"))
}
