// Command nilcore is the entrypoint. It dispatches subcommands:
//
//	nilcore -goal "..." [-dir ./repo] ...     run one task to completion (default)
//	nilcore serve -channel telegram ...        listen on a chat channel and dispatch
//
// Each run happens in a disposable git worktree of -dir (which must be a git
// repo): a backend runs inside a container sandbox, then the verifier decides
// whether it passed. Secrets come from the environment only (invariant I3).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/channel"
	"nilcore/internal/channel/slack"
	"nilcore/internal/channel/telegram"
	"nilcore/internal/eventlog"
	"nilcore/internal/memory"
	"nilcore/internal/model"
	"nilcore/internal/onboard"
	"nilcore/internal/paths"
	"nilcore/internal/policy"
	"nilcore/internal/provider"
	"nilcore/internal/sandbox"
	"nilcore/internal/secrets"
	"nilcore/internal/server"
	"nilcore/internal/store"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "serve":
		serveMain(args[1:])
	case len(args) > 0 && args[0] == "init":
		initMain(args[1:])
	default:
		runMain(args)
	}
}

// initMain runs the onboarding wizard (or non-interactive provisioning).
func initMain(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	nonInteractive := fs.Bool("non-interactive", false, "assemble config from environment without prompting")
	configPath := fs.String("config", "", "config output path (default: <config-dir>/config.json)")
	_ = fs.Parse(args)

	store := secrets.Detect()
	var (
		cfg onboard.Config
		err error
	)
	if *nonInteractive {
		cfg, err = onboard.FromEnv(os.Getenv, store)
	} else {
		w := &onboard.Wizard{In: os.Stdin, Out: os.Stdout, Secrets: store}
		cfg, err = w.Run()
	}
	if err != nil {
		fatal(err)
	}

	path := *configPath
	if path == "" {
		dir, derr := paths.ConfigDir()
		if derr != nil {
			fatal(derr)
		}
		path = filepath.Join(dir, "config.json")
	}
	if err := cfg.Save(path); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote config to %s (secrets stored in the %s backend)\n", path, store.Name())
}

// commonFlags registers the flags shared by run and serve on fs.
type commonFlags struct {
	dir, backendName, runtime, image, checkCmd, logPath *string
	maxSteps                                            *int
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		dir:         fs.String("dir", ".", "git repository tasks run against (in a disposable worktree)"),
		backendName: fs.String("backend", "native", "native | codex | claude-code"),
		runtime:     fs.String("runtime", "podman", "container runtime: podman | docker"),
		image:       fs.String("image", "docker.io/library/debian:stable-slim", "sandbox image"),
		checkCmd:    fs.String("verify", "make verify", "command that returns 0 when the task is done"),
		logPath:     fs.String("log", "nilcore.events.jsonl", "append-only event log path"),
		maxSteps:    fs.Int("max-steps", 60, "tool-call budget for the native loop"),
	}
}

