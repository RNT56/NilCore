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
	flow := &selfimprove.Flow{
		Scope: selfimprove.DefaultScope(),
		Run: func(ctx context.Context, g string) (bool, error) {
			out, err := runViaKernel(ctx, orch, backend.Task{ID: fmt.Sprintf("self-%d", time.Now().Unix()), Goal: g})
			if err != nil {
				return false, err
			}
			return out.Verified, nil
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
