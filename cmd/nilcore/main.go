// Command nilcore is the entrypoint. It dispatches subcommands:
//
//	nilcore init                          guided setup (keys, runtime, backend, channel, allowlist)
//	nilcore -goal "..." [-dir ./repo] ... run one task to completion (default)
//	nilcore build -goal "..." -new ./svc  drive a whole project to a verifier-green tree (multi-agent)
//	nilcore serve -channel telegram ...   listen on a chat channel and dispatch
//	nilcore doctor                        check whether this host is ready to run/serve
//	nilcore config show                   print the active configuration (secret-free)
//	nilcore secret set <name>             store/rotate a single secret
//	nilcore version | help                build version | usage banner
//
// Each run happens in a disposable git worktree of -dir (which must be a git
// repo): a backend runs inside a container sandbox, then the verifier decides
// whether it passed. Credentials resolve environment-first, then the SecretStore
// recorded by `nilcore init` — never from the model (invariant I3).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"nilcore/internal/advisor"
	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/budget"
	"nilcore/internal/channel"
	"nilcore/internal/channel/slack"
	"nilcore/internal/channel/telegram"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/maint"
	"nilcore/internal/memory"
	"nilcore/internal/meter"
	"nilcore/internal/model"
	"nilcore/internal/onboard"
	"nilcore/internal/paths"
	"nilcore/internal/policy"
	"nilcore/internal/provider"
	"nilcore/internal/sandbox"
	"nilcore/internal/secrets"
	"nilcore/internal/server"
	"nilcore/internal/session"
	"nilcore/internal/steering"
	"nilcore/internal/store"
	"nilcore/internal/summarize"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
	"nilcore/internal/worktree"
)

// version is the build version, overridable at release time via
// -ldflags "-X main.version=<tag>". It falls back to the VCS revision.
var version = "dev"

func main() {
	// If this process is a re-exec'd namespace-sandbox child, apply confinement
	// and exec the command now — this never returns. A no-op (one getenv) for
	// every normal invocation and on every non-Linux host.
	sandbox.MaybeRunInit()

	args := os.Args[1:]
	if len(args) == 0 {
		// The conversational front door is the natural default: bare `nilcore`
		// launches the interactive chat REPL (docs/CONVERSATIONAL.md §7). The
		// flag-prefixed `nilcore -goal …` and the explicit subcommands below keep
		// their existing behavior unchanged.
		chatMain(nil)
		return
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
	case "-v", "--version", "version":
		fmt.Println(versionString())
	case "chat":
		chatMain(args[1:])
	case "tui":
		tuiMain(args[1:])
	case "serve":
		serveMain(args[1:])
	case "build":
		buildMain(args[1:])
	case "init":
		initMain(args[1:])
	case "doctor":
		doctorMain(args[1:])
	case "config":
		configMain(args[1:])
	case "secret":
		secretMain(args[1:])
	case "inspect":
		inspectMain(args[1:])
	case "mcp-call":
		mcpCallMain(args[1:])
	case "propose-edit":
		proposeEditMain(args[1:])
	case "watch":
		watchMain(args[1:])
	case "schedule":
		scheduleMain(args[1:])
	case "registry":
		registryMain(args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			runMain(args) // documented `nilcore -goal ...` default
			return
		}
		fmt.Fprintf(os.Stderr, "error: unknown command %q\nrun 'nilcore help' for usage\n", args[0])
		os.Exit(2)
	}
}

// usageText is the top-level help: what NilCore is, then the command list, then
// the first-time on-ramp. Hand-written so the front door reads like a product,
// not a flag dump.
const usageText = `NilCore — a tiny, robust coding agent. The harness is small; the model is the engine.

Usage:
  nilcore                               start the interactive chat front door (same as 'nilcore chat')
  nilcore chat [-dir ./repo]            talk to the agent: it picks the machine and works while you type
  nilcore init                          guided setup: keys, runtime, backend, channel, allowlist
  nilcore -goal "<task>" [-dir ./repo]  run one task to completion in a disposable worktree
  nilcore build -goal "<project>" -new ./svc   drive a whole project to a verifier-green tree (multi-agent)
  nilcore serve -channel telegram       listen on a chat channel and dispatch tasks
  nilcore watch [-signals ./signals]    self-start tasks from dropped signal files (reversible auto, else gated)
  nilcore propose-edit -goal "..." -paths ...  gated self-edit of the agent's own prompts/skills/tools
  nilcore doctor                        check whether this host is ready to run/serve
  nilcore config show                   print the active configuration (secret-free)
  nilcore secret set <name>             store or rotate a single secret in the secret store
  nilcore inspect [health]              replay the event log (summary), or probe its health (exit 0/1)
  nilcore mcp-call <server> <tool> ...  invoke a configured MCP tool (the runtime bridge for generated wrappers)
  nilcore version                       print the build version

Run 'nilcore <command> -h' for a command's flags.
First time? Start with: nilcore init
`

func usage(w io.Writer) { fmt.Fprint(w, usageText) }

// versionString reports the build version: the ldflags-stamped tag, or the VCS
// revision recorded in the build info when running an un-stamped binary.
func versionString() string {
	if version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					rev := s.Value
					if len(rev) > 12 {
						rev = rev[:12]
					}
					return "nilcore dev (" + rev + ")"
				}
			}
		}
	}
	return "nilcore " + version
}