// runMain executes a single task and exits.
func runMain(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	goal := fs.String("goal", "", "the coding task, in plain language")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	if *goal == "" {
		fmt.Fprintln(os.Stderr, "error: --goal is required")
		os.Exit(2)
	}

	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()
	prov := resolveProvider(*c.backendName)
	mem := setupMemory(log)

	orch := &agent.Orchestrator{
		BaseRepo:  absDir,
		NewEnv:    envFactory(c, prov, log, mem, absDir),
		Log:       log,
		Router:    agent.SingleRouter{},
		Spawner:   agent.NoSpawner{},
		Approver:  policy.NewConsoleApprover(os.Stdin, os.Stdout),
		OnSuccess: memWriteBack(mem, absDir),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	out, err := orch.Execute(ctx, backend.Task{ID: fmt.Sprintf("t-%d", time.Now().Unix()), Goal: *goal})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("\nbackend:  %s\nverified: %v\nsummary:  %s\n", out.Backend, out.Verified, out.Summary)
	if !out.Verified {
		fmt.Printf("\nchecks did not pass:\n%s\n", out.Detail)
		os.Exit(1)
	}
}

// serveMain listens on a chat channel and dispatches tasks until interrupted.
func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	channelName := fs.String("channel", "telegram", "telegram | slack")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()
	prov := resolveProvider(*c.backendName)
	ch := buildChannel(*channelName)
	mem := setupMemory(log)
	newEnv := envFactory(c, prov, log, mem, absDir)

	run := func(ctx context.Context, t backend.Task, approver policy.Approver) (string, error) {
		orch := &agent.Orchestrator{
			BaseRepo:  absDir,
			NewEnv:    newEnv,
			Log:       log,
			Router:    agent.SingleRouter{},
			Spawner:   agent.NoSpawner{},
			Approver:  approver, // gate questions route back to this thread
			OnSuccess: memWriteBack(mem, absDir),
		}
		out, err := orch.Execute(ctx, t)
		if err != nil {
			return "", err
		}
		if !out.Verified {
			return "❌ checks did not pass:\n" + out.Detail, nil
		}
		return "✅ verified — " + out.Summary, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &server.Server{Channel: ch, Log: log, Run: run}
	fmt.Fprintf(os.Stderr, "nilcore serve: listening on the %s channel (Ctrl-C to stop)\n", *channelName)
	if err := srv.Serve(ctx); err != nil {
		fatal(err)
	}
}

// resolveProvider builds the model provider for the native backend and validates
// the backend name + required secret up front.
func resolveProvider(backendName string) model.Provider {
	switch backendName {
	case "native":
		p, err := provider.Resolve(getenv("NILCORE_MODEL", "claude-sonnet-4-6"))
		if err != nil {
			fatal(err)
		}
		return p
	case "codex", "claude-code":
		return nil
	default:
		fatal(fmt.Errorf("unknown backend %q (want native | codex | claude-code)", backendName))
		return nil
	}
}

// envFactory builds the per-worktree backend+verifier factory.
func envFactory(c commonFlags, prov model.Provider, log *eventlog.Log, mem *memory.Memory, project string) func(string) agent.Env {
	return func(dir string) agent.Env {
		box := sandbox.NewContainer(*c.runtime, *c.image, dir)
		v := verify.New(box, *c.checkCmd)
		be := buildBackend(*c.backendName, prov, box, v, log, *c.maxSteps, mem, project)
		return agent.Env{Backend: be, Verifier: v}
	}
}

func buildBackend(name string, prov model.Provider, box sandbox.Sandbox, v verify.Verifier, log *eventlog.Log, maxSteps int, mem *memory.Memory, project string) backend.CodingBackend {
	switch name {
	case "codex":
		// Key from the environment only (I3); injected into the container per run.
		return &backend.Codex{Box: box, Key: os.Getenv("CODEX_API_KEY"), Log: log}
	case "claude-code":
		return &backend.ClaudeCode{Box: box, Key: os.Getenv("ANTHROPIC_API_KEY"), Log: log}
	default: // native
		n := &backend.Native{
			Model:        prov,
			Box:          box,
			Verifier:     v,
			Log:          log,
			Tools:        tools.Default(),
			CommandGuard: policy.DefaultCommandPolicy().Check,
			MaxSteps:     maxSteps,
		}
		if mem != nil {
			n.MemoryContext = func(ctx context.Context, _ string) string {
				blk, _ := mem.Context(ctx, memory.ScopeProject, project, "", 10)
				return blk
			}
		}
		return n
	}
}

// setupMemory opens the persistent store (best-effort), wires it as a second
// backing for the event log, and returns the memory API (nil if unavailable).
func setupMemory(log *eventlog.Log) *memory.Memory {
	dir, err := paths.EnsureDir(paths.DataDir())
	if err != nil {
		return nil
	}
	s, err := store.Open(filepath.Join(dir, "nilcore.db"))
	if err != nil {
		return nil // memory is optional; never block a run on it
	}
	log.UseStore(s)
	return memory.New(s)
}

// memWriteBack persists a durable record after a verified task (P4-T05).
func memWriteBack(mem *memory.Memory, project string) func(context.Context, backend.Task, agent.Outcome) {
	if mem == nil {
		return nil
	}
	return func(ctx context.Context, t backend.Task, out agent.Outcome) {
		_, _ = mem.Remember(ctx, []memory.Record{{
			Scope: memory.ScopeProject, Project: project, Key: "task:" + t.ID, Value: out.Summary,
		}})
	}
}

func buildChannel(name string) channel.Channel {
	switch name {
	case "telegram":
		tok := os.Getenv("TELEGRAM_BOT_TOKEN")
		if tok == "" {
			fatal(fmt.Errorf("TELEGRAM_BOT_TOKEN is required for the telegram channel"))
		}
		return telegram.New(tok)
	case "slack":
		app, bot := os.Getenv("SLACK_APP_TOKEN"), os.Getenv("SLACK_BOT_TOKEN")
		if app == "" || bot == "" {
			fatal(fmt.Errorf("SLACK_APP_TOKEN and SLACK_BOT_TOKEN are required for the slack channel"))
		}
		return slack.New(app, bot)
	default:
		fatal(fmt.Errorf("unknown channel %q (want telegram | slack)", name))
		return nil
	}
}

func mustAbs(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		fatal(err)
	}
	return abs
}

func openLog(path string) *eventlog.Log {
	log, err := eventlog.Open(path)
	if err != nil {
		fatal(err)
	}
	return log
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
