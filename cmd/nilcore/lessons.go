package main

// lessons.go implements `nilcore lessons` — the read-only A8 lessons view (Phase
// 16, Pillar 3 / LRN-T03), the operator face of "learn from scars". It distills
// RECURRING verifier-failure PATTERNS from the append-only event log into structural
// memory records (verifier_id + fail_class + count — never raw failing output, I7)
// and prints them, most-recurrent first. It mirrors trust/experience's read-only
// discipline: a new subcommand off main's switch, no new event kinds (purely a
// reader — I5), default behaviour off the literal `lessons` first-arg so the rest of
// the CLI is byte-identical. lessons.DistillN is FAIL-CLOSED on a broken hash chain:
// a tampered log earns NO lessons, so the command prints the error and exits non-zero
// rather than distilling over forged evidence (I5).
//
// The same distiller is wired into the run path by wireLessons (setupPersistence,
// behind NILCORE_LESSONS), which folds the distilled scars into cross-project memory
// so the next same-class task surfaces them as context — closing the loop the CLI
// only inspects.

import (
	"context"
	"flag"
	"fmt"
	"os"

	"nilcore/internal/memory"
	"nilcore/internal/memory/lessons"
)

// lessonsMain is the `nilcore lessons` entrypoint. It distills the event log into
// recurring-failure lesson records and prints them. Read-only: it never writes the
// log or memory (the auto-fold into memory is the separate, opt-in run-path wiring).
// A broken chain (lessons.DistillN's fail-closed error) is printed and exits non-zero.
func lessonsMain(args []string) {
	fs := flag.NewFlagSet("lessons", flag.ExitOnError)
	logPath := fs.String("log", defaultLogPath, "append-only event log path")
	minRec := fs.Int("min", lessons.MinRecurrence, "minimum recurrences before a pattern counts as a scar")
	_ = fs.Parse(args)

	recs, err := lessons.DistillN(*logPath, *minRec)
	if err != nil {
		// Fail-closed: a broken chain yields no trustworthy lessons (I5).
		fatal(err)
	}
	if len(recs) == 0 {
		fmt.Fprintln(os.Stdout, "no recurring verifier-failure patterns yet — the verifier has not failed the same way twice.")
		return
	}
	fmt.Fprintf(os.Stdout, "recurring verifier-failure patterns (>= %d occurrences):\n\n", *minRec)
	for _, r := range recs {
		fmt.Fprintf(os.Stdout, "  • %s\n", r.Value)
	}
}

// wireLessons folds the distilled lessons into cross-project memory at run start so
// the next same-class task surfaces prior scars as context (LRN-T03 wiring). It is
// the closed-loop half of `nilcore lessons`. DEFAULT-OFF: with NILCORE_LESSONS unset
// it does nothing, so the run is byte-identical. It is best-effort — distilling reads
// the log READ-ONLY and fails closed on a broken chain (no lessons over forged
// evidence, I5); a distill or remember error is reported to stderr but never aborts
// the run (a learning aid must not break a working run). The records are structural
// only (verifier_id + fail_class + count — I7), and memory.Remember dedupes, so
// repeated runs do not pile up duplicates.
func wireLessons(logPath string, mem *memory.Memory) {
	if mem == nil || os.Getenv("NILCORE_LESSONS") == "" {
		return
	}
	recs, err := lessons.Distill(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: lessons distill skipped: %v\n", err)
		return
	}
	if len(recs) == 0 {
		return
	}
	if _, err := mem.Remember(context.Background(), recs); err != nil {
		fmt.Fprintf(os.Stderr, "nilcore: lessons remember skipped: %v\n", err)
	}
}