// initMain runs the onboarding wizard (or non-interactive provisioning).
func initMain(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	nonInteractive := fs.Bool("non-interactive", false, "assemble config from environment without prompting")
	allowEmpty := fs.Bool("allow-empty", false, "write the config even with no captured provider key (env-only setup)")
	vault := fs.String("vault", "key-file", "secret vault master key (headless, no keychain): key-file | passphrase")
	configPath := fs.String("config", "", "config output path (default: <config-dir>/config.json)")
	_ = fs.Parse(args)

	if *vault != "key-file" && *vault != "passphrase" {
		fatal(fmt.Errorf("unknown -vault %q (want key-file | passphrase)", *vault))
	}

	secretDir, derr := paths.ConfigDir()
	if derr != nil {
		fatal(derr)
	}
	path := *configPath
	if path == "" {
		path = filepath.Join(secretDir, "config.json")
	}

	// Refuse to mix master-key modes on one vault: re-sealing existing entries
	// under a different key would leave them undecryptable.
	if *vault == "passphrase" && !passphraseInUse(secretDir) {
		if _, err := os.Stat(filepath.Join(secretDir, "secrets.key")); err == nil {
			fatal(fmt.Errorf("a key-file vault already exists at %s; remove secrets.key and secrets.vault to switch to a passphrase vault", secretDir))
		}
	}
	if *vault == "key-file" && passphraseInUse(secretDir) {
		fatal(fmt.Errorf("a passphrase vault already exists at %s; pass -vault passphrase, or remove secrets.salt and secrets.vault to switch to a key-file vault", secretDir))
	}

	store := writeStore(secretDir, *vault == "passphrase", !*nonInteractive)
	if store.Name() == "env" {
		if *vault == "passphrase" || passphraseInUse(secretDir) {
			fatal(fmt.Errorf("passphrase vault selected but no passphrase available — set NILCORE_VAULT_PASSPHRASE, or run `nilcore init` interactively"))
		}
		fatal(fmt.Errorf("no writable secret backend: no OS keychain found and the encrypted vault under the " +
			"config dir could not be created — fix the config-dir permissions, or provide a keychain"))
	}

	var (
		cfg onboard.Config
		err error
	)
	if *nonInteractive {
		cfg, err = onboard.FromEnv(os.Getenv, store)
	} else {
		w := &onboard.Wizard{In: os.Stdin, Out: os.Stdout, Secrets: store, ConfigPath: path}
		cfg, err = w.Run()
	}
	if errors.Is(err, onboard.ErrAborted) {
		fmt.Fprintf(os.Stderr, "aborted — config not written (any keys you entered were already saved to "+
			"the %s backend; re-run `nilcore init` to finish)\n", store.Name())
		return
	}
	if err != nil {
		fatal(err)
	}

	if len(cfg.Providers) == 0 && !*allowEmpty {
		fatal(fmt.Errorf("no provider key was captured, so this config cannot run a task; " +
			"re-run `nilcore init` (or pass -allow-empty to write an env-only config)"))
	}

	if err := cfg.Save(path); err != nil {
		fmt.Fprintf(os.Stderr, "warning: secrets were stored in the %s backend but the config could not be "+
			"written; re-running `nilcore init` will reuse them\n", store.Name())
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote config to %s (secrets stored in the %s backend)\n", path, store.Name())
	printNextSteps(os.Stderr, cfg)
	if passphraseInUse(secretDir) {
		fmt.Fprintln(os.Stderr, "  vault is passphrase-sealed: export NILCORE_VAULT_PASSPHRASE before run/serve/doctor")
	}
}

// printNextSteps closes onboarding with a concrete on-ramp instead of a flat
// confirmation — including the serve allowlist reminder when a channel was set,
// so the operator is never led into serve's empty-allowlist refusal blind.
func printNextSteps(w io.Writer, cfg onboard.Config) {
	fmt.Fprintln(w, "\nYou're set. Try:")
	fmt.Fprintln(w, `  nilcore -dir ./repo -goal "fix the failing test"`)
	if cfg.Channel.Type == "telegram" || cfg.Channel.Type == "slack" {
		if len(cfg.Channel.Allow) == 0 {
			fmt.Fprintf(w, "  set an allowlist before serving: export NILCORE_ALLOWLIST=<%s-user-id>\n", cfg.Channel.Type)
		}
		fmt.Fprintf(w, "  nilcore serve -channel %s\n", cfg.Channel.Type)
	}
	fmt.Fprintln(w, "  nilcore doctor   # re-check readiness anytime")
}

// doctorMain reports whether this host can actually run (and serve), reusing the
// config's Readiness plus a live credential-resolution check. Exits non-zero when
// not run-ready, so it is usable as a scripted health gate.
func doctorMain(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", "", "config file from `nilcore init` (default: <config-dir>/config.json)")
	check := fs.Bool("check", false, "make a minimal live API call per model to verify the keys actually authenticate (network)")
	_ = fs.Parse(args)

	b := loadBoot(*configPath)
	var checker func(string) error
	if *check {
		checker = liveChecker(b.cred)
	}
	report, ready := diagnose(b.cfg, b.cred, checker)
	fmt.Print(report)
	fmt.Print(sandboxReport(b.cfg.Runtime))
	if dir, derr := paths.ConfigDir(); derr == nil && passphraseInUse(dir) && os.Getenv("NILCORE_VAULT_PASSPHRASE") == "" {
		fmt.Println("\nnote: this host uses a passphrase-sealed vault but NILCORE_VAULT_PASSPHRASE is not set — stored keys will not resolve.")
	}
	if !ready {
		os.Exit(1)
	}
}

// diagnose renders the doctor report and reports run-readiness. It is pure over
// (config, credential resolver), so it is testable without touching the host; the
// optional check verifies a model spec authenticates with a live call (nil skips).
func diagnose(cfg onboard.Config, cred func(string) string, check func(string) error) (string, bool) {
	var b strings.Builder
	ok := func(c bool) string {
		if c {
			return "✓"
		}
		return "✗"
	}
	b.WriteString("Configuration:\n")
	b.WriteString(cfg.Readiness())

	b.WriteString("\nCredentials (environment or stored):\n")
	anyResolved := false
	if len(cfg.Providers) == 0 {
		b.WriteString("  ✗ no providers configured — run `nilcore init`\n")
	}
	for _, p := range cfg.Providers {
		env := providerEnv(p.Name)
		resolved := env != "" && cred(env) != ""
		anyResolved = anyResolved || resolved
		fmt.Fprintf(&b, "  %s %s key resolves (%s)\n", ok(resolved), p.Name, env)
	}
	// Run-readiness keys on the *configured backend's* credential, not merely on
	// some provider resolving — so `nilcore doctor`'s exit code (a scripted gate)
	// matches what the chosen backend actually needs.
	ready := anyResolved
	switch cfg.Backend {
	case "codex", "claude-code":
		env := backendKeyEnv(cfg)
		ready = cred(env) != ""
		fmt.Fprintf(&b, "  %s %s backend key resolves (%s)\n", ok(ready), cfg.Backend, env)
	default: // native
		if cfg.Executor != "" {
			ready = cred(providerEnv(vendorOf(cfg.Executor))) != ""
		}
	}

	for _, env := range channelEnvs(cfg.Channel.Type) {
		fmt.Fprintf(&b, "  %s %s resolves\n", ok(cred(env) != ""), env)
	}
	if cfg.Channel.Type == "telegram" || cfg.Channel.Type == "slack" {
		allow := principalAllowlist(cfg)
		fmt.Fprintf(&b, "  %s serve allowlist resolves (%d) — required to serve\n", ok(len(allow) > 0), len(allow))
	}
	// The web-search key (brave) resolves like any credential — report it so doctor
	// is honest about whether the keyed search backend will actually work.
	if cfg.Web.Enabled && cfg.Web.SearchKeyRef != "" {
		fmt.Fprintf(&b, "  %s web search key resolves (BRAVE_API_KEY)\n", ok(cred(searchKeyEnv) != ""))
	}

	// Optional live check: prove the configured model actually authenticates,
	// not merely that a key is present. A failure makes the host not-ready.
	if check != nil {
		b.WriteString("\nLive model check:\n")
		specs := liveSpecs(cfg)
		if len(specs) == 0 {
			b.WriteString("  - skipped (no native model to probe for this backend)\n")
		}
		for _, spec := range specs {
			err := check(spec)
			fmt.Fprintf(&b, "  %s %s responds\n", ok(err == nil), spec)
			if err != nil {
				fmt.Fprintf(&b, "      %v\n", err)
				ready = false
			}
		}
	}
	return b.String(), ready
}

// liveSpecs returns the provider:model specs `doctor -check` verifies with a live
// call: the native executor (and a distinct advisor). Delegated backends use a
// CLI key with no model.Provider to probe, so they are presence-checked only.
func liveSpecs(cfg onboard.Config) []string {
	if cfg.Backend != "" && cfg.Backend != "native" {
		return nil
	}
	var specs []string
	if cfg.Executor != "" {
		specs = append(specs, cfg.Executor)
	}
	if cfg.Advisor != "" && cfg.Advisor != cfg.Executor {
		specs = append(specs, cfg.Advisor)
	}
	return specs
}

// liveChecker verifies a provider:model spec can authenticate, with a minimal
// one-token request and a short timeout. Used by `nilcore doctor -check`.
func liveChecker(cred func(string) string) func(string) error {
	return func(spec string) error {
		prov, err := provider.ResolveWith(spec, cred)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err = prov.Complete(ctx, "", []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "ping"}}}}, nil, 1)
		return err
	}
}

