package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"nilcore/internal/inspect"
)

// inspectMain implements `nilcore inspect` — read-only operator observability over
// the append-only event log (P6-T07). With no subcommand it replays the log and
// prints a Summary (total events, counts by kind, the distinct tasks seen). The
// `health` subcommand is a readiness probe: it exits 0 when the log is readable and
// its hash chain verifies, 1 otherwise — usable as a liveness gate for `serve`.
// inspect never mutates the log; integrity is delegated to eventlog.Verify, so the
// hash chain has one authority.
func inspectMain(args []string) {
	health := false
	if len(args) > 0 && args[0] == "health" {
		health, args = true, args[1:]
	}
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	logPath := fs.String("log", "nilcore.events.jsonl", "append-only event log path")
	_ = fs.Parse(args)

	if health {
		if err := inspect.Health(*logPath); err != nil {
			fmt.Fprintf(os.Stderr, "not ready: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("ready — event log is readable and the hash chain verifies")
		return
	}

	sum, err := inspect.Replay(*logPath)
	if err != nil {
		fatal(err)
	}
	fmt.Print(renderInspect(*logPath, sum))
}

// renderInspect formats a Summary as a compact operator report. Pure (no I/O) so
// it is unit-testable without capturing stdout.
func renderInspect(path string, s inspect.Summary) string {
	out := fmt.Sprintf("event log: %s\n  %d event(s) across %d task(s)\n", path, s.Total, len(s.Tasks))
	if len(s.ByKind) > 0 {
		kinds := make([]string, 0, len(s.ByKind))
		for k := range s.ByKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		out += "  by kind:\n"
		for _, k := range kinds {
			out += fmt.Sprintf("    %-22s %d\n", k, s.ByKind[k])
		}
	}
	if len(s.Tasks) > 0 {
		out += "  tasks:\n"
		for _, t := range s.Tasks {
			out += fmt.Sprintf("    %s\n", t)
		}
	}
	out += "  chain: verified ✓\n"
	return out
}
