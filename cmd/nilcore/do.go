package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"nilcore/internal/router"
)

// do.go — the "let the agent pick how to work" entry (Phase 16, Pillar 8 — UOK V2,
// docs/ROADMAP-KERNEL-V2.md). It completes the kernel's stated purpose ("the
// conversational router picks an ENVELOPE, not a machine"): instead of the human typing
// run / build / swarm, `nilcore do -goal "…"` routes the goal to the cheapest preset that
// fits (internal/router) and dispatches to that proven machine. It adds NO execution of
// its own — it only PICKS, then hands off to the existing, verifier-governed entrypoint,
// so every invariant the chosen machine upholds (I2 verify, I3 gate, I5 log) is unchanged.
//
// The router decision is heuristic today (router.Classify); the Oracle seam (passed nil
// here) is where a learned/model-backed router — informed by the experience/lessons/trust
// ledgers — plugs in later WITHOUT changing this dispatch, exactly as trust routing
// shipped deterministic-first.

func doMain(args []string) {
	fs := flag.NewFlagSet("do", flag.ExitOnError)
	goal := fs.String("goal", "", "the task, in plain language — the agent picks how to work")
	dir := fs.String("dir", ".", "git repo to work in (forwarded to whichever preset the goal routes to)")
	swarmPreset := fs.String("preset", "", "swarm preset to use if routed to swarm (empty = swarm's own default)")
	as := fs.String("as", "", "force a preset (run|build|swarm|decompose), bypassing the router")
	dryRun := fs.Bool("dry-run", false, "print the routing decision and exit without dispatching")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `nilcore do — one entry; the agent picks how to work.

It routes the goal to the cheapest preset that fits and dispatches to that machine:
  run        a single task (the default for ordinary work)
  build      a whole-project / scaffold goal, driven over -dir
  swarm      a breadth / parallel objective
  decompose  split into independent sub-goals, run each, integrate (force with -as decompose)

Usage:
  nilcore do -goal "<task>" [-dir ./repo] [-preset <swarm-preset>] [-as run|build|swarm|decompose] [-dry-run]

For flags beyond these, call the chosen command directly (e.g. nilcore build -new ./svc).
`)
	}
	_ = fs.Parse(args)

	if strings.TrimSpace(*goal) == "" {
		fmt.Fprintln(os.Stderr, "error: --goal is required\nrun 'nilcore do -h' for usage")
		os.Exit(2)
	}

	preset, by, err := resolvePreset(*as, *goal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	// The routing decision is the visible "agent picks how to work" moment. It goes to
	// stderr so it never pollutes a command's stdout.
	fmt.Fprintf(os.Stderr, "do: routing to %q (%s) — %s\n", preset, by, *goal)
	if *dryRun {
		return
	}

	switch preset {
	case router.Run:
		runMain(presetArgs(router.Run, *goal, *dir, ""))
	case router.Build:
		buildMain(presetArgs(router.Build, *goal, *dir, ""))
	case router.Swarm:
		swarmMain(presetArgs(router.Swarm, *goal, *dir, *swarmPreset))
	case router.Decompose:
		decomposeMain(presetArgs(router.Decompose, *goal, *dir, "")) // opt-in via -as decompose
	default:
		// Unreachable: resolvePreset only returns valid presets.
		fmt.Fprintf(os.Stderr, "error: unroutable preset %q\n", preset)
		os.Exit(2)
	}
}

// resolvePreset decides which preset to run goal as. A non-empty `as` forces that preset
// (provenance "forced"), failing closed on an unknown value; otherwise the router decides
// (heuristic today, Oracle seam reserved). It is pure so the decision is unit-tested
// without dispatching.
func resolvePreset(as, goal string) (router.Preset, string, error) {
	if strings.TrimSpace(as) != "" {
		p := router.Preset(strings.ToLower(strings.TrimSpace(as)))
		if !p.Valid() {
			return "", "", fmt.Errorf("unknown preset %q for -as (want run|build|swarm|decompose)", as)
		}
		return p, "forced", nil
	}
	// nil Oracle ⇒ the deterministic heuristic. The seam is where a learned router plugs in.
	p, by := router.Route(context.Background(), nil, goal, router.All())
	return p, by, nil
}

// presetArgs synthesizes the argument slice for the chosen preset's entrypoint. Every
// preset's entrypoint accepts -dir (swarm via registerCommon), so -dir is always
// forwarded — a `do -dir ./repo` that routes to swarm must still run against ./repo,
// never silently fall back to cwd. swarm additionally takes an optional -preset (empty
// ⇒ swarm's own default). It is pure so the synthesis is unit-tested. `do` forwards only
// the common flags — richer flags (greenfield -new, budgets, backends) belong to the
// explicit command.
func presetArgs(p router.Preset, goal, dir, swarmPreset string) []string {
	a := []string{"-goal", goal, "-dir", dir}
	if p == router.Swarm && strings.TrimSpace(swarmPreset) != "" {
		a = append(a, "-preset", swarmPreset)
	}
	return a
}