// backendKeyEnv returns the credential env-var the configured backend needs:
// the codex/claude-code delegated key, or the native executor's provider key.
func backendKeyEnv(cfg onboard.Config) string {
	switch cfg.Backend {
	case "codex":
		return "CODEX_API_KEY"
	case "claude-code":
		return "ANTHROPIC_API_KEY"
	default: // native
		if cfg.Executor == "" {
			return ""
		}
		return providerEnv(vendorOf(cfg.Executor))
	}
}

// channelEnvs returns the credential env-var names a channel needs, for the
// doctor's resolution check.
func channelEnvs(channelType string) []string {
	switch channelType {
	case "telegram":
		return []string{"TELEGRAM_BOT_TOKEN"}
	case "slack":
		return []string{"SLACK_APP_TOKEN", "SLACK_BOT_TOKEN"}
	default:
		return nil
	}
}

// configMain handles `nilcore config show`: print the active configuration. The
// config holds only secret *references*, so it is safe to print verbatim.
func configMain(args []string) {
	if len(args) == 0 || args[0] != "show" {
		fatal(fmt.Errorf("usage: nilcore config show [-config <path>]"))
	}
	fs := flag.NewFlagSet("config show", flag.ExitOnError)
	configPath := fs.String("config", "", "config file (default: <config-dir>/config.json)")
	_ = fs.Parse(args[1:])

	cfg := loadConfig(*configPath)
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(out))
}

// secretMain handles `nilcore secret set <name>`: store or rotate a single
// credential in the writable secret store, reading the value with echo disabled.
func secretMain(args []string) {
	if len(args) < 2 || args[0] != "set" {
		fatal(fmt.Errorf("usage: nilcore secret set <name>"))
	}
	name := args[1]
	secretDir, derr := paths.ConfigDir()
	if derr != nil {
		fatal(derr)
	}
	store := writeStore(secretDir, false, true) // auto-detect passphrase mode; prompt if needed
	if store.Name() == "env" {
		if passphraseInUse(secretDir) {
			fatal(fmt.Errorf("passphrase vault in use but no passphrase available — set NILCORE_VAULT_PASSPHRASE or run interactively"))
		}
		fatal(fmt.Errorf("no writable secret backend: no OS keychain and no encrypted vault could be created"))
	}
	val, err := onboard.PromptSecret("Value for "+name, os.Stdin, os.Stdout)
	if err != nil {
		fatal(err)
	}
	if val == "" {
		fatal(fmt.Errorf("empty value — nothing stored"))
	}
	if err := store.Set(name, val); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "stored %s in the %s backend\n", name, store.Name())
}

// commonFlags registers the flags shared by run and serve on fs.
type commonFlags struct {
	dir, backendName, runtime, image, checkCmd, logPath, config, sandboxPref *string
	maxSteps, advisorMaxCalls, escalateAfter, raceN                          *int
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		dir:             fs.String("dir", ".", "git repository tasks run against (in a disposable worktree)"),
		backendName:     fs.String("backend", "native", "native | codex | claude-code"),
		runtime:         fs.String("runtime", "podman", "container runtime: podman | docker"),
		image:           fs.String("image", onboard.DefaultImage, "sandbox image"),
		sandboxPref:     fs.String("sandbox", "auto", "sandbox backend: auto | namespace | container"),
		checkCmd:        fs.String("verify", "make verify", "command that returns 0 when the task is done"),
		logPath:         fs.String("log", "nilcore.events.jsonl", "append-only event log path"),
		config:          fs.String("config", "", "config file from `nilcore init` (default: <config-dir>/config.json)"),
		maxSteps:        fs.Int("max-steps", 60, "tool-call budget for the native loop"),
		advisorMaxCalls: fs.Int("advisor-max-calls", 4, "per-task ceiling on advisor escalations (native backend)"),
		escalateAfter:   fs.Int("escalate-after", 2, "auto-consult the advisor after N consecutive verifier failures (0 = off)"),
		raceN:           fs.Int("race-n", 1, "on a verify failure, race N parallel attempts and keep a verifier-green one (1 = off; adaptive — only fires after the single attempt fails)"),
	}
}

