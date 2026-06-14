// Command nilcore runs one coding task end to end: pick a backend, execute it
// inside a container sandbox against a working directory, then let the verifier
// decide whether it actually passed. This is the Phase 0 core — the smallest
// thing that proves the loop converges. The channel, memory, and routing layers
// grow around it without changing this contract.
//
// Example:
//
//	export ANTHROPIC_API_KEY=sk-...
//	nilcore -dir ./repo -goal "make the failing test in math_test.go pass" \
//	         -verify "go build ./... && go test ./..."
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

func main() {
	var (
		dir         = flag.String("dir", ".", "working directory (a git worktree in normal use)")
		goal        = flag.String("goal", "", "the coding task, in plain language")
		backendName = flag.String("backend", "native", "native | codex | claude-code")
		runtime     = flag.String("runtime", "podman", "container runtime: podman | docker")
		image       = flag.String("image", "docker.io/library/debian:stable-slim", "sandbox image")
		checkCmd    = flag.String("verify", "make verify", "command that returns 0 when the task is done")
		logPath     = flag.String("log", "nilcore.events.jsonl", "append-only event log path")
		maxSteps    = flag.Int("max-steps", 60, "tool-call budget for the native loop")
	)
	flag.Parse()

	if *goal == "" {
		fmt.Fprintln(os.Stderr, "error: --goal is required")
		os.Exit(2)
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		fatal(err)
	}

	log, err := eventlog.Open(*logPath)
	if err != nil {
		fatal(err)
	}
	defer log.Close()

	box := sandbox.NewContainer(*runtime, *image, absDir)
	v := verify.New(box, *checkCmd)

	be, err := pickBackend(*backendName, box, v, log, *maxSteps)
	if err != nil {
		fatal(err)
	}

	orch := &agent.Orchestrator{Backend: be, Verifier: v, Log: log}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	task := backend.Task{
		ID:   fmt.Sprintf("t-%d", time.Now().Unix()),
		Goal: *goal,
		Dir:  absDir,
	}

	out, err := orch.Execute(ctx, task)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("\nbackend:  %s\nverified: %v\nsummary:  %s\n", out.Backend, out.Verified, out.Summary)
	if !out.Verified {
		fmt.Printf("\nchecks did not pass:\n%s\n", out.Detail)
		os.Exit(1)
	}
}

func pickBackend(name string, box sandbox.Sandbox, v verify.Verifier, log *eventlog.Log, maxSteps int) (backend.CodingBackend, error) {
	switch name {
	case "native":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required for the native backend")
		}
		return &backend.Native{
			Model:    model.New(key, getenv("NILCORE_MODEL", "claude-sonnet-4-6")),
			Box:      box,
			Verifier: v,
			Log:      log,
			MaxSteps: maxSteps,
		}, nil
	case "codex":
		return &backend.Codex{}, nil
	case "claude-code":
		return &backend.ClaudeCode{}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want native | codex | claude-code)", name)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
