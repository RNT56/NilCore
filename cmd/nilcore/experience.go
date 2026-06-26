package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"nilcore/internal/experience"
)

// experienceMain is the read-only operator view of the unified learned-state
// projection (Phase 16, EXP-T07): the verifier-judged per-backend standings and
// the outcome rollup, replayed from the append-only event log. Like `nilcore
// trust`, it FAILS CLOSED on a broken hash chain (prints the error, exits
// non-zero), so a tampered log never renders a clean scoreboard.
func experienceMain(args []string) {
	fs := flag.NewFlagSet("experience", flag.ExitOnError)
	logPath := fs.String("log", "nilcore.events.jsonl", "append-only event log path")
	format := fs.String("format", "text", "text | json")
	class := fs.String("class", "", "task-class filter (reserved; default = global)")
	_ = fs.Parse(args)

	x, err := experience.OverLog(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "experience: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	stands, _ := x.BackendStanding(ctx, *class)
	agg, _ := x.Outcomes(ctx, *class)

	if *format == "json" {
		b, _ := json.MarshalIndent(map[string]any{"standings": stands, "outcomes": agg}, "", "  ")
		fmt.Println(string(b))
		return
	}
	fmt.Printf("verifier-judged outcomes (class %q): %d races, %d passed, median cost $%.4f, median latency %.0fns\n",
		agg.Class, agg.Races, agg.Passes, agg.MedianCostUSD, agg.MedianLatency)
	if len(stands) == 0 {
		fmt.Println("(no backend standings yet — no race outcomes recorded)")
		return
	}
	fmt.Println("per-backend standing (verifier-judged):")
	for _, s := range stands {
		fmt.Printf("  %-14s races=%-4d wins=%-4d pass-rate=%.2f\n", s.Backend, s.Races, s.Wins, s.PassRate)
	}
}