// runMain executes a single task and exits.
func runMain(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	goal := fs.String("goal", "", "the coding task, in plain language")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	if *goal == "" {
		fmt.Fprintln(os.Stderr, "error: --goal is required\nrun 'nilcore help' for usage")
		os.Exit(2)
	}

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))

	absDir := mustAbs(*c.dir)
	setupMCP(absDir) // generate on-demand MCP wrappers if servers are configured
	log := openLog(*c.logPath)
	defer log.Close()
	prov, err := resolveProvider(*c.backendName, b)
	if err != nil {
		fatal(err)
	}
	mem, cp := setupPersistence(log)

	orch := &agent.Orchestrator{
		BaseRepo:   absDir,
		NewEnv:     envFactory(c, prov, b.cred, resolveAdvisor(*c.backendName, b, c), log, mem, absDir),
		Log:        log,
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Approver:   policy.NewConsoleApprover(os.Stdin, os.Stdout),
		RaceN:      *c.raceN,
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

// buildRunOrchestrator constructs the single-task orchestrator the run-style
// commands share (run / propose-edit / watch): the native backend via envFactory,
// the console gate, persistence, and memory write-back. It mirrors runMain's wiring
// so a self-started or self-edit task runs through the EXACT same verified path —
// the verifier remains the sole authority on done (I2), the gate on irreversible
// actions (I3/policy).
func buildRunOrchestrator(c commonFlags, b boot, log *eventlog.Log, absDir string) *agent.Orchestrator {
	prov, err := resolveProvider(*c.backendName, b)
	if err != nil {
		fatal(err)
	}
	mem, cp := setupPersistence(log)
	return &agent.Orchestrator{
		BaseRepo:   absDir,
		NewEnv:     envFactory(c, prov, b.cred, resolveAdvisor(*c.backendName, b, c), log, mem, absDir),
		Log:        log,
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Approver:   policy.NewConsoleApprover(os.Stdin, os.Stdout),
		RaceN:      *c.raceN,
		OnSuccess:  memWriteBack(mem, absDir),
		Checkpoint: cp,
	}
}

// serveMain listens on a chat channel and gives every thread the SAME
// conversational Session the terminal front door uses (C3-T02): Telegram/Slack thus
// get queue+steer and auto-routing. It builds the deny-all channel gate, a per-
// thread Session factory wired to the full machinery (router + drivers, metered per
// CONVERSATION = threadID against a per-conversation budget wall), and runs the
// server's concurrent Permit-gated intake until interrupted.
func serveMain(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	channelName := fs.String("channel", "", "telegram | slack (default: config, else telegram)")
	budgetCeil := fs.Float64("budget", chatDefaultBudget, "global dollar ceiling per conversation (a hard wall via the meter)")
	maxConcurrent := fs.Int("max-concurrent", 0, "max serve drives running at once across all threads (0 = default 4)")
	webhookAddr := fs.String("webhook", "", "address for the SCM/CI webhook intake (e.g. 127.0.0.1:8099); needs NILCORE_WEBHOOK_SECRET")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))

	absDir := mustAbs(*c.dir)
	setupMCP(absDir) // generate on-demand MCP wrappers if servers are configured
	// Rotate an over-large event log BEFORE opening the handle, so the fresh handle
	// appends to the rotated (or recreated) path rather than a renamed inode.
	_ = maint.RotateLog(*c.logPath, serveLogMaxBytes)
	log := openLog(*c.logPath)
	defer log.Close()
	// Persistence backbone (best-effort): durable event-log mirroring + cross-project
	// memory (the opt-in live tool) + the checkpointer that gives serve threads
	// conversation persistence and leftover-task resume across a restart.
	mem, ckpt := setupPersistence(log)
	// Reclaim worktree admin entries left by a crashed prior process. SAFE: only
	// worktrees whose directory is already gone are candidates (a live run's
	// worktree directory is present), so this never collects an active worktree.
	serveGC(context.Background(), absDir, log)
	prov, err := resolveProvider(*c.backendName, b)
	if err != nil {
		fatal(err)
	}
	if prov == nil {
		// A delegated backend (codex/claude-code) has no model.Provider to route,
		// classify, or converse with — the conversational front door is a native-loop
		// experience. The legacy one-task-per-message serve is gone; require native.
		fatal(fmt.Errorf("nilcore serve requires the native backend (a model provider to route and converse with); "+
			"the %q backend has no native model", *c.backendName))
	}
	allow := principalAllowlist(b.cfg)
	if len(allow) == 0 {
		fatal(fmt.Errorf("serve refuses to start with an empty principal allowlist (no ambient authority): " +
			"set NILCORE_ALLOWLIST to a comma-separated list of permitted channel user ids, " +
			"or add \"allow\" to the channel section of config.json"))
	}
	chName := channelSpec(*channelName, b.cfg)
	rawCh, auth, err := buildChannel(chName, b.cred, allow, log)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One shared concurrency gate caps how many drives run at once across ALL
	// threads, so a burst of conversations queues rather than overrunning the host's
	// sandbox/model capacity. Drained and stopped after the serve loop returns.
	gate := newDriveGate(ctx, *maxConcurrent)
	defer gate.close()

	// Web access (parity with chat): resolved from config, with the search backend's
	// host auto-allowlisted. The proxy is bound to the serve ctx (one proxy for the
	// server). Default-deny when web is not configured.
	searchKey := b.cred(searchKeyEnv)
	webAllow, searchBackend := resolveWeb(b.cfg, "", searchKey)
	egress, proxyAddr, stopProxy, _ := startEgressProxy(ctx, webAllow)
	defer stopProxy()
	if egress.Empty() {
		fmt.Fprintln(os.Stderr, "nilcore serve: web access off (default-deny)")
	} else {
		fmt.Fprintf(os.Stderr, "nilcore serve: web access on — search: %s, %d allowed host(s)\n", searchBackend, len(webAllow))
	}

	d := serveDeps{
		flags:           c,
		provider:        prov,
		boot:            b,
		log:             log,
		baseRepo:        absDir,
		budget:          *budgetCeil,
		gate:            gate,
		mem:             mem,
		checkpoint:      ckpt,
		egress:          egress,
		egressProxyAddr: proxyAddr,
		searchBackend:   searchBackend,
		searchKey:       searchKey,
	}
	factory := serveSessionFactory(d)

	// Durable resume: re-drive any native task a prior process left running or
	// interrupted, BEFORE accepting new traffic. Each runs in a fresh disposable
	// worktree off the current HEAD (idempotent — no committed base state until a
	// gated merge) under a deny-default approver (a headless resume has no live
	// thread to answer a gate, so irreversible actions stay denied — I3).
	if ckpt != nil {
		resumeInflight(ctx, d)
	}

	// SCM/CI webhook intake (P9-T04), opt-in via --webhook: a signed GitHub webhook
	// becomes a trigger.Signal on the same gated machinery, bounded by the serve ctx.
	if *webhookAddr != "" {
		startWebhookListener(ctx, *webhookAddr, c, b, log, absDir, b.cred("NILCORE_WEBHOOK_SECRET"))
	}

	srv := &server.Server{Channel: rawCh, Auth: auth, NewSession: factory, Log: log, ResolveRoot: resolveReadRoot}
	fmt.Fprintf(os.Stderr, "nilcore serve: listening on the %s channel (Ctrl-C to stop)\n", chName)
	serveErr := srv.Serve(ctx)

	// Clean SIGTERM checkpoint: mark any task still "running" as "interrupted" so the
	// NEXT process resumes it. Uses a fresh detached context — the serve ctx is
	// already cancelled here, and the store write must still land.
	if ckpt != nil {
		ic, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = ckpt.Interrupt(ic)
		cancel()
	}
	if serveErr != nil {
		fatal(serveErr)
	}
}

// serveLogMaxBytes is the event-log rotation threshold checked at serve startup.
const serveLogMaxBytes = 64 << 20 // 64 MiB

// serveGC reclaims worktree admin entries left by a crashed prior process, driven
// through maint.GC so its "never touch an Active item" policy is exercised. The
// only candidates are worktrees whose directory is already gone, so it can never
// collect a live run's worktree — safe even with other NilCore processes running.
func serveGC(ctx context.Context, baseRepo string, log *eventlog.Log) {
	gc := maint.GC{
		List: func() ([]maint.Item, error) {
			paths, err := worktree.Prunable(ctx, baseRepo)
			items := make([]maint.Item, len(paths))
			for i, p := range paths {
				items[i] = maint.Item{ID: p}
			}
			return items, err
		},
		Remove: func(string) error { return worktree.Prune(ctx, baseRepo) },
	}
	if removed, err := gc.Collect(ctx); err != nil {
		log.Append(eventlog.Event{Kind: "maint_error", Detail: map[string]any{"op": "worktree_gc", "error": err.Error()}})
	} else if len(removed) > 0 {
		log.Append(eventlog.Event{Kind: "maint_gc", Detail: map[string]any{"reclaimed_worktrees": len(removed)}})
	}
}

// denyAllApprover refuses every irreversible action — the approver for a headless
// durable-resume. A resumed task has no live channel thread to answer a gate, so
// the safe default (I3) is to deny: the task does its reversible work and stops at
// the first gate rather than blocking or silently auto-approving.
type denyAllApprover struct{}

func (denyAllApprover) Approve(string) bool { return false }

// resumeInflight re-drives every NATIVE task a prior process left running or
// interrupted, before serve accepts new traffic. Each runs through a freshly
// reconstructed single-task orchestrator (the identical verified path as a live
// serve drive, minus the conversational seams) in a disposable worktree, under the
// deny-default approver. Checkpoint.Resume records terminal status per task, so a
// task is resumed at most once per boot. (Supervise/project resume uses the
// separate multi-agent RunState machinery and is a documented follow-on.)
func resumeInflight(ctx context.Context, d serveDeps) {
	c := d.flags
	adv := resolveAdvisor(*c.backendName, d.boot, c)
	run := func(ctx context.Context, t backend.Task) (bool, error) {
		newEnv := func(dir string) agent.Env {
			box := selectSandbox(*c.sandboxPref, *c.runtime, *c.image, dir)
			v := behavioralVerifier(box, *c.checkCmd)
			be := buildBackend("native", d.provider, d.boot.cred, adv, box, v, d.log, *c.maxSteps, d.mem, d.baseRepo)
			return agent.Env{Backend: be, Verifier: v}
		}
		orch := &agent.Orchestrator{
			BaseRepo:   d.baseRepo,
			NewEnv:     newEnv,
			Log:        d.log,
			Router:     agent.SingleRouter{},
			Spawner:    agent.NoSpawner{},
			Approver:   denyAllApprover{},
			Checkpoint: d.checkpoint,
		}
		out, err := orch.Execute(ctx, t)
		return out.Verified, err
	}
	if err := d.checkpoint.Resume(ctx, run); err != nil {
		d.log.Append(eventlog.Event{Kind: "resume_error", Detail: map[string]any{"error": err.Error()}})
	}
}

