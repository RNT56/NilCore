package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/policy"
	"nilcore/internal/selfimprove"
)

// proposeEditMain implements `nilcore propose-edit` — the gated self-edit flow
// (P5-T02). It runs a proposal through the pipeline: a SCOPE CHECK (only the
// agent's own prompts/skills/tools may be touched; the core, the invariants, and
// the contract files are denied outright), then the goal runs as a normal VERIFIED
// task, and a HUMAN GATE is required before it is accepted. The verifier and the
// gate are never bypassed (I2/I3); every step is audited. This is the deliberate
// entry point for the agent improving its own capability — under the same rails as
// any other change.
func proposeEditMain(args []string) {
	fs := flag.NewFlagSet("propose-edit", flag.ExitOnError)
	reason := fs.String("reason", "", "why this self-edit (recurring failure, missing tool, …)")
	goal := fs.String("goal", "", "the edit task, in plain language")
	pathsCSV := fs.String("paths", "", "comma-separated repo-relative files the edit may touch")
	c := registerCommon(fs)
	_ = fs.Parse(args)
	if *goal == "" || *pathsCSV == "" {
		fmt.Fprintln(os.Stderr, "error: --goal and --paths are required\n"+
			"  example: nilcore propose-edit -goal \"add a deploy skill\" -paths skills/deploy/SKILL.md -reason \"repeated manual step\"")
		os.Exit(2)
	}

	var paths []string
	for _, p := range strings.Split(*pathsCSV, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	proposal := selfimprove.Proposal{Reason: *reason, Paths: paths, Goal: *goal}

	// Fail fast on an out-of-scope proposal BEFORE building the orchestrator (which
	// needs a provider key): a denied self-edit never warrants spinning anything up.
	if ok, why := selfimprove.DefaultScope().Check(proposal); !ok {
		fmt.Fprintf(os.Stderr, "self-edit rejected: %s\n", why)
		os.Exit(1)
	}

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))
	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()

	orch := buildRunOrchestrator(c, b, log, absDir, mintBlastBudget(*c.blastRadius, log))
	// KeepBranch preserves the run's verified worktree branch so the scope screen below
	// can diff exactly what the run wrote (execution-time path enforcement, not just the
	// pre-declaration scope check): the free-text Goal could otherwise steer the model to
	// edit a file the proposal never declared, and the diff catches that before the gate.
	orch.KeepBranch = true
	var runBranch string // the verified branch the last Run left behind (for the diff)
	flow := &selfimprove.Flow{
		Scope: selfimprove.DefaultScope(),
		Run: func(ctx context.Context, g string) (bool, error) {
			out, err := runViaKernel(ctx, orch, backend.Task{ID: fmt.Sprintf("self-%d", time.Now().Unix()), Goal: g})
			if err != nil {
				return false, err
			}
			runBranch = out.Branch
			return out.Verified, nil
		},
		// Changed reports what the verified run actually modified, by diffing the kept
		// branch against the base repo's HEAD. Propose fail-closes on ANY path outside the
		// declared, in-scope allow-list. If the branch was not preserved (older path), it
		// returns an error so Propose refuses to gate an unverifiable run (fail-closed).
		Changed: func(ctx context.Context) ([]string, error) {
			return selfEditChangedPaths(ctx, absDir, runBranch)
		},
		Gate: policy.NewConsoleApprover(os.Stdin, os.Stdout).Approve,
		Log:  log,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	merged, err := flow.Propose(ctx, proposal)
	if err != nil {
		fatal(err)
	}
	if !merged {
		fmt.Println("self-edit not applied (rejected by scope, unverified, or gate denied).")
		os.Exit(1)
	}
	fmt.Println("self-edit accepted, verified, and approved.")
}

// selfEditChangedPaths reports the repo-relative files a verified self-edit run
// actually modified, by diffing the run's kept branch against the base repo HEAD in
// dir. It is the EXECUTION-time scope evidence selfimprove.Flow.Changed screens: a
// run that (steered by the free-text Goal) touched a file the proposal never
// declared — including the verifier of record — is caught here and never gated.
//
// Fail-closed: an empty branch (KeepBranch did not preserve one) or a git fault
// returns an error, so Propose refuses to gate a run whose footprint it cannot
// prove. Uses the SAME hardened git clamp as the rest of the cmd layer (I4).
func selfEditChangedPaths(ctx context.Context, dir, branch string) ([]string, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("self-edit run left no verified branch to scope-check (KeepBranch not honored)")
	}
	out, err := chatGit(ctx, dir, "diff", "--name-only", "HEAD.."+branch)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}
