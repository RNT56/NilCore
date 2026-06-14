// Command nilcore runs one coding task end to end: create a fresh git worktree
// of the target repo, run a backend inside a container sandbox against it, then
// let the verifier decide whether it actually passed. The channel, memory, and
// routing layers grow around this without changing the backend contract.
//
// Example:
//
//	export ANTHROPIC_API_KEY=sk-...
//	nilcore -dir ./repo -goal "make the failing test in math_test.go pass" \
//	         -verify "go build ./... && go test ./..."
//
// -dir must be a git repository; each run happens in a disposable worktree of it.
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
	"nilcore/internal/policy"
	"nilcore/internal/provider"
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

	// Resolve the model provider up front (native backend) so a missing key
	// surfaces before any worktree is created. Model selection is
	// "provider:model" via NILCORE_MODEL; a bare model defaults to Anthropic.
	var prov model.Provider
	switch *backendName {
	case "native":
		p, err := provider.Resolve(getenv("NILCORE_MODEL", "claude-sonnet-4-6"))
		if err != nil {
			fatal(err)
		}
		prov = p
	case "codex", "claude-code":
		// No up-front secret needed in Phase 1 (delegated CLIs authenticate themselves).
	default:
		fatal(fmt.Errorf("unknown backend %q (want native | codex | claude-code)", *backendName))
	}

	// NewEnv builds a sandbox + verifier + backend pointed at a given worktree;
	// the orchestrator calls it once per task.
	newEnv := func(dir string) agent.Env {
		box := sandbox.NewContainer(*runtime, *image, dir)
		v := verify.New(box, *checkCmd)
		be := buildBackend(*backendName, prov, box, v, log, *maxSteps)
		return agent.Env{Backend: be, Verifier: v}
	}

	orch := &agent.Orchestrator{
		BaseRepo: absDir,
		NewEnv:   newEnv,
		Log:      log,
		Router:   agent.SingleRouter{},
		Spawner:  agent.NoSpawner{},
		Approver: policy.NewConsoleApprover(os.Stdin, os.Stdout),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	// Dir is assigned by the orchestrator from the worktree it creates.
	task := backend.Task{
		ID:   fmt.Sprintf("t-%d", time.Now().Unix()),
		Goal: *goal,
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

// buildBackend constructs the chosen backend for a worktree. The backend name is
// validated up front in main, so this does not return an error.
func buildBackend(name string, prov model.Provider, box sandbox.Sandbox, v verify.Verifier, log *eventlog.Log, maxSteps int) backend.CodingBackend {
	switch name {
	case "codex":
		return &backend.Codex{}
	case "claude-code":
		return &backend.ClaudeCode{}
	default: // native
		return &backend.Native{
			Model:    prov,
			Box:      box,
			Verifier: v,
			Log:      log,
			MaxSteps: maxSteps,
		}
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