// serveDeps is the resolved input to the serve Session factory: everything a per-
// thread Session needs after flags + boot resolve. It mirrors chatDeps but carries
// the serve-mode budget ceiling and is keyed per CONVERSATION (the channel
// threadID) rather than the single "chat-local".
type serveDeps struct {
	flags      commonFlags
	provider   model.Provider
	boot       boot
	log        *eventlog.Log
	baseRepo   string
	budget     float64
	gate       *driveGate        // shared serve drive-concurrency cap
	mem        *memory.Memory    // cross-project memory; feeds the opt-in NILCORE_LIVE_INDEX live tool (nil ⇒ none)
	checkpoint *agent.Checkpoint // durable task/conversation persistence (nil ⇒ in-memory only, no resume)

	// Web access, resolved once from config (nilcore init) and applied to every
	// thread's drives — parity with the chat front door. egress empty ⇒ default-deny.
	egress          policy.Egress
	egressProxyAddr string
	searchBackend   tools.SearchBackend
	searchKey       string
}

// webEnabled reports whether sandboxed web access is configured for serve.
func (d serveDeps) webEnabled() bool {
	return !d.egress.Empty() && d.egressProxyAddr != ""
}

// serveSessionFactory returns the server.SessionFactory that builds one wired
// conversational Session per thread. Each thread is its OWN conversation: it gets
// its own budget.Ledger with the global ceiling (the per-conversation wall, §6),
// one metered provider keyed by the threadID so N back-to-back drives in that
// thread share ONE ceiling (never N×ceiling), the SupervisorFirstRouter, and the
// four drivers running the EXISTING native/supervisor/project machinery with the
// thread's Inbox + channel Emitter + channel Approver wired in. The Emitter and
// Approver are supplied by the server (transport-bound: Update for reasoning, Ask
// for gates); the factory owns only the machinery assembly.
func serveSessionFactory(d serveDeps) server.SessionFactory {
	return func(ctx context.Context, threadID, sender string, out emit.Emitter, approver policy.Approver) *session.Session {
		// One conversation = one ledger + one global ceiling = one metered provider
		// keyed by the threadID. Routing, drives, chat replies, and the summarize
		// fold-back all charge this single wall (§6).
		ledger := budget.New()
		ledger.SetGlobalCeiling(d.budget)
		metered := &meter.Provider{Inner: d.provider, Ledger: ledger, Task: threadID, Price: meter.NewTable()}

		sess := session.New(threadID, sender, d.baseRepo, d.log)
		sess.Out = out // reasoning/intent streams back to this thread (Channel.Update)
		sess.Budget = ledger
		// Conversation persistence: with a checkpointer, this thread's bounded
		// WorkState is saved after every drive fold and re-hydrated here on a
		// restart (the threadID is the stable conversation key) — so a restarted
		// serve continues each thread rather than starting it blank.
		if d.checkpoint != nil {
			sess.Store = d.checkpoint
			sess.Restore(ctx)
		}
		// Context-usage tracking + auto-compaction parity with the chat front door.
		sess.CtxWindow = meter.CtxWindow
		sess.Summarizer = metered
		metered.OnUsage = sess.RecordUsage

		sess.Router = &session.SupervisorFirstRouter{
			Classifier:      metered,
			ShouldSupervise: chatShouldSupervise,
			Log:             d.log,
			ID:              threadID,
		}
		// Execute-mode sizing parity with chat: a pinned /execute bypasses the router
		// and sizes native-vs-supervise with the same heuristic.
		sess.Sizer = chatShouldSupervise
		sess.Drivers = session.Drivers{
			Native:    session.NewNativeDriver(gateNative(d.gate, serveNativeRun(d, metered, approver, threadID)), metered, threadID),
			Supervise: session.NewSuperviseDriver(gateSupervise(d.gate, serveSuperviseRun(d, ledger, approver)), metered),
			Project:   session.NewProjectDriver(gateProject(d.gate, serveProjectRun(d, ledger, approver)), metered),
			Chat:      session.NewChatDriver(metered),
		}
		return sess
	}
}

// serveNativeRun is the serve-mode RunNativeFunc: it runs ONE native drive through
// the orchestrator's single-task path with the session's Inbox + Seed (prior
// History — continue, not restart) + Emitter wired onto backend.Native, the per-
// drive worktree keyed by TaskID, and the thread's channel Approver routing gates
// back over chat. The loop runs with the conversation-metered provider so spend
// keys by the threadID, never the per-drive task id (§6). It mirrors chatNativeRun
// but with the channel approver in place of the console one.
func serveNativeRun(d serveDeps, metered model.Provider, approver policy.Approver, threadID string) session.RunNativeFunc {
	adv := resolveAdvisor(*d.flags.backendName, d.boot, d.flags)
	return func(ctx context.Context, in session.NativeRun) (session.DriveOutcome, error) {
		newEnv := func(dir string) agent.Env {
			box := selectSandbox(*d.flags.sandboxPref, *d.flags.runtime, *d.flags.image, dir)
			// Route a container box through the allowlist proxy when web access is on
			// (no-op otherwise; default-deny stays the norm), and bind-mount /add'd
			// roots read-only. Mirrors chat.
			applyContainerEgress(box, d.egress, d.egressProxyAddr, *d.flags.runtime)
			applyContainerReadRoots(box, in.ReadRoots)
			// Read-only modes ship nothing, so there is nothing to gate (I2) — a
			// pass-through verifier; Execute/Auto get the real project verifier.
			v := behavioralVerifier(box, *d.flags.checkCmd)
			if in.Mode.ReadOnly() {
				v = verify.Pass{}
			}
			n := serveNativeBackend(d, metered, adv, box, v, in)
			return agent.Env{Backend: n, Verifier: v}
		}
		orch := &agent.Orchestrator{
			BaseRepo:   d.baseRepo,
			NewEnv:     newEnv,
			Log:        d.log,
			Router:     agent.SingleRouter{},
			Spawner:    agent.NoSpawner{},
			Approver:   approver,       // gates route back to this thread (Channel.Ask)
			RaceN:      *d.flags.raceN, // escalate a verify failure to a best-of-N race
			Checkpoint: d.checkpoint,   // records running/done so a restart can resume a leftover drive
		}
		out, err := orch.Execute(ctx, backend.Task{ID: in.TaskID, Goal: modePreamble(in.Mode) + in.Goal})
		if err != nil {
			return session.DriveOutcome{}, err
		}
		return session.DriveOutcome{Summary: out.Summary, Verified: out.Verified}, nil
	}
}

