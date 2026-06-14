// Command nilcore is the entrypoint. It dispatches subcommands:
//
//	nilcore -goal "..." [-dir ./repo] ...     run one task to completion (default)
//	nilcore serve -channel telegram ...        listen on a chat channel and dispatch
//
// Each run happens in a disposable git worktree of -dir (which must be a git
// repo): a backend runs inside a container sandbox, then the verifier decides
// whether it passed. Credentials resolve environment-first, then the SecretStore
// recorded by `nilcore init` — never from the model (invariant I3).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	dir, backendName, runtime, image, checkCmd, logPath, config *string
	maxSteps                                                    *int
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		dir:         fs.String("dir", ".", "git repository tasks run against (in a disposable worktree)"),
		backendName: fs.String("backend", "native", "native | codex | claude-code"),
		runtime:     fs.String("runtime", "podman", "container runtime: podman | docker"),
		image:       fs.String("image", onboard.DefaultImage, "sandbox image"),
		checkCmd:    fs.String("verify", "make verify", "command that returns 0 when the task is done"),
		logPath:     fs.String("log", "nilcore.events.jsonl", "append-only event log path"),
		config:      fs.String("config", "", "config file from `nilcore init` (default: <config-dir>/config.json)"),
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

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))

	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()
	prov := resolveProvider(*c.backendName, b)
	mem, cp := setupPersistence(log)

	orch := &agent.Orchestrator{
		BaseRepo:   absDir,
		NewEnv:     envFactory(c, prov, b.cred, log, mem, absDir),
		Log:        log,
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Approver:   policy.NewConsoleApprover(os.Stdin, os.Stdout),
		OnSuccess:  memWriteBack(mem, absDir),
		Checkpoint: cp,
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
	channelName := fs.String("channel", "", "telegram | slack (default: config, else telegram)")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))

	absDir := mustAbs(*c.dir)
	log := openLog(*c.logPath)
	defer log.Close()
	prov := resolveProvider(*c.backendName, b)
	allow := principalAllowlist(b.cfg)
	if len(allow) == 0 {
		fatal(fmt.Errorf("serve refuses to start with an empty principal allowlist (no ambient authority): " +
			"set NILCORE_ALLOWLIST to a comma-separated list of permitted channel user ids, " +
			"or add \"allow\" to the channel section of config.json"))
	}
	ch := buildChannel(channelSpec(*channelName, b.cfg), b.cred, allow, log)
	mem, cp := setupPersistence(log)
	newEnv := envFactory(c, prov, b.cred, log, mem, absDir)

	run := func(ctx context.Context, t backend.Task, approver policy.Approver) (string, error) {
		orch := &agent.Orchestrator{
			BaseRepo:   absDir,
			NewEnv:     newEnv,
			Log:        log,
			Router:     agent.SingleRouter{},
			Spawner:    agent.NoSpawner{},
			Approver:   approver, // gate questions route back to this thread
			OnSuccess:  memWriteBack(mem, absDir),
			Checkpoint: cp,
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
	if cp != nil {
		_ = cp.Interrupt(context.Background()) // SIGTERM: checkpoint in-flight before exit (P6-T03)
	}
}

// resolveProvider builds the model provider for the native backend and validates
// the backend name + required secret up front. The model spec is NILCORE_MODEL,
// else the configured executor, else the built-in default; the key resolves
// environment-first then SecretStore via b.cred.
func resolveProvider(backendName string, b boot) model.Provider {
	switch backendName {
	case "native":
		p, err := provider.ResolveWith(modelSpec(os.Getenv("NILCORE_MODEL"), b.cfg.Executor), b.cred)
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
func envFactory(c commonFlags, prov model.Provider, cred func(string) string, log *eventlog.Log, mem *memory.Memory, project string) func(string) agent.Env {
	return func(dir string) agent.Env {
		box := sandbox.NewContainer(*c.runtime, *c.image, dir)
		v := verify.New(box, *c.checkCmd)
		be := buildBackend(*c.backendName, prov, cred, box, v, log, *c.maxSteps, mem, project)
		return agent.Env{Backend: be, Verifier: v}
	}
}

func buildBackend(name string, prov model.Provider, cred func(string) string, box sandbox.Sandbox, v verify.Verifier, log *eventlog.Log, maxSteps int, mem *memory.Memory, project string) backend.CodingBackend {
	switch name {
	case "codex":
		// Key resolved env-first then SecretStore (I3); injected into the container per run.
		return &backend.Codex{Box: box, Key: cred("CODEX_API_KEY"), Log: log}
	case "claude-code":
		return &backend.ClaudeCode{Box: box, Key: cred("ANTHROPIC_API_KEY"), Log: log}
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

// setupPersistence opens the persistent store (best-effort), wires it as a second
// backing for the event log, and returns the memory API and the task checkpointer
// (both nil if the store is unavailable — persistence is optional, never blocking).
func setupPersistence(log *eventlog.Log) (*memory.Memory, *agent.Checkpoint) {
	dir, err := paths.EnsureDir(paths.DataDir())
	if err != nil {
		return nil, nil
	}
	s, err := store.Open(filepath.Join(dir, "nilcore.db"))
	if err != nil {
		return nil, nil
	}
	log.UseStore(s)
	return memory.New(s), agent.NewCheckpoint(s)
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

// authChannel is the subset a transport must expose so serve can pin gate-answer
// authorization into it (the clicker's identity is only visible inside the
// transport, so the frozen Channel interface cannot carry it).
type authChannel interface {
	channel.Channel
	SetAuthorizer(allow func(string) bool, log *eventlog.Log)
}

// buildChannel constructs the chat transport and wraps it in deny-all-by-default
// authorization: only principals in allow may command the agent (Receive) or
// answer an irreversible-action gate (Ask). Wiring both sides closes audit H2/H3
// — a freshly-deployed bot is inert to whoever merely finds it.
func buildChannel(name string, cred func(string) string, allow []string, log *eventlog.Log) channel.Channel {
	var bot authChannel
	switch name {
	case "telegram":
		tok := cred("TELEGRAM_BOT_TOKEN")
		if tok == "" {
			fatal(fmt.Errorf("TELEGRAM_BOT_TOKEN is required for the telegram channel"))
		}
		bot = telegram.New(tok)
	case "slack":
		app, bt := cred("SLACK_APP_TOKEN"), cred("SLACK_BOT_TOKEN")
		if app == "" || bt == "" {
			fatal(fmt.Errorf("SLACK_APP_TOKEN and SLACK_BOT_TOKEN are required for the slack channel"))
		}
		bot = slack.New(app, bt)
	default:
		fatal(fmt.Errorf("unknown channel %q (want telegram | slack)", name))
		return nil
	}
	auth := channel.NewAuthorized(bot, allow, log) // filters inbound commands
	bot.SetAuthorizer(auth.Permit, log)            // and gate-button answers
	return auth
}

// principalAllowlist is the set of principals permitted to command the agent and
// answer gates in serve mode: NILCORE_ALLOWLIST (comma-separated channel user
// ids) merged with any allowlist recorded in config.json. It is empty by default;
// serve refuses to start until it is set, so the bot has no ambient authority
// (invariant §2.3) — anyone who finds it cannot drive it.
func principalAllowlist(cfg onboard.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, p := range strings.Split(os.Getenv("NILCORE_ALLOWLIST"), ",") {
		add(p)
	}
	for _, p := range cfg.Channel.Allow {
		add(p)
	}
	return out
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

// boot is what the run/serve paths derive from config.json + the SecretStore at
// startup: the parsed configuration and a credential resolver. The resolver
// prefers the process environment (I3's primary path) and falls back to the
// SecretStore via the secret references in config.json — so keys captured by
// `nilcore init` feed a run without re-exporting them. Best-effort: with no config
// and no keychain it degrades to pure environment lookup (the prior behavior).
type boot struct {
	cfg  onboard.Config
	cred func(string) string
}

func loadBoot(configPath string) boot {
	cfg := loadConfig(configPath)
	return boot{cfg: cfg, cred: newCredResolver(cfg, secrets.Detect(), os.Getenv)}
}

// loadConfig reads config.json (from configPath or the default location). A
// missing or unreadable config is not an error — it yields the zero Config, and
// the run falls back to the environment + built-in defaults.
func loadConfig(configPath string) onboard.Config {
	path := configPath
	if path == "" {
		dir, err := paths.ConfigDir()
		if err != nil {
			return onboard.Config{}
		}
		path = filepath.Join(dir, "config.json")
	}
	cfg, err := onboard.Load(path)
	if err != nil {
		return onboard.Config{}
	}
	return cfg
}

// newCredResolver returns an env-name → value lookup: the process environment
// first, then the SecretStore entry named by config.json for that variable. It
// never logs a value — an unresolved key returns "", and the caller reports the
// specific missing-credential error.
func newCredResolver(cfg onboard.Config, store secrets.SecretStore, getenv func(string) string) func(string) string {
	refByEnv := secretRefsByEnv(cfg)
	return func(env string) string {
		if v := getenv(env); v != "" {
			return v
		}
		if ref, ok := refByEnv[env]; ok {
			if v, err := store.Get(ref); err == nil {
				return v
			}
		}
		return ""
	}
}

// secretRefsByEnv maps each credential's environment-variable name to the
// SecretStore reference recorded in config.json. CODEX_API_KEY is not captured by
// the wizard, so it has no reference and stays environment-only.
func secretRefsByEnv(cfg onboard.Config) map[string]string {
	m := map[string]string{}
	for _, p := range cfg.Providers {
		if env := providerEnv(p.Name); env != "" {
			m[env] = p.KeyRef
		}
	}
	switch cfg.Channel.Type {
	case "telegram":
		if len(cfg.Channel.TokenRefs) > 0 {
			m["TELEGRAM_BOT_TOKEN"] = cfg.Channel.TokenRefs[0]
		}
	case "slack":
		if len(cfg.Channel.TokenRefs) > 0 {
			m["SLACK_APP_TOKEN"] = cfg.Channel.TokenRefs[0]
		}
		if len(cfg.Channel.TokenRefs) > 1 {
			m["SLACK_BOT_TOKEN"] = cfg.Channel.TokenRefs[1]
		}
	}
	return m
}

func providerEnv(name string) string {
	switch name {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	default:
		return ""
	}
}

// modelSpec picks the role→provider:model spec: NILCORE_MODEL wins, then the
// configured executor, then the built-in default.
func modelSpec(envSpec, cfgExecutor string) string {
	if envSpec != "" {
		return envSpec
	}
	if cfgExecutor != "" {
		return cfgExecutor
	}
	return "claude-sonnet-4-6"
}

// channelSpec picks the chat channel: the -channel flag wins, then a configured
// channel (other than "none"), then telegram.
func channelSpec(flagVal string, cfg onboard.Config) string {
	if flagVal != "" {
		return flagVal
	}
	if t := cfg.Channel.Type; t != "" && t != "none" {
		return t
	}
	return "telegram"
}

// applyConfigDefaults lets config.json supply runtime/image when the operator did
// not pass the corresponding flag. Explicit flags always win; built-in defaults
// fill the rest.
func applyConfigDefaults(c commonFlags, cfg onboard.Config, set map[string]bool) {
	if !set["runtime"] && cfg.Runtime != "" {
		*c.runtime = cfg.Runtime
	}
	if !set["image"] && cfg.Image != "" {
		*c.image = cfg.Image
	}
}

// flagsSet returns the names of flags explicitly provided on the command line, so
// config defaults apply only where the operator was silent.
func flagsSet(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