// serveNativeBackend builds the backend.Native for one serve drive with the
// conversational seams attached: the session Inbox (steer/queue), Seed (prior
// History — continue, not restart), and Emitter (live reasoning over Channel.Update).
// It mirrors chatNativeBackend but is fed by the serve deps.
func serveNativeBackend(d serveDeps, prov model.Provider, adv advisorCfg, box sandbox.Sandbox, v verify.Verifier, in session.NativeRun) *backend.Native {
	// Mode capability parity with the chat front door: a read-only Discuss/Plan drive
	// over a channel gets the write-free registry + shell off (the same structural
	// no-write guarantee), Execute/Auto get the full set. Without this, a /plan pinned
	// over Telegram would be advertised but not enforced.
	reg, guard, disableShell := capabilityForMode(in.Mode)
	if len(in.ReadRoots) > 0 {
		reg.Register(tools.ReadTool{ReadRoots: in.ReadRoots})
		reg.Register(tools.SearchTool{ReadRoots: in.ReadRoots})
	}
	// Web tools (parity with chat): advertised only when web access is on and the box
	// is egress-capable; the body is fenced as untrusted data (I7), the search key
	// (brave) injected via per-run env, never the command string (I3).
	if d.webEnabled() {
		if _, ok := box.(*sandbox.Container); ok {
			reg.Register(tools.WebFetchTool{Box: box})
			if d.searchBackend != tools.SearchOff && d.egress.Allow(tools.SearchHostFor(d.searchBackend)) {
				reg.Register(tools.WebSearchTool{Box: box, Backend: d.searchBackend, APIKey: d.searchKey})
			}
			// browser_view (P9-T02): opt-in via NILCORE_BROWSER, same as chat.
			if drv := os.Getenv("NILCORE_BROWSER"); drv != "" {
				reg.Register(tools.BrowserViewTool{Box: box, DriverCmd: drv})
			}
		}
	}
	n := &backend.Native{
		Model:        prov,
		Box:          box,
		Verifier:     v,
		Log:          d.log,
		Tools:        reg,
		CommandGuard: guard,
		DisableShell: disableShell,
		MaxSteps:     *d.flags.maxSteps,
		Seed:         in.Seed,
	}
	if in.Inbox != nil {
		n.Inbox = in.Inbox
	}
	if in.Emitter != nil {
		n.Emitter = in.Emitter
	}
	if adv.prov != nil {
		n.Advisor = advisor.New(adv.prov, adv.maxCalls)
		n.EscalateAfter = adv.escalateAfter
	}
	// Live incremental code-intelligence (P3-T16), opt-in via NILCORE_LIVE_INDEX —
	// the serve loop gets the same `live` tool as the run/chat paths.
	if os.Getenv("NILCORE_LIVE_INDEX") != "" {
		n.LiveSession = liveSession(d.mem, d.baseRepo)
	}
	return n
}

// serveSuperviseRun / serveProjectRun assemble the multi-agent stack for one serve
// drive via buildStack, pinning the thread's shared conversation ledger (§6) and the
// channel approver (the single human promote routes back over chat). Like the chat
// path, the planner's own Inbox/Out wiring is a documented follow-on; the supervised/
// project drive itself runs bounded, verifier-gated, and charged against the per-
// conversation wall, and its outcome folds back exactly like a native drive.
func serveSuperviseRun(d serveDeps, ledger *budget.Ledger, approver policy.Approver) session.RunSuperviseFunc {
	return func(ctx context.Context, goal string, _ []model.Message, _ session.InboxHandle, _ emit.Emitter) (session.DriveOutcome, error) {
		stack, err := buildStack(serveBuildDeps(d, ledger, approver, goal))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		o, err := stack.loop.Run(ctx)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		return session.DriveOutcome{Summary: o.Summary, Branch: o.Branch, Verified: o.Done}, nil
	}
}

func serveProjectRun(d serveDeps, ledger *budget.Ledger, approver policy.Approver) session.RunProjectFunc {
	return func(ctx context.Context, goal string, _ summarize.ContextSummary, _ emit.Emitter) (session.DriveOutcome, error) {
		stack, err := buildStack(serveBuildDeps(d, ledger, approver, goal))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		o, err := stack.loop.Run(ctx)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		return session.DriveOutcome{Summary: o.Summary, Branch: o.Branch, Verified: o.Done}, nil
	}
}

// serveBuildDeps adapts the serve deps to a buildDeps for buildStack, pinning the
// shared conversation ledger (so the supervised/project drive charges the SAME
// per-conversation ceiling, §6) and the channel approver (the gate routes back over
// chat). It mirrors chatBuildDeps' interactive rail sizing.
func serveBuildDeps(d serveDeps, ledger *budget.Ledger, approver policy.Approver, goal string) buildDeps {
	adv := resolveAdvisor("native", d.boot, d.flags)
	strong := adv.prov
	if strong == nil {
		strong = d.provider
	}
	return buildDeps{
		goal:     goal,
		dir:      d.baseRepo,
		runtime:  *d.flags.runtime,
		image:    *d.flags.image,
		verify:   *d.flags.checkCmd,
		maxIter:  defaultChatMaxIter,
		maxFan:   defaultChatMaxFanout,
		maxAgent: defaultChatMaxAgents,
		maxDepth: 1,
		maxSteps: *d.flags.maxSteps,
		budget:   d.budget,
		executor: d.provider,
		strong:   strong,
		log:      d.log,
		approver: approver,
		ledger:   ledger, // pin the per-conversation wall (§6)
	}
}

// resolveProvider builds the model provider for the native backend and validates
// the backend name + required secret up front. The model spec is NILCORE_MODEL,
// else the configured executor, else the built-in default; the key resolves
// environment-first then SecretStore via b.cred. A missing key is reported with
// the actionable remedy (run init / export the var) rather than a bare error.
func resolveProvider(backendName string, b boot) (model.Provider, error) {
	switch backendName {
	case "native":
		spec := modelSpec(os.Getenv("NILCORE_MODEL"), b.cfg.Executor)
		p, err := provider.ResolveWith(spec, b.cred)
		if err != nil {
			if env := providerEnv(vendorOf(spec)); env != "" {
				return nil, fmt.Errorf("%w; run `nilcore init` to store the key, or set %s in the environment", err, env)
			}
			return nil, fmt.Errorf("%w; run `nilcore init` to store the key", err)
		}
		// Wrap the executor in the resilience layer: transient API errors retry with
		// jittered exponential backoff and a circuit breaker trips on sustained
		// failure, so an unattended run survives provider blips. It stays INNERMOST —
		// the meter (the budget wall) wraps this, so budget.ErrCeiling is never
		// mistaken for a transient fault and retried. Failover across providers
		// activates when more than one is configured; a single provider gets
		// retry + breaker. On invalid options, fall back to the bare provider.
		res, rerr := model.NewResilient([]model.Provider{p}, model.Options{
			MaxRetries:       2,
			Jitter:           200 * time.Millisecond,
			BreakerThreshold: 4,
		})
		if rerr != nil {
			return p, nil
		}
		return res, nil
	case "codex", "claude-code":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (want native | codex | claude-code)", backendName)
	}
}

// advisorCfg carries the optional strong-model advisor tier from boot into the
// per-task native backend. A nil prov means no advisor (the loop runs without
// escalation, exactly as before).
type advisorCfg struct {
	prov          model.Provider
	maxCalls      int
	escalateAfter int
}

// resolveAdvisor builds the advisor tier for the native backend: NILCORE_ADVISOR,
// else the configured advisor model, else none. A configured advisor that cannot
// be resolved (e.g. missing key) is reported and skipped — the run proceeds
// without escalation rather than failing, since the advisor is an enhancement.
func resolveAdvisor(backendName string, b boot, c commonFlags) advisorCfg {
	adv := advisorCfg{maxCalls: *c.advisorMaxCalls, escalateAfter: *c.escalateAfter}
	if backendName != "native" {
		return adv
	}
	spec := os.Getenv("NILCORE_ADVISOR")
	if spec == "" {
		spec = b.cfg.Advisor
	}
	if spec == "" {
		return adv
	}
	p, err := provider.ResolveWith(spec, b.cred)
	if err != nil {
		fmt.Fprintf(os.Stderr, "advisor disabled: %v\n", err)
		return adv
	}
	adv.prov = p
	return adv
}

// sandboxReport renders which sandbox backend `nilcore` will use on this host:
// the namespace backend (no container runtime needed) when the kernel supports
// it, else a container. It probes the live host, so it lives here rather than in
// the pure, host-independent diagnose().
func sandboxReport(runtime string) string {
	if runtime == "" {
		runtime = "podman"
	}
	ns, reason, container := sandbox.Available(runtime)

	var b strings.Builder
	mark := func(c bool) string {
		if c {
			return "✓"
		}
		return "✗"
	}
	b.WriteString("\nSandbox (model-emitted execution, I4):\n")
	if ns {
		b.WriteString("  ✓ namespace backend available (user namespaces + Landlock) — no container runtime needed\n")
	} else {
		fmt.Fprintf(&b, "  · namespace backend unavailable: %s\n", reason)
	}
	fmt.Fprintf(&b, "  %s container runtime %q on PATH\n", mark(container), runtime)
	switch {
	case ns:
		b.WriteString("  → auto: the namespace backend will be preferred\n")
	case container:
		b.WriteString("  → auto: the container backend will be used\n")
	default:
		b.WriteString("  ✗ no usable sandbox — install podman/docker, or run on a Landlock-capable Linux kernel\n")
	}
	return b.String()
}

// selectSandbox builds the sandbox for one worktree, preferring the namespace
// backend (no container runtime, image, or daemon needed) when the kernel
// supports it and the operator hasn't pinned a choice. prefer is the -sandbox
// flag ("auto" by default); "auto"/"" also honors the NILCORE_SANDBOX env
// override. A namespace request the host can't satisfy is reported and degraded
// to a container rather than aborting the run — sandboxing still holds (I4).
func selectSandbox(prefer, runtime, image, dir string) sandbox.Sandbox {
	if prefer == "" || prefer == string(sandbox.Auto) {
		if env := os.Getenv("NILCORE_SANDBOX"); env != "" {
			prefer = env
		}
	}
	box, err := sandbox.New(sandbox.Options{
		Prefer:  sandbox.Backend(prefer),
		Runtime: runtime,
		Image:   image,
		HostDir: dir,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: %v; using the container backend\n", err)
		return sandbox.NewContainer(runtime, image, dir)
	}
	return box
}

// envFactory builds the per-worktree backend+verifier factory.
func envFactory(c commonFlags, prov model.Provider, cred func(string) string, adv advisorCfg, log *eventlog.Log, mem *memory.Memory, project string) func(string) agent.Env {
	return func(dir string) agent.Env {
		box := selectSandbox(*c.sandboxPref, *c.runtime, *c.image, dir)
		v := behavioralVerifier(box, *c.checkCmd)
		be := buildBackend(*c.backendName, prov, cred, adv, box, v, log, *c.maxSteps, mem, project)
		// Operator steering (P10-T01): a committed NILCORE.md / AGENTS.md is present in
		// the worktree checkout; load it once and prepend as trusted instructions on
		// the native backend. nil/empty ⇒ byte-identical; only the native loop reads it.
		if n, ok := be.(*backend.Native); ok {
			if steer, _ := steering.DiscoverAndLoad(dir); steer != "" {
				n.SteeringContext = func() string { return steer }
			}
		}
		return agent.Env{Backend: be, Verifier: v}
	}
}

func buildBackend(name string, prov model.Provider, cred func(string) string, adv advisorCfg, box sandbox.Sandbox, v verify.Verifier, log *eventlog.Log, maxSteps int, mem *memory.Memory, project string) backend.CodingBackend {
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
			Tools:        loopTools(),
			CommandGuard: policy.DefaultCommandPolicy().Check,
			MaxSteps:     maxSteps,
		}
		// A fresh advisor per task so its per-task consult ceiling is honored.
		if adv.prov != nil {
			n.Advisor = advisor.New(adv.prov, adv.maxCalls)
			n.EscalateAfter = adv.escalateAfter
		}
		if mem != nil {
			n.MemoryContext = func(ctx context.Context, _ string) string {
				blk, _ := mem.Context(ctx, memory.ScopeProject, project, "", 10)
				return blk
			}
		}
		// Live incremental code-intelligence (P3-T16), opt-in via NILCORE_LIVE_INDEX:
		// the loop gets a worktree-aware `live` tool whose graph re-indexes edits
		// incrementally and fuses project memory. Off by default (nil ⇒ byte-identical;
		// no full per-run index cost unless requested).
		if os.Getenv("NILCORE_LIVE_INDEX") != "" {
			n.LiveSession = liveSession(mem, project)
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
// — a freshly-deployed bot is inert to whoever merely finds it. A missing token
// is reported with the remedy rather than a bare requirement.
// buildChannel returns the RAW transport plus the deny-all-by-default Authorized
// gate over it. Serve mode (C3-T02) drives Receive/Update/Ask on the raw transport
// and does its OWN per-message Permit check in the intake path via the returned
// Authorized — it deliberately does NOT consume Authorized.Receive, whose internal
// filtering loop would swallow an unauthorized message before the server's per-
// thread intake could log and refuse it (docs/CONVERSATIONAL.md §5.4). The gate-
// button authorizer is still wired to Permit, so a gate answer stays gated.
func buildChannel(name string, cred func(string) string, allow []string, log *eventlog.Log) (channel.Channel, *channel.Authorized, error) {
	var bot authChannel
	switch name {
	case "telegram":
		tok := cred("TELEGRAM_BOT_TOKEN")
		if tok == "" {
			return nil, nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required for the telegram channel; run `nilcore init` or set it in the environment")
		}
		bot = telegram.New(tok)
	case "slack":
		app, bt := cred("SLACK_APP_TOKEN"), cred("SLACK_BOT_TOKEN")
		if app == "" || bt == "" {
			return nil, nil, fmt.Errorf("SLACK_APP_TOKEN and SLACK_BOT_TOKEN are required for the slack channel; run `nilcore init` or set them in the environment")
		}
		bot = slack.New(app, bt)
	default:
		return nil, nil, fmt.Errorf("unknown channel %q (want telegram | slack)", name)
	}
	auth := channel.NewAuthorized(bot, allow, log) // the per-message Permit gate
	bot.SetAuthorizer(auth.Permit, log)            // gate-button answers stay gated
	return bot, auth, nil
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
	return boot{cfg: cfg, cred: newCredResolver(cfg, detectStore(false), os.Getenv)}
}

// detectStore selects the host SecretStore. It resolves the config dir and
// delegates to detectStoreIn; with no config dir it falls back to the keychain or
// the read-only environment store.
func detectStore(forWrite bool) secrets.SecretStore {
	dir, err := paths.ConfigDir()
	if err != nil {
		if kc := secrets.Detect(); kc.Name() == "keychain" {
			return kc
		}
		return secrets.EnvStore{}
	}
	return detectStoreIn(dir, forWrite)
}

// detectStoreIn picks the SecretStore for dir. On the write path (`nilcore init`)
// it commits to a single backend: the OS keychain if present, else a freshly
// provisioned encrypted file vault, else the read-only environment store. On the
// read path (run/serve) it returns a fallthrough CHAIN of every available backend
// (keychain, an existing vault, the environment) so a key stored at init is found
// at run time even if the keychain became unavailable in between — and a
// pure-environment host (no vault) never has files created for it.
func detectStoreIn(dir string, forWrite bool) secrets.SecretStore {
	return assembleStore(dir, forWrite, secrets.Detect())
}

// assembleStore is detectStoreIn with the host keychain injected, so the
// keychain-present and keychain-absent paths are both testable hermetically.
func assembleStore(dir string, forWrite bool, keychain secrets.SecretStore) secrets.SecretStore {
	hasKeychain := keychain.Name() == "keychain"
	if forWrite {
		if hasKeychain {
			return keychain
		}
		if v := fileVault(dir); v.Name() == "file" {
			return v
		}
		return secrets.EnvStore{}
	}
	var stores []secrets.SecretStore
	if hasKeychain {
		stores = append(stores, keychain)
	}
	// Include the file vault only when the vault and its key-source already exist,
	// so a read never provisions a fresh key/salt and a pure-environment host
	// creates no files. Key-file mode needs secrets.key; passphrase mode needs the
	// salt plus NILCORE_VAULT_PASSPHRASE (env only on the read path — no prompt).
	if _, err := os.Stat(filepath.Join(dir, "secrets.vault")); err == nil {
		if passphraseInUse(dir) {
			if v := passphraseVault(dir, vaultPassphrase(false), false); v.Name() == "file" {
				stores = append(stores, v)
			}
		} else if _, err := os.Stat(filepath.Join(dir, "secrets.key")); err == nil {
			if v := fileVault(dir); v.Name() == "file" {
				stores = append(stores, v)
			}
		}
	}
	stores = append(stores, secrets.EnvStore{})
	if len(stores) == 1 {
		return stores[0]
	}
	return chainStore{stores}
}

// chainStore tries an ordered list of backends so a secret stored in any one of
// them resolves. Get returns the first hit; Set/Delete target the first backend
// that accepts the write (the read-only environment store is skipped).
type chainStore struct{ stores []secrets.SecretStore }

func (c chainStore) Get(name string) (string, error) {
	for _, s := range c.stores {
		if v, err := s.Get(name); err == nil {
			return v, nil
		}
	}
	return "", secrets.ErrNotFound
}

func (c chainStore) Set(name, value string) error {
	for _, s := range c.stores {
		if err := s.Set(name, value); err == nil {
			return nil
		}
	}
	return secrets.ErrReadOnly
}

func (c chainStore) Delete(name string) error {
	err := secrets.ErrNotFound
	for _, s := range c.stores {
		e := s.Delete(name)
		if e == nil {
			return nil
		}
		err = e
	}
	return err
}

func (c chainStore) Name() string {
	names := make([]string, 0, len(c.stores))
	for _, s := range c.stores {
		names = append(names, s.Name())
	}
	return strings.Join(names, "+")
}

// fileVault opens (provisioning the master key if absent) the encrypted vault in
// dir, falling back to the read-only environment store if it cannot be sealed.
func fileVault(dir string) secrets.SecretStore {
	key, err := secrets.MasterKeyFromFile(filepath.Join(dir, "secrets.key"))
	if err != nil {
		return secrets.EnvStore{}
	}
	vault, err := secrets.OpenFileVault(filepath.Join(dir, "secrets.vault"), key)
	if err != nil {
		return secrets.EnvStore{}
	}
	return vault
}

// vaultSaltFile marks a passphrase-sealed vault: when it is present the vault's
// master key is derived from NILCORE_VAULT_PASSPHRASE + this salt, not a key file.
const vaultSaltFile = "secrets.salt"

// passphraseInUse reports whether dir holds a passphrase-sealed vault (vs. the
// key-file default) — detected by the presence of the salt file.
func passphraseInUse(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, vaultSaltFile))
	return err == nil
}

// passphraseVault opens the passphrase-sealed file vault in dir, deriving the key
// from pass + the stored salt. With create set (first-time setup), a missing salt
// is generated. Returns the read-only EnvStore when pass is empty or the vault
// cannot be sealed, so callers degrade rather than crash.
func passphraseVault(dir, pass string, create bool) secrets.SecretStore {
	if pass == "" {
		return secrets.EnvStore{}
	}
	salt, err := secrets.ReadOrCreateSalt(filepath.Join(dir, vaultSaltFile), create)
	if err != nil {
		return secrets.EnvStore{}
	}
	key := secrets.MasterKeyFromPassphrase(pass, salt, 0)
	vault, err := secrets.OpenFileVault(filepath.Join(dir, "secrets.vault"), key)
	if err != nil {
		return secrets.EnvStore{}
	}
	return vault
}

// vaultPassphrase resolves the vault passphrase: NILCORE_VAULT_PASSPHRASE first
// (the unattended path — inject it via systemd EnvironmentFile and the like),
// then an interactive prompt with echo off. Empty when neither is available.
func vaultPassphrase(interactive bool) string {
	if p := os.Getenv("NILCORE_VAULT_PASSPHRASE"); p != "" {
		return p
	}
	if !interactive {
		return ""
	}
	p, _ := onboard.PromptSecret("Vault passphrase", os.Stdin, os.Stdout)
	return p
}

// writeStore selects the writable secret backend for init / secret-set. Passphrase
// mode is used when explicitly requested (`init -vault passphrase`) or already set
// up (a salt file exists); otherwise the default keychain→key-file selection
// applies. interactive permits prompting for the passphrase when it is not in env.
func writeStore(dir string, requestPassphrase, interactive bool) secrets.SecretStore {
	if requestPassphrase || passphraseInUse(dir) {
		return passphraseVault(dir, vaultPassphrase(interactive), true)
	}
	return detectStoreIn(dir, true)
}

// loadConfig reads config.json (from configPath or the default location). A
// missing config is not an error — it yields the zero Config, and the run falls
// back to the environment + built-in defaults. A present-but-invalid config is
// surfaced as a loud stderr warning (then degrades) rather than vanishing
// silently, so a typo in a hand-edited config.json is diagnosable.
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
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "warning: ignoring %v\n", err) // err already names the path
		}
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
// SecretStore reference recorded in config.json. The codex key, when captured by
// the wizard as a provider, is resolvable from the store like any other.
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
	// The web-search key resolves like a provider key: env-first then SecretStore via
	// this ref (I3 — never the key itself, never logged, never in a prompt).
	if cfg.Web.SearchKeyRef != "" {
		m["BRAVE_API_KEY"] = cfg.Web.SearchKeyRef
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
	case "codex":
		return "CODEX_API_KEY" // delegated backend key, resolved like a provider key
	default:
		return ""
	}
}

// vendorOf returns the provider vendor of a "provider:model" spec (a bare model
// is Anthropic, a bare "openrouter" is OpenRouter), so a missing-key error can
// name the exact environment variable to set.
func vendorOf(spec string) string {
	if i := strings.Index(spec, ":"); i >= 0 {
		return spec[:i]
	}
	if spec == "openrouter" {
		return "openrouter"
	}
	return "anthropic"
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

// applyConfigDefaults lets config.json supply runtime/image/backend when the
// operator did not pass the corresponding flag. Explicit flags always win;
// built-in defaults fill the rest.
func applyConfigDefaults(c commonFlags, cfg onboard.Config, set map[string]bool) {
	if !set["runtime"] && cfg.Runtime != "" {
		*c.runtime = cfg.Runtime
	}
	if !set["image"] && cfg.Image != "" {
		*c.image = cfg.Image
	}
	if !set["backend"] && cfg.Backend != "" {
		*c.backendName = cfg.Backend
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
