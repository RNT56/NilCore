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
	"nilcore/internal/blastbudget"
	"nilcore/internal/budget"
	"nilcore/internal/channel"
	"nilcore/internal/channel/slack"
	"nilcore/internal/channel/telegram"
	"nilcore/internal/emit"
	"nilcore/internal/eventlog"
	"nilcore/internal/experience"
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
	"nilcore/internal/trust"
	"nilcore/internal/verify"
	"nilcore/internal/wake"
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
	case "do":
		doMain(args[1:])
	case "tui":
		tuiMain(args[1:])
	case "serve":
		serveMain(args[1:])
	case "build":
		buildMain(args[1:])
	case "swarm":
		swarmMain(args[1:])
	case "decompose":
		decomposeMain(args[1:])
	case "flows":
		flowsMain(args[1:])
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
	case "report":
		reportMain(args[1:])
	case "trust":
		trustMain(args[1:])
	case "selfacc":
		selfaccMain(args[1:])
	case "experience":
		experienceMain(args[1:])
	case "lessons":
		lessonsMain(args[1:])
	case "flywheel":
		flywheelMain(args[1:])
	case "objective":
		objectiveMain(args[1:])
	case "auto-approvals":
		autoApprovalsMain(args[1:])
	case "capability":
		capabilityMain(args[1:])
	case "trace", "why":
		traceMain(args[1:])
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
	case "browse":
		browseMain(args[1:])
	case "desktop":
		desktopMain(args[1:])
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
const usageText = `NilCore — ONE coding agent you talk to. It writes the code, runs and verifies it
works (in a real browser/app when needed), and reaches for more power — delegation, a
swarm, a backend — when the job calls for it. The harness is small; the model is the engine.

TALK TO IT — this is the product:
  nilcore                               start the conversation; it decides how to work and works while you type
  nilcore chat [-dir ./repo]            the same conversation, explicitly (-dir picks the repo)
  nilcore do -goal "<task>"             one task; the agent ROUTES it to run/build/swarm and dispatches (-dry-run to preview)
  nilcore tui                           the conversation in a full-screen UI
  nilcore serve -channel telegram       run the agent as a chat bot (Telegram/Slack/…)
  nilcore -goal "<task>" [-dir ./repo]  one-shot: run a single task to completion, no conversation

WHAT IT CAN DO — capabilities the agent reaches for on demand (or invoke directly):
  nilcore build -goal "<project>" -new ./svc          drive a whole project to a verifier-green tree (multi-agent)
  nilcore swarm -goal "<objective>" -preset research  fan out a verified agent swarm (typed artifacts, requeue-until-clean)
  nilcore decompose -goal "<a> and <b>"  split a goal into independent sub-goals, run each, merge-and-re-verify into one tip (kernel recursion)
  nilcore flows validate -flow f.json   preflight a portable agentic-flows workflow; 'flows run' executes its agent_task nodes (github.com/RNT56/agentic-flows)
  nilcore browse -goal "..."            drive a persistent in-sandbox browser (observe→plan→act→verify; findings re-verified)
  nilcore desktop -goal "..."           drive a contained virtual desktop (Set-of-Marks; --mac-host drives a real Mac, gated)
  nilcore watch [-signals ./signals]    self-start tasks from dropped signal files (reversible auto, else gated)
  nilcore schedule -goal "..." -every 1h   self-start on a cron cadence
  nilcore flywheel [--once]             run the self-improvement loop (eval → mine failures → propose a gated fix)
  nilcore propose-edit -goal "..." -paths ...   gated self-edit of the agent's own prompts/skills/tools
  (run/build/swarm run on ONE engine — the kernel; the conversation picks which preset. Set NILCORE_KERNEL=0 to opt out.)

THE COCKPIT — read-only / operator surfaces to inspect, audit, and steer:
  nilcore trace <task> | nilcore why <task>   explain why the agent did what it did (event log, read-only)
  nilcore report [-format text|matrix]  the verification report (the I2 trust gate)
  nilcore trust [-format text|json]     the Trust Ledger scoreboard (verifier-judged routing; the verifier still decides)
  nilcore experience | nilcore capability     the learned-state scoreboard / a drive's exact capability descriptor
  nilcore lessons                       recurring verifier-failure patterns the agent has learned from
  nilcore auto-approvals [-denied]      account of past graduated auto-approvals + the per-class undo story
  nilcore objective <list|add|disable|enable>   manage the standing-objectives backlog (operator-only)
  nilcore selfacc <propose|check>       review self-authored acceptance verifiers (operator-gated; NILCORE_SELFACC to bind)
  nilcore inspect [health]              replay the event log (summary), or probe its health (exit 0/1)
  nilcore registry list|install <m>     manage local skills / MCP-server specs

SETUP & MAINTENANCE:
  nilcore init                          guided setup: keys, runtime, backend, channel, allowlist
  nilcore doctor                        check whether this host is ready to run/serve
  nilcore config show                   print the active configuration (secret-free)
  nilcore secret set <name>             store or rotate a single secret in the secret store
  nilcore mcp-call <server> <tool> ...  invoke a configured MCP tool (the runtime bridge for generated wrappers)
  nilcore version                       print the build version

Run 'nilcore <command> -h' for a command's flags.
First time? Run 'nilcore init', then just run 'nilcore' and talk to it.
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
	dir, backendName, backends, preferBackend, runtime, image, checkCmd, logPath, config, sandboxPref *string
	maxSteps, advisorMaxCalls, escalateAfter, raceN                                                   *int
	autoSupervise                                                                                     *bool
	blastRadius                                                                                       *string
}

func registerCommon(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		dir:         fs.String("dir", ".", "git repository tasks run against (in a disposable worktree)"),
		backendName: fs.String("backend", "native", "native | codex | claude-code | auto (auto = the system picks the best AVAILABLE backend, seeded by -prefer-backend / config, learned by the Trust Ledger)"),
		// One-run override of the durable PreferredBackend setting (config.preferred_backend).
		// Only consulted when the resolved backend is "auto"; it seeds the auto cold-start
		// order (preferred-first). Empty ⇒ fall back to config.preferred_backend, else native.
		preferBackend: fs.String("prefer-backend", "", "preferred backend to try first under -backend auto: native | codex | claude-code (overrides config preferred_backend for this run)"),
		// Phase 13 multi-backend strength-routing (additive, default-off). EMPTY ⇒ the
		// single -backend path, byte-identical. A single name ⇒ same as -backend <name>.
		// Two or more DISTINCT names ⇒ the orchestrator tries the historically-strongest
		// first (Trust Ledger) and, on a verify-fail, races the distinct backends with the
		// VERIFIER as judge (I2). When both are given, -backends wins. COST: each race runs
		// N backends — the budget meter ceiling caps the spend.
		backends:        fs.String("backends", "", "comma-separated backend set for multi-backend strength-routing, e.g. native,codex,claude-code (empty = single -backend path; two or more activates routing; -backends wins over -backend)"),
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
		autoSupervise:   fs.Bool("auto-supervise", false, "let a complex goal opportunistically scale up to the supervised project loop (bounded by the same caps as `nilcore build`); off = single-task, byte-identical"),
		blastRadius:     fs.String("blast-radius", "off", "blast-radius envelope for unattended / auto-approval runs: off | tight | standard (bounds distinct egress hosts, auto-approval count, sandbox wall-time, and per-UTC-day auto-approval $); off = unfenced, byte-identical"),
	}
}

// knownBackendNames is the set of backend names buildBackend can construct. A
// -backends entry outside this set is FATAL (mirrors resolveProvider's unknown-name
// rejection), so an operator typo never silently routes nowhere.
var knownBackendNames = map[string]bool{"native": true, "codex": true, "claude-code": true}

// parseBackends turns the -backends flag into the de-duplicated, order-preserving
// list of backend tokens for the multi-backend path. EMPTY ⇒ nil (the caller stays
// on the single -backend path, byte-identical). Each token is either a concrete
// backend (validated against knownBackendNames; an unknown name is FATAL like other
// flags) or the sentinel "auto", which the caller (wireMultiBackend) EXPANDS to the
// host's availableBackends before the len<=1 single-path check. A single distinct
// concrete name ⇒ a one-element slice, which the orchestrator treats as the single
// path (multiBackend() needs len > 1), byte-identical to -backend <name>.
func parseBackends(spec string) []string {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if name != "auto" && !knownBackendNames[name] {
			fatal(fmt.Errorf("unknown backend %q in -backends (want native | codex | claude-code | auto)", name))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// expandAutoBackends replaces every "auto" token in tokens with the host's
// availableBackends (ledger-orderable downstream), preserving the operator's
// explicit order and de-duplicating. `-backends auto` ⇒ ALL available backends
// compete; "auto" mixed with explicit names ⇒ their union (explicit names keep
// their position; auto's expansion fills in the rest in canonical order). No "auto"
// token ⇒ tokens returned unchanged (byte-identical to the pre-expansion list). An
// empty avail under auto contributes nothing, so e.g. `-backends auto` on a
// native-only host collapses to [native] ⇒ the single path.
func expandAutoBackends(tokens []string, cfg onboard.Config, cred func(string) string) []string {
	hasAuto := false
	for _, t := range tokens {
		if t == "auto" {
			hasAuto = true
			break
		}
	}
	if !hasAuto {
		return tokens
	}
	avail := availableBackends(cfg, cred)
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, t := range tokens {
		if t == "auto" {
			for _, a := range avail {
				add(a)
			}
			continue
		}
		add(t)
	}
	return out
}

// canonicalBackendOrder is the baseline order availableBackends emits before any
// preference or Trust-Ledger reordering: native (the always-considered baseline),
// then the delegated CLIs. It is the deterministic floor the auto selector builds
// on, so an empty ledger with no preference yields native first (today's default).
var canonicalBackendOrder = []string{"native", "codex", "claude-code"}

// availableBackends reports which of {native, codex, claude-code} are actually
// USABLE on this host, in canonical order. A backend is available when both its
// tool and its credential are present, so the auto selector never picks a backend
// that would fatal at resolve time:
//
//   - native:      the RESOLVED model spec's provider key resolvable via cred
//     (env-first, then SecretStore). The spec is modelSpec(NILCORE_MODEL,
//     cfg.Executor) — exactly what resolveProvider("native") uses — so it
//     honors the NILCORE_MODEL override and the built-in default model, not
//     just a configured executor. native is usable iff that resolves; a host
//     with no provider key genuinely has no usable native backend.
//   - codex:       the `codex` CLI on PATH AND CODEX_API_KEY resolvable.
//   - claude-code: the `claude` CLI on PATH AND ANTHROPIC_API_KEY resolvable.
//
// cred is the SecretStore-backed resolver (b.cred): it returns "" when neither the
// environment nor the store holds the key, and never logs or exposes a value (I3).
// The result may be empty (nothing configured); the caller decides whether that is
// fatal. Availability is config DATA derived from presence checks — no key flows
// through the result.
func availableBackends(cfg onboard.Config, cred func(string) string) []string {
	var out []string
	for _, name := range canonicalBackendOrder {
		switch name {
		case "native":
			// Mirror resolveProvider("native") EXACTLY: resolve the same spec it will
			// (NILCORE_MODEL → cfg.Executor → built-in default) and gate on that spec's
			// provider key. Otherwise auto would wrongly exclude native on a host that
			// sets NILCORE_MODEL (or relies on the default model) without a config
			// executor — a backend `nilcore run` resolves fine.
			spec := modelSpec(os.Getenv("NILCORE_MODEL"), cfg.Executor)
			if spec != "" && cred(providerEnv(vendorOf(spec))) != "" {
				out = append(out, name)
			}
		case "codex":
			if onboard.OnPath("codex") && cred("CODEX_API_KEY") != "" {
				out = append(out, name)
			}
		case "claude-code":
			if onboard.OnPath("claude") && cred("ANTHROPIC_API_KEY") != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// preferredBackendName resolves the preference that seeds the auto cold-start
// order: the one-run -prefer-backend flag if set, else the durable
// config.preferred_backend, else "native". The -prefer-backend flag is validated
// HERE — the single chokepoint every auto path (run / serve / the
// buildRunOrchestrator commands) flows through — so a typo fails loudly and
// uniformly rather than being silently dropped by orderPreferredFirst. The config
// field is validated by onboard.Validate at load.
func preferredBackendName(c commonFlags, cfg onboard.Config) string {
	if p := strings.TrimSpace(*c.preferBackend); p != "" {
		validateConcreteBackendFlag("-prefer-backend", p)
		return p
	}
	if cfg.PreferredBackend != "" {
		return cfg.PreferredBackend
	}
	return "native"
}

// orderPreferredFirst returns avail with preferred moved to the front (preserving
// the relative order of the rest), IFF preferred is actually in avail. A preferred
// backend that is not available is ignored — preference only reorders what the host
// can run, it never conjures an unavailable backend. The input slice is not mutated.
func orderPreferredFirst(avail []string, preferred string) []string {
	idx := -1
	for i, n := range avail {
		if n == preferred {
			idx = i
			break
		}
	}
	if idx <= 0 {
		// Not present, or already first ⇒ nothing to do (and a defensive copy is
		// unnecessary because the caller treats the result as read-only).
		return avail
	}
	out := make([]string, 0, len(avail))
	out = append(out, avail[idx])
	out = append(out, avail[:idx]...)
	out = append(out, avail[idx+1:]...)
	return out
}

// resolveAutoBackend turns `-backend auto` into a single concrete backend name for
// the rest of runMain (resolveProvider, envFactory) to consume UNCHANGED. It is the
// system's "pick the best available backend" decision:
//
//  1. compute availableBackends(host) — if empty, FATAL with init guidance (mirrors
//     resolveProvider's missing-key remedy), because there is nothing to run.
//  2. seed a cold-start order: put the preferred backend (flag > config > native)
//     FIRST among the available set, so a fresh install honors the operator's stated
//     preference before any evidence exists.
//  3. overlay verifier-judged evidence: trust.Replay(log) then Ledger.Order(avail).
//     Order ranks KNOWN backends best-first and PRESERVES the preferred-first order
//     among UNPROVEN ones, so "prefer X until evidence overtakes" falls out for free
//     (the ledger needs no change). A broken-chain Replay error is LOGGED
//     (trust_replay_error) and we keep the preferred-first order — fail-soft, never
//     abort, exactly like wireMultiBackend.
//  4. return avail[0] — the chosen single backend.
//
// A metadata-only `backend_auto` event records the choice, the ordered candidates,
// and whether the ledger reordered them — names only, never a secret (I3). The
// ledger/preference only ORDER which backend runs; the verifier still judges (I2).
func resolveAutoBackend(c commonFlags, b boot, log *eventlog.Log) string {
	avail := availableBackends(b.cfg, b.cred)
	if len(avail) == 0 {
		fatal(fmt.Errorf("backend \"auto\": no usable backend on this host — configure an executor model + key (native) or install codex/claude with its API key; run `nilcore init` to set one up"))
	}
	preferred := preferredBackendName(c, b.cfg)
	ordered := orderPreferredFirst(avail, preferred)

	trustOrdered := false
	led, err := trust.Replay(*c.logPath)
	if err != nil {
		// Fail-soft on a broken chain: keep the preferred-first order rather than
		// aborting the run (mirrors wireMultiBackend's degrade-to-configured-order).
		fmt.Fprintf(os.Stderr, "trust: ledger unavailable (%v); using preferred-first backend order\n", err)
		log.Append(eventlog.Event{Kind: "trust_replay_error", Detail: map[string]any{"error": err.Error()}})
	} else {
		ordered = led.Order(ordered)
		trustOrdered = true
	}

	chosen := ordered[0]
	// Metadata-only audit of the auto decision: names + flags, never a secret (I3).
	log.Append(eventlog.Event{Kind: "backend_auto", Backend: chosen, Detail: map[string]any{
		"preferred":     preferred,
		"available":     avail,
		"candidates":    ordered,
		"trust_ordered": trustOrdered,
	}})
	return chosen
}

// validateConcreteBackendFlag fatals if the operator passed a -prefer-backend value
// that is not a concrete backend (native | codex | claude-code). It mirrors the
// onboard validation so a typo fails loudly at flag time rather than silently
// degrading the auto cold-start order. "auto" is rejected: a preference must name a
// real backend. Empty ⇒ no preference (valid).
func validateConcreteBackendFlag(name, value string) {
	v := strings.TrimSpace(value)
	if v == "" {
		return
	}
	if !knownBackendNames[v] {
		fatal(fmt.Errorf("unknown %s %q (want native | codex | claude-code)", name, v))
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
	validateConcreteBackendFlag("-prefer-backend", *c.preferBackend)

	absDir := mustAbs(*c.dir)
	mcpManager := setupMCP(absDir) // start MCP servers + generate wrappers (if configured)
	defer mcpClose(mcpManager)
	log := openLog(*c.logPath)
	defer log.Close()
	// Resolve `-backend auto` (or config backend:auto, which applyConfigDefaults maps
	// onto *c.backendName) to a single concrete backend BEFORE anything reads the name.
	// The rest of runMain (resolveProvider, envFactory's capture of *c.backendName) is
	// unchanged and sees a concrete name. No "auto" ⇒ this is a no-op (byte-identical).
	if *c.backendName == "auto" {
		*c.backendName = resolveAutoBackend(c, b, log)
	}
	prov, err := resolveProvider(*c.backendName, b)
	if err != nil {
		fatal(err)
	}
	mem, cp, _ := setupPersistence(log, *c.logPath)

	// One shared blast-radius budget for the whole run: the SAME meter fences the
	// sandbox wall-time + egress hosts (BR-T02/T03) and the auto-approval $/rate/
	// irreversible axes (BR-T04). nil when -blast-radius is off (the default) ⇒ unfenced,
	// byte-identical.
	blast := mintBlastBudget(*c.blastRadius, log)

	orch := &agent.Orchestrator{
		BaseRepo: absDir,
		NewEnv:   envFactory(c, prov, b.cred, resolveAdvisor(*c.backendName, b, c), log, mem, absDir, b.cfg, blast),
		Log:      log,
		Router:   agent.SingleRouter{},
		Spawner:  agent.NoSpawner{},
		// GAA-T07: graduated auto-approval on the run front door. With no operator
		// envelope configured (cfg.AutoApprove empty) wrapAutoApprove returns the console
		// approver UNCHANGED, so the default is byte-identical; an envelope lets an
		// earned-trust boundary auto-proceed within the shared blast-radius fence.
		Approver:   wrapAutoApprove(policy.NewConsoleApprover(os.Stdin, os.Stdout), b.cfg, absDir, *c.logPath, log, blast),
		RaceN:      *c.raceN,
		OnSuccess:  memWriteBack(mem, absDir),
		Checkpoint: cp,
	}
	// Phase 13: when -backends names two or more backends, activate strength-routing
	// (Backends/NewEnvFor/Selector). EMPTY/single ⇒ no-op (byte-identical single path).
	wireMultiBackend(orch, c, b, log, mem, absDir, blast)
	// Phase 13: when -auto-supervise is set, wire the optional supervised seam so a
	// complex goal opportunistically scales up to the bounded project loop. DEFAULT
	// OFF ⇒ Project/ShouldSupervise stay nil ⇒ Execute is byte-identical single-task.
	cleanup := wireAutoSupervise(orch, c, b, prov, log, *goal)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	out, err := runViaKernel(ctx, orch, backend.Task{ID: fmt.Sprintf("t-%d", time.Now().Unix()), Goal: *goal})
	if err != nil {
		fatal(err)
	}
	fmt.Printf("\nbackend:  %s\nverified: %v\nsummary:  %s\n", out.Backend, out.Verified, out.Summary)
	if !out.Verified {
		fmt.Printf("\nchecks did not pass:\n%s\n", out.Detail)
		os.Exit(1)
	}
}

// wireAutoSupervise is the optional `-auto-supervise` seam for `nilcore run`: when
// the flag is set, it builds the SAME bounded project loop `nilcore build`
// constructs (the build caps verbatim — max-iterations 12, max-fanout 8, max-agents
// 64, max-depth 1, concurrency 1, budget 25, max-steps 80) and wires it onto the
// orchestrator's Project/ShouldSupervise seam, so a goal the model sizes "big
// enough to decompose" opportunistically scales up to the supervised fan-out
// instead of running as a single task. It adds NO new authority: the supervisor
// caps, the budget wall, and the verifier (I2) are the build path's rails verbatim;
// the single human promote still gates the only irreversible step.
//
// When -auto-supervise is FALSE (the default) it is a no-op — Project/ShouldSupervise
// stay nil, so Execute is byte-identical to today's single-task run. It returns a
// cleanup func the caller defers (a no-op when off, the build stack's read-worktree
// teardown when on).
//
// The ShouldSupervise trigger is MODEL-DRIVEN when a native model.Provider is
// available (prov != nil): it reuses the session classifier (the same router Part 1
// made authoritative) so run's sizing is consistent with chat/serve — the model
// proposes, and a supervise/project proposal triggers the scale-up. With no provider
// (a delegated codex/claude-code backend) or on a classifier error it falls back to
// the cheap chatShouldSupervise heuristic, so the capability is gained either way.
func wireAutoSupervise(o *agent.Orchestrator, c commonFlags, b boot, prov model.Provider, log *eventlog.Log, goal string) func() {
	noop := func() {}
	if c.autoSupervise == nil || !*c.autoSupervise {
		return noop // default off: byte-identical single-task run
	}

	// Size the goal ONCE, up front (model-driven when possible), so the potentially
	// expensive stack — incl. a greenfield bootstrap — is only constructed when the
	// goal actually warrants the scale-up. A simple goal stays the single-task path
	// with no stack built. This mirrors the orchestrator's "ShouldSupervise gates the
	// Project loop" contract: here the gate is evaluated before construction.
	if !autoSuperviseTrigger(prov, log)(goal) {
		return noop // sized simple: single-task run (Project/ShouldSupervise stay nil)
	}

	// Resolve the executor (cheap) and strong (advisor) tiers exactly as buildMain
	// does, so the supervised seam runs the identical stack. The supervised workers
	// are native, but the seam is an ENHANCEMENT, never required: on a delegated
	// backend (-backend codex/claude-code) with no native model+key, skip the
	// scale-up and let the single-task run proceed rather than aborting a working
	// run mid-flight the moment a goal happens to size complex.
	exec, err := resolveProvider("native", b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auto-supervise: no native executor available (%v); continuing as a single-task run\n", err)
		log.Append(eventlog.Event{Kind: "auto_supervise_skipped", Detail: map[string]any{"reason": err.Error()}})
		return noop
	}
	strong := resolveAdvisor("native", b, c).prov
	if strong == nil {
		strong = exec // no advisor configured: reuse the executor as the strong tier
	}

	// The build caps VERBATIM (the same construction `nilcore build` uses), so the
	// scaled-up run is bounded identically — no new rails. One shared blast budget
	// fences the gate + the loop's sandboxes (nil when off ⇒ unfenced).
	asBlast := mintBlastBudget(*c.blastRadius, log)
	stack, err := buildStack(buildDeps{
		goal:        goal, // the project loop bakes this goal; matches the chat supervised path
		dir:         o.BaseRepo,
		runtime:     *c.runtime,
		image:       *c.image,
		sandboxPref: *c.sandboxPref,
		verify:      *c.checkCmd,
		maxIter:     12,
		maxFan:      8,
		maxAgent:    64,
		maxDepth:    1,
		concurrency: 1,
		maxSteps:    80,
		budget:      25.00,
		executor:    exec,
		strong:      strong,
		log:         log,
		logPath:     *c.logPath,
		blast:       asBlast,
		// GAA-T07: the -auto-supervise scale-up honors the same auto-approval envelope as
		// `nilcore build`; default-off (no envelope) ⇒ the console approver unchanged. The
		// SAME blast meter fences the gate + the loop's sandboxes (BR-T04).
		approver: wrapAutoApprove(policy.NewConsoleApprover(os.Stdin, os.Stdout), b.cfg, o.BaseRepo, *c.logPath, log, asBlast),
	})
	if err != nil {
		fatal(err)
	}

	o.Project = stack.loop
	// The goal was already sized supervise above; the orchestrator re-checks
	// ShouldSupervise(t.Goal) before running the loop, so hand it a constant-true
	// predicate (a second classifier call would be redundant and re-bill).
	o.ShouldSupervise = func(string) bool { return true }
	return stack.cleanup
}

// autoSuperviseTrigger builds the ShouldSupervise predicate for an -auto-supervise
// run. When a native provider is available it is MODEL-DRIVEN — it reuses the
// session SupervisorFirstRouter (Part 1's now-authoritative classifier) to size the
// goal, so a supervise/project route triggers the scale-up and run's trigger matches
// chat/serve. The classifier is NOT metered here (a single one-shot sizing call on a
// run with no conversation ledger); the supervised loop it gates is itself budget-
// walled. A classifier error or a non-work route falls back to chatShouldSupervise,
// and with no provider (codex/claude-code) the heuristic is the trigger outright — so
// the supervised capability is gained on every backend.
func autoSuperviseTrigger(prov model.Provider, log *eventlog.Log) func(goal string) bool {
	if prov == nil {
		return chatShouldSupervise // delegated backend: no model to classify; heuristic trigger
	}
	router := &session.SupervisorFirstRouter{
		Classifier:      prov,
		ShouldSupervise: chatShouldSupervise, // the router's own unparseable/no-model fallback
		Log:             log,
		ID:              "auto-supervise",
	}
	return func(goal string) bool {
		route, err := router.Route(context.Background(), goal, session.WorkState{})
		if err != nil {
			// Classifier transport fault: degrade to the cheap heuristic rather than
			// failing the run — the supervised seam is an enhancement, never required.
			return chatShouldSupervise(goal)
		}
		return route == session.RouteSupervise || route == session.RouteProject
	}
}

// buildRunOrchestrator constructs the single-task orchestrator the run-style
// commands share (run / propose-edit / watch): the native backend via envFactory,
// the console gate, persistence, and memory write-back. It mirrors runMain's wiring
// so a self-started or self-edit task runs through the EXACT same verified path —
// the verifier remains the sole authority on done (I2), the gate on irreversible
// actions (I3/policy).
func buildRunOrchestrator(c commonFlags, b boot, log *eventlog.Log, absDir string, blast *blastbudget.Budget) *agent.Orchestrator {
	// Open the persistence backbone, then build the orchestrator over it. A one-shot
	// command (propose-edit / watch / scheduler / webhook / `nilcore flywheel`) owns its
	// own store for the process lifetime. A long-running serve must NOT call this for its
	// folds — it would open a SECOND single-writer handle to the same DB; serve uses
	// buildRunOrchestratorWith with the store it already opened (see serveMain).
	mem, cp, _ := setupPersistence(log, *c.logPath)
	return buildRunOrchestratorWith(c, b, log, absDir, blast, mem, cp)
}

// buildRunOrchestratorWith builds the single-task run orchestrator over an
// ALREADY-OPENED memory + checkpointer, so a process that has already set up persistence
// (serve) can share its one *sql.DB rather than opening competing handles. It mirrors
// runMain's wiring: the native backend via envFactory, the console gate, memory
// write-back, and the optional multi-backend / blast seams. The verifier stays the sole
// authority on done (I2); the gate governs irreversible actions (I3).
func buildRunOrchestratorWith(c commonFlags, b boot, log *eventlog.Log, absDir string, blast *blastbudget.Budget, mem *memory.Memory, cp *agent.Checkpoint) *agent.Orchestrator {
	// Resolve `-backend auto` / config backend:auto to a concrete name so the
	// self-started run paths honor the same system-selection seam as `nilcore run`.
	if *c.backendName == "auto" {
		*c.backendName = resolveAutoBackend(c, b, log)
	}
	prov, err := resolveProvider(*c.backendName, b)
	if err != nil {
		fatal(err)
	}
	orch := &agent.Orchestrator{
		BaseRepo:   absDir,
		NewEnv:     envFactory(c, prov, b.cred, resolveAdvisor(*c.backendName, b, c), log, mem, absDir, b.cfg, blast),
		Log:        log,
		Router:     agent.SingleRouter{},
		Spawner:    agent.NoSpawner{},
		Approver:   policy.NewConsoleApprover(os.Stdin, os.Stdout),
		RaceN:      *c.raceN,
		OnSuccess:  memWriteBack(mem, absDir),
		Checkpoint: cp,
	}
	// Phase 13: -backends (two or more) activates strength-routing for the run-style
	// commands (propose-edit / watch / self-improve / schedule). EMPTY/single ⇒ no-op.
	wireMultiBackend(orch, c, b, log, mem, absDir, blast)
	// P16 closed-loop self-acceptance (opt-in, NILCORE_SELFACC): after the floor
	// verifier passes, the agent's own gated acceptance checks must ALSO pass. nil when
	// off ⇒ byte-identical. Captures the run's model provider for authoring.
	orch.SelfAccept = selfAcceptHook(prov)
	return orch
}

// makeHeadlessBackground configures a BACKGROUND orchestrator (autonomy daemon, serve-
// embedded flywheel) for unattended operation. buildRunOrchestratorWith defaults to the
// attended ConsoleApprover (os.Stdin) — fatal for a background goroutine, which would
// block forever on a gate prompt no human can answer. This deny-defaults the gate (with
// graduated auto-approval WHEN an operator envelope is configured) and DISABLES self-
// acceptance when there is NO envelope: a headless run with no envelope can never
// approve an agent-authored check, so authoring it would be wasted model cost.
func makeHeadlessBackground(orch *agent.Orchestrator, cfg onboard.Config, logPath string, log *eventlog.Log, blast *blastbudget.Budget) {
	orch.Approver = wrapAutoApprove(denyAllApprover{}, cfg, orch.BaseRepo, logPath, log, blast)
	if cfg.AutoApprove.Empty() {
		orch.SelfAccept = nil
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
	maxLifetime := fs.Duration("max-lifetime", 0, "after this wall-clock duration, serve checkpoints in-flight work and exits cleanly so a supervisor (systemd) restarts it into the resume path (0 = no cap)")
	webhookAddr := fs.String("webhook", "", "address for the SCM/CI webhook intake (e.g. 127.0.0.1:8099); needs NILCORE_WEBHOOK_SECRET")
	autonomySignals := fs.String("autonomy-signals", "", "directory the autonomy daemon (NILCORE_AUTONOMY) watches for dropped goal files, folded into the unified self-start queue alongside standing objectives + due wakes (empty = no file funnel)")
	egressProfile := fs.String("egress-profile", "", "opt into a named research egress preset (finance|docs|web-research) that WIDENS the sandbox allowlist to a sanctioned host set; empty = default-deny. Overrides NILCORE_EGRESS_PROFILE and the persisted config.")
	c := registerCommon(fs)
	_ = fs.Parse(args)

	b := loadBoot(*c.config)
	applyConfigDefaults(c, b.cfg, flagsSet(fs))

	absDir := mustAbs(*c.dir)
	mcpManager := setupMCP(absDir) // start MCP servers + generate wrappers (if configured)
	defer mcpClose(mcpManager)
	// Rotate an over-large event log BEFORE opening the handle, so the fresh handle
	// appends to the rotated (or recreated) path rather than a renamed inode.
	_ = maint.RotateLog(*c.logPath, serveLogMaxBytes)
	log := openLog(*c.logPath)
	defer log.Close()
	// Persistence backbone (best-effort): durable event-log mirroring + cross-project
	// memory (the opt-in live tool) + the checkpointer that gives serve threads
	// conversation persistence and leftover-task resume across a restart.
	mem, ckpt, serveStore := setupPersistence(log, *c.logPath)
	// serve is long-running and owns the shared *sql.DB for its whole lifetime; close it
	// on shutdown so the WAL is checkpointed and the handle released cleanly (the daemon
	// folds + objective backlog share this one handle, so it is closed only here, after
	// the serve loop returns and those goroutines have drained on ctx cancel).
	if serveStore != nil {
		defer serveStore.Close()
	}
	// Reclaim worktree admin entries left by a crashed prior process. SAFE: only
	// worktrees whose directory is already gone are candidates (a live run's
	// worktree directory is present), so this never collects an active worktree.
	serveGC(context.Background(), absDir, log)
	validateConcreteBackendFlag("-prefer-backend", *c.preferBackend)
	// Resolve `-backend auto` / config backend:auto to a concrete name before serve
	// reads it. serve still requires native below; auto simply lets the system pick
	// the best available backend, and a non-native pick surfaces the same clear
	// "serve requires native" remedy rather than a bare "unknown backend".
	if *c.backendName == "auto" {
		*c.backendName = resolveAutoBackend(c, b, log)
	}
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
	// Optional wall-clock lifetime cap: after -max-lifetime, the serve ctx expires,
	// Serve returns, and the SAME clean-shutdown path below checkpoints in-flight work
	// (ckpt.Interrupt) — so a supervisor (systemd Restart=always) brings it back into
	// the resume path on a schedule (a bounded-lifetime self-restart for very long
	// unattended operation). 0 ⇒ no cap (byte-identical: runs until a signal).
	if *maxLifetime > 0 {
		var cancelLifetime context.CancelFunc
		ctx, cancelLifetime = context.WithTimeout(ctx, *maxLifetime)
		defer cancelLifetime()
		fmt.Fprintf(os.Stderr, "nilcore serve: max-lifetime %s (will checkpoint + exit for a supervised restart)\n", *maxLifetime)
	}

	// Periodic maintenance: a long-lived serve accumulates worktree admin entries from
	// crashed/abandoned drives between restarts, so re-run the crash-safe worktree GC
	// on a background ticker (not just at startup). Bounded by the serve ctx — it exits
	// on shutdown. (Log rotation stays a startup-only step: the event-log handle is
	// already open here, so rotating its path mid-serve would not affect the live
	// inode; rotation-while-open needs eventlog support and is out of scope.)
	go runMaintenanceTicker(ctx, absDir, log)

	// SIF-T08: the optional self-improvement flywheel as a bounded serve-background
	// cadence. DEFAULT-OFF — only when NILCORE_FLYWHEEL is set. The orchestrator is
	// built HERE (at startup) so a missing model key fails loudly at boot rather than
	// inside the goroutine; each tick runs one bounded cycle (verifier + gate own every
	// ship — I2). It never edits the verifier of record (selfimprove.DefaultScope).
	if os.Getenv("NILCORE_FLYWHEEL") != "" {
		// Reuse serve's already-opened persistence (mem/ckpt) — never re-open the store
		// (one *sql.DB for the whole serve process; no competing single-writer handles).
		fwBlast := mintBlastBudget(*c.blastRadius, log)
		fwOrch := buildRunOrchestratorWith(c, b, log, absDir, fwBlast, mem, ckpt)
		// The flywheel ticks in a background goroutine — it must never gate against
		// os.Stdin (no human attends it). Headless approver + envelope-gated self-accept.
		makeHeadlessBackground(fwOrch, b.cfg, *c.logPath, log, fwBlast)
		fwLoop := newFlywheelLoop(fwOrch, log, *c.logPath, 1, time.Minute)
		go runFlywheelTicker(ctx, fwLoop)
	}

	// One shared concurrency gate caps how many drives run at once across ALL
	// threads, so a burst of conversations queues rather than overrunning the host's
	// sandbox/model capacity. Drained and stopped after the serve loop returns.
	gate := newDriveGate(ctx, *maxConcurrent)
	defer gate.close()

	// Web access (parity with chat): resolved from config, with the search backend's
	// host auto-allowlisted. The proxy is bound to the serve ctx (one proxy for the
	// server). Default-deny when web is not configured.
	searchKey := b.cred(searchKeyEnv)
	// Pillar-5 research egress profile (P11-T28): same flag/env/config resolution as
	// the chat front door, fail-closed on an unknown name / unparseable file. The
	// widen-tree hosts are the BASE of the serve allowlist; the audited widen is
	// recorded as one metadata-only event. Nothing opted in ⇒ byte-identical.
	prof, perr := resolveEgressProfile(b.cfg, *egressProfile)
	if perr != nil {
		fatal(perr)
	}
	emitEgressProfile(log, prof, egressBackendLabel(*c.sandboxPref))
	warnNamespaceEgress(prof, *c.sandboxPref)
	webAllow, searchBackend := resolveWeb(b.cfg, prof.Tree.Allowed, "", searchKey)
	// One shared blast-radius budget for the whole serve PROCESS: the SAME meter fences
	// the egress hosts (BR-T02), the per-thread sandbox wall-time (BR-T03), and the
	// auto-approval $/rate/irreversible axes (BR-T04/GAA-T07). Minted once here, threaded
	// to the egress proxy + serveDeps. nil when -blast-radius is off ⇒ unfenced.
	serveBlast := mintBlastBudget(*c.blastRadius, log)
	egress, proxyAddr, stopProxy, _ := startEgressProxy(ctx, webAllow, serveBlast, proxyBindAddr(*c.sandboxPref, *c.runtime))
	defer stopProxy()
	if egress.Empty() {
		fmt.Fprintln(os.Stderr, "nilcore serve: web access off (default-deny)")
	} else {
		fmt.Fprintf(os.Stderr, "nilcore serve: web access on — search: %s, %d allowed host(s)\n", searchBackend, len(webAllow))
	}

	// The self-timer registry behind the `sleep` tool — durable over the checkpointer's
	// store, so wakes survive a restart (re-fired by the waker on next boot). Off
	// without a checkpointer (nil ⇒ no `sleep` tool, no waker).
	var wakeReg *wake.Registry
	if ckpt != nil {
		wakeReg = wake.New(ckpt, log)
	}

	d := serveDeps{
		flags:           c,
		provider:        prov,
		boot:            b,
		log:             log,
		baseRepo:        absDir,
		budget:          *budgetCeil,
		gate:            gate,
		blast:           serveBlast, // one shared fence for the whole serve process (nil when off)
		mem:             mem,
		checkpoint:      ckpt,
		wakeReg:         wakeReg,
		egress:          egress,
		egressProxyAddr: proxyAddr,
		searchBackend:   searchBackend,
		searchKey:       searchKey,
		egressTree:      prof.Tree, // Pillar-5 widen-tree; empty ⇒ build stays deny-all (P11-T28)
	}
	factory := serveSessionFactory(d, rawCh, ctx)

	// AUTO-T06: the autonomy daemon self-services the operator objective backlog when
	// idle. DEFAULT-OFF — only when NILCORE_AUTONOMY is set. The orchestrator is built
	// HERE (at startup) so a missing model key fails loudly at boot rather than in the
	// goroutine; its gate is HEADLESS (deny irreversible by default, auto only for an
	// earned boundary inside the operator envelope + the shared blast fence — I3). The
	// objective CRUD is operator-only (XC-T06); the daemon only RUNS what an objective
	// names. An empty backlog emits nothing, so this is inert until objectives exist.
	autonomyOwnsWakes := false
	if os.Getenv("NILCORE_AUTONOMY") != "" && serveStore != nil {
		// Reuse serve's already-opened persistence (mem/ckpt + serveStore) — the daemon
		// shares the one *sql.DB for both its orchestrator and the objective backlog, so
		// it never opens a competing single-writer handle to the same file.
		autoOrch := buildRunOrchestratorWith(c, b, log, absDir, d.blast, mem, ckpt)
		makeHeadlessBackground(autoOrch, b.cfg, *c.logPath, log, d.blast)
		// The daemon drains the unified queue: standing objectives + dropped file signals
		// (-autonomy-signals) + due durable wakes — all through the verified,
		// headless-gated orchestrator. The server ALSO has its own runWaker over the same
		// registry, so under autonomy we suppress it (below) and let the gated daemon own
		// wakes: a wake must not bypass the headless gate via the server's direct re-Turn.
		go runAutonomyDaemon(ctx, autoOrch, log, serveStore, gate.idle, wakeReg, *autonomySignals)
		autonomyOwnsWakes = wakeReg != nil
	}

	// Durable resume: re-drive any native task a prior process left running or
	// interrupted, BEFORE accepting new traffic. Each runs in a fresh disposable
	// worktree off the current HEAD (idempotent — no committed base state until a
	// gated merge). On an irreversible gate the resume approver INFORMS the owner's
	// thread over the channel then denies (escalate-on-gate); irreversible actions
	// are never auto-approved on a headless resume (I3).
	if ckpt != nil {
		resumeInflight(ctx, d, rawCh)
	}

	// SCM/CI webhook intake (P9-T04), opt-in via --webhook: a signed GitHub webhook
	// becomes a trigger.Signal on the same gated machinery, bounded by the serve ctx.
	if *webhookAddr != "" {
		startWebhookListener(ctx, *webhookAddr, c, b, log, absDir, b.cred("NILCORE_WEBHOOK_SECRET"))
	}

	srv := &server.Server{Channel: rawCh, Auth: auth, NewSession: factory, Log: log, ResolveRoot: resolveReadRoot, Wake: wakeReg, SuppressWaker: autonomyOwnsWakes}
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

// serveMaintInterval is how often a running serve re-runs the worktree GC so a
// multi-day session self-prunes instead of accumulating crashed-drive worktrees
// between restarts. Hourly is far more often than worktrees leak, and the GC itself
// only ever reclaims already-gone directories, so it is cheap and crash-safe.
const serveMaintInterval = time.Hour

// runMaintenanceTicker re-runs serveGC on serveMaintInterval until ctx is cancelled.
// It is a background goroutine started by serveMain; it owns nothing the request path
// touches (the GC reads/prunes only crashed worktrees), so it never contends with a
// live drive. Exits cleanly on shutdown.
func runMaintenanceTicker(ctx context.Context, baseRepo string, log *eventlog.Log) {
	t := time.NewTicker(serveMaintInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			serveGC(ctx, baseRepo, log)
		}
	}
}

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

// informGateApprover is the resume-path approver when a transport is connected: a
// headless resumed task still cannot safely BLOCK on a live gate during pre-traffic
// resume (the server receive loop is not running, so a polling Ask would steal
// inbound user messages), so on an irreversible gate it INFORMS the owner's thread
// (send-only Update — no poll) of exactly what is blocking, then DENIES (the task
// does its reversible work and stops, as before). The owner re-issues the task when
// attached, where the live channel Ask resolves the gate normally. It NEVER
// auto-approves (I3); a missing owner thread or a failed push degrades to a silent
// deny, identical to denyAllApprover.
type informGateApprover struct {
	ch     channel.Channel
	ctx    context.Context
	taskID string
	log    *eventlog.Log
}

func (a informGateApprover) Approve(action string) bool {
	thread := ownerThreadFromTaskID(a.taskID)
	informed := false
	if a.ch != nil && thread != "" {
		nctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
		if err := a.ch.Update(nctx, thread,
			"GATE — a resumed task stopped at an irreversible step:\n"+action+
				"\nRe-issue this task when you're ready to approve it."); err == nil {
			informed = true
		}
		cancel()
	}
	if a.log != nil {
		a.log.Append(eventlog.Event{Kind: "resume_gate_denied",
			Detail: map[string]any{"task": a.taskID, "informed": informed}})
	}
	return false // never auto-approve a resumed gate (I3)
}

// ownerThreadFromTaskID recovers the owning channel thread from a serve native task
// id, which the native driver mints as "<threadID>-<seq>". It returns the threadID
// prefix when the trailing segment after the last '-' is all digits (the seq), else
// "" (so the caller degrades to a silent deny rather than push to a wrong thread).
func ownerThreadFromTaskID(id string) string {
	i := strings.LastIndexByte(id, '-')
	if i <= 0 || i == len(id)-1 {
		return ""
	}
	for j := i + 1; j < len(id); j++ {
		if id[j] < '0' || id[j] > '9' {
			return ""
		}
	}
	return id[:i]
}

// resumeInflight re-drives every task a prior process left in flight, before serve
// accepts new traffic. It runs TWO passes over the durable store, partitioned by
// status so neither re-drives the other's rows:
//
//   - Native tasks (running/interrupted): each runs through a freshly reconstructed
//     single-task orchestrator (the identical verified path as a live serve drive,
//     minus the conversational seams) in a disposable worktree, via Checkpoint.Resume.
//   - Multi-agent runs (SuperviseStatus): each is replayed by resumeSupervise — the
//     preserved integration tip is re-verified (I2) and the supervisor stack is rebuilt
//     rooted at it + seeded with the prior dispositions, so the run continues from the
//     merged work instead of redoing it.
//
// On an irreversible gate the approver informs the owner's thread then denies (a real
// Ask can't run safely here — the receive loop is not up yet); with no transport it is
// a silent deny. Each task is resumed at most once per boot (terminal status recorded).
func resumeInflight(ctx context.Context, d serveDeps, notifyCh channel.Channel) {
	resumeSupervise(ctx, d, notifyCh)
	c := d.flags
	adv := resolveAdvisor(*c.backendName, d.boot, c)
	run := func(ctx context.Context, t backend.Task) (bool, error) {
		newEnv := func(dir string) agent.Env {
			box := selectSandbox(*c.sandboxPref, *c.runtime, *c.image, dir)
			v := behavioralVerifier(box, *c.checkCmd)
			be := buildBackend("native", d.provider, d.boot.cred, adv, box, v, d.log, *c.maxSteps, d.mem, d.baseRepo, d.boot.cfg)
			return agent.Env{Backend: be, Verifier: v}
		}
		// A resumed gate INFORMS the owner's thread then denies (escalate-on-gate); with
		// no transport it stays a silent deny (I3). Never auto-approves either way.
		var approver policy.Approver = denyAllApprover{}
		if notifyCh != nil {
			approver = informGateApprover{ch: notifyCh, ctx: ctx, taskID: t.ID, log: d.log}
		}
		orch := &agent.Orchestrator{
			BaseRepo:   d.baseRepo,
			NewEnv:     newEnv,
			Log:        d.log,
			Router:     agent.SingleRouter{},
			Spawner:    agent.NoSpawner{},
			Approver:   approver,
			Checkpoint: d.checkpoint,
		}
		out, err := runViaKernel(ctx, orch, t)
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
	flags    commonFlags
	provider model.Provider
	boot     boot
	log      *eventlog.Log
	baseRepo string
	budget   float64
	gate     *driveGate // shared serve drive-concurrency cap
	// blast is the serve process's SHARED blast-radius budget (one envelope for the
	// whole process, so N threads cannot each spend the per-day ceiling). Minted from
	// -blast-radius; nil ("off", the default) ⇒ unfenced, byte-identical (GAA-T07/BR-T04).
	blast      *blastbudget.Budget
	mem        *memory.Memory    // cross-project memory; feeds the opt-in NILCORE_LIVE_INDEX live tool (nil ⇒ none)
	checkpoint *agent.Checkpoint // durable task/conversation persistence (nil ⇒ in-memory only, no resume)
	wakeReg    *wake.Registry    // durable self-timer registry behind the `sleep` tool (nil ⇒ sleep off)

	// Web access, resolved once from config (nilcore init) and applied to every
	// thread's drives — parity with the chat front door. egress empty ⇒ default-deny.
	egress          policy.Egress
	egressProxyAddr string
	searchBackend   tools.SearchBackend
	searchKey       string

	// egressTree is the Pillar-5 research-egress widen-tree (P11-T28): the resolved
	// named-preset (+ project-file) host set, or empty when no profile is opted in.
	// It feeds buildDeps.egress so a supervised/project drive's researcher role can
	// intersect against the sanctioned hosts (a deny-all role stays --network none).
	egressTree policy.Egress
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
// formatNotification renders a work drive's terminal outcome as a one-line status
// push to a detached principal's channel thread (notify-on-terminal). Bounded so a
// sprawling summary can't blow the channel's message limits.
func formatNotification(n session.Notification) string {
	var b strings.Builder
	switch {
	case n.Failed:
		b.WriteString("❌ Run errored.")
	case n.Verified:
		b.WriteString("✅ Done (verified).")
	default:
		b.WriteString("⚠️ Stopped — not verified; may need your input.")
	}
	if s := strings.TrimSpace(n.Summary); s != "" {
		// clipRunes (not byte-truncate): the summary is model-authored prose that is
		// often multibyte UTF-8, and a mid-rune cut could emit invalid bytes that a
		// strict transport (Telegram) rejects, dropping the whole notification.
		b.WriteString(" " + clipRunes(s, 600))
	}
	if n.Branch != "" {
		b.WriteString("\nbranch: " + n.Branch)
	}
	return b.String()
}

func serveSessionFactory(d serveDeps, notifyCh channel.Channel, baseCtx context.Context) server.SessionFactory {
	return func(ctx context.Context, threadID, sender string, out emit.Emitter, approver policy.Approver) *session.Session {
		// GAA-T07: graduated auto-approval on the serve surface. The per-thread channel
		// approver (built upstream in server.threadFor) is wrapped here so an earned-trust
		// boundary may auto-proceed within the operator envelope + the shared blast fence;
		// fall-through still routes the gate question back over the channel. Default-off
		// (no envelope, or a nil channel approver on a headless path) ⇒ unchanged.
		approver = wrapAutoApprove(approver, d.boot.cfg, d.baseRepo, *d.flags.logPath, d.log, d.blast)
		// One conversation = one ledger + one global ceiling = one metered provider
		// keyed by the threadID. Routing, drives, chat replies, and the summarize
		// fold-back all charge this single wall (§6).
		ledger := budget.New()
		ledger.SetGlobalCeiling(d.budget)
		metered := &meter.Provider{Inner: d.provider, Ledger: ledger, Task: threadID, Price: meter.NewTable()}

		sess := session.New(threadID, sender, d.baseRepo, d.log)
		sess.Out = out // reasoning/intent streams back to this thread (Channel.Update)
		// ATTENDED over the channel: a live serve thread has a reachable, authorized
		// principal (the Receive loop is running and the sender is pinned + allowlisted),
		// so enable ask_user — the drive may pose a question, which streams out as a
		// channel message and is answered by the operator's next thread reply (routed by
		// intake → Turn → the ask box). The wall-clock backstop bounds an absent operator,
		// and `/questions off` lets them silence it. The headless durable-resume path
		// (resumeInflight) builds NO Session, so ask_user is structurally absent there.
		sess.EnableAskUser(out)
		// Terminal-outcome push so a DETACHED principal learns a work drive finished
		// without re-attaching. Uses the SERVE BASE ctx (not the per-request ctx, which
		// is dead once the principal detaches) with a short timeout, so the push fires
		// while serve is alive and never wedges the conversation. nil channel ⇒ no push.
		if notifyCh != nil {
			sess.Notify = func(n session.Notification) {
				nctx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
				defer cancel()
				_ = notifyCh.Update(nctx, threadID, formatNotification(n))
			}
		}
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
			Native:    session.NewNativeDriver(gateNative(d.gate, serveNativeRun(d, metered, approver, threadID, sender)), metered, threadID),
			Supervise: session.NewSuperviseDriver(gateSupervise(d.gate, serveSuperviseRun(d, ledger, approver, threadID)), metered),
			Project:   session.NewProjectDriver(gateProject(d.gate, serveProjectRun(d, ledger, approver, threadID)), metered),
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
func serveNativeRun(d serveDeps, metered model.Provider, approver policy.Approver, threadID, sender string) session.RunNativeFunc {
	adv := resolveAdvisor(*d.flags.backendName, d.boot, d.flags)
	// The per-thread self-timer arm: the `sleep` tool calls this to durably register a
	// wake for THIS conversation (threadID owned by sender). nil when no registry is
	// wired (no checkpointer) ⇒ no `sleep` tool advertised.
	var wakeArm func(context.Context, time.Duration, string) error
	if d.wakeReg != nil {
		wakeArm = func(c context.Context, after time.Duration, note string) error {
			_, err := d.wakeReg.Arm(c, threadID, sender, after, note)
			return err
		}
	}
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
			n := serveNativeBackend(d, metered, adv, box, v, in, wakeArm)
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
		out, err := runViaKernel(ctx, orch, backend.Task{ID: in.TaskID, Goal: modePreamble(in.Mode) + in.Goal})
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
func serveNativeBackend(d serveDeps, prov model.Provider, adv advisorCfg, box sandbox.Sandbox, v verify.Verifier, in session.NativeRun, wakeArm func(context.Context, time.Duration, string) error) *backend.Native {
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
		// web_search — EXACTLY ONE path (Phase 15 capability switch), at PARITY with chat.
		// Path A (provider-native server-side search) runs OUTSIDE the box, so it needs no
		// sandbox — register it regardless of the box type (gating it on a Container silently
		// gave a namespace/non-container box no web_search despite the opt-in).
		nativeWS := selectNativeWebSearch(modelSpec(os.Getenv("NILCORE_MODEL"), d.boot.cfg.Executor))
		if nativeWS != nil {
			reg.Register(nativeWS)
		}
		if _, ok := box.(*sandbox.Container); ok {
			reg.Register(tools.WebFetchTool{Box: box})
			// Path B: the sandboxed, egress-confined client tool — ONLY when Path A did not
			// already claim web_search (never both) and only on a container box (fetches in-box).
			if nativeWS == nil && d.searchBackend != tools.SearchOff && d.egress.Allow(tools.SearchHostFor(d.searchBackend)) {
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
	// Attended ask seam (ask_user / set_ask_level) over the channel: wired only when the
	// serve session enabled attended asking (a live thread). The session-owned adapter
	// parks the drive in AwaitingInput; the question streams out via the channel emitter
	// and the human's reply arrives as an ordinary authorized thread message, routed by
	// Session.Turn to the ask box (intake → Turn → Resolve). Headless resume builds its
	// backend without a Session, so this is nil there (the never-block guarantee).
	if in.AskUser != nil {
		n.AskUser = in.AskUser
	}
	// Same orientation + window seams as buildBackend's native case: serve/chat
	// drives start with the map and compact before overflow, exactly like run.
	n.RepoContext = func(context.Context) string { return repoMap(box.Workdir(), repoMapBudget) }
	n.CtxWindow = meter.CtxWindow
	if adv.prov != nil {
		n.Advisor = advisor.New(adv.prov, adv.maxCalls)
		n.EscalateAfter = adv.escalateAfter
	}
	// Live incremental code-intelligence (P3-T16), opt-in via NILCORE_LIVE_INDEX —
	// the serve loop gets the same `live` tool as the run/chat paths.
	if os.Getenv("NILCORE_LIVE_INDEX") != "" {
		n.LiveSession = liveSession(d.mem, d.baseRepo)
	}
	// Self-timer (serve-only): the `sleep` tool arms a durable wake for this thread.
	// nil ⇒ no `sleep` tool advertised (byte-identical) — e.g. no checkpointer wired.
	n.Wake = wakeArm
	return n
}

// serveSuperviseRun / serveProjectRun assemble the multi-agent stack for one serve
// drive via buildStack, pinning the thread's shared conversation ledger (§6) and the
// channel approver (the single human promote routes back over chat). Like the chat
// path, the planner's steer/queue Inbox and live Out are wired to this thread's seams,
// so a supervised serve drive streams its intent over the channel and folds queued
// turns in at round boundaries; the drive itself runs bounded, verifier-gated, and
// charged against the per-conversation wall, and its outcome folds back exactly like a
// native drive. (Seeding the planner with prior HISTORY/SUMMARY stays a deeper
// follow-on — super.Run takes only a goal — so those params are not yet consumed.)
func serveSuperviseRun(d serveDeps, ledger *budget.Ledger, approver policy.Approver, threadID string) session.RunSuperviseFunc {
	taskID := superviseTaskID(threadID)
	// The session gate (last param) is intentionally unused here: serve already passes
	// its CHANNEL approver (parks AwaitingGate and routes the prompt over the transport),
	// which is the proven path — there is no stdin to race, so AU-T05b's REPL fix does not
	// apply. Keeping serve on its channel approver leaves the serve gate byte-identical.
	return func(ctx context.Context, goal string, _ []model.Message, in session.InboxHandle, outEmitter emit.Emitter, ask session.AskerHandle, _ policy.Approver) (session.DriveOutcome, error) {
		stack, err := buildStack(serveBuildDeps(d, ledger, approver, goal, taskID))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		// Attended over the channel: a live serve thread can answer, so wire the
		// supervisor's ask_user to this thread's ask box (headless resume builds no
		// Session, so ask stays nil there).
		stack.sup.AskUser = superAskFunc(ask)
		// Steer/queue + live stream parity with the native serve loop.
		stack.sup.Inbox = in
		stack.sup.Out = outEmitter
		defer stack.cleanup() // tear down the supervisor's live read worktree per drive
		o, err := buildViaKernel(ctx, stack.loop)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		// Mark the durable supervise row terminal when the drive ran to its OWN
		// conclusion; a SIGTERM-interrupted drive (ctx cancelled) is left "supervise" so
		// the next boot's resumeSupervise replays it from the last checkpointed tip.
		finalizeSupervise(ctx, d, taskID, goal, o.Done)
		return session.DriveOutcome{Summary: o.Summary, Branch: o.Branch, Verified: o.Done}, nil
	}
}

func serveProjectRun(d serveDeps, ledger *budget.Ledger, approver policy.Approver, threadID string) session.RunProjectFunc {
	taskID := superviseTaskID(threadID)
	return func(ctx context.Context, goal string, _ summarize.ContextSummary, outEmitter emit.Emitter, _ policy.Approver) (session.DriveOutcome, error) {
		stack, err := buildStack(serveBuildDeps(d, ledger, approver, goal, taskID))
		if err != nil {
			return session.DriveOutcome{}, err
		}
		stack.sup.Out = outEmitter // stream the project planner's intent back over the channel
		defer stack.cleanup()      // tear down the supervisor's live read worktree per drive
		o, err := buildViaKernel(ctx, stack.loop)
		if err != nil {
			return session.DriveOutcome{}, err
		}
		finalizeSupervise(ctx, d, taskID, goal, o.Done)
		return session.DriveOutcome{Summary: o.Summary, Branch: o.Branch, Verified: o.Done}, nil
	}
}

// serveBuildDeps adapts the serve deps to a buildDeps for buildStack, pinning the
// shared conversation ledger (so the supervised/project drive charges the SAME
// per-conversation ceiling, §6) and the channel approver (the gate routes back over
// chat). It mirrors chatBuildDeps' interactive rail sizing.
func serveBuildDeps(d serveDeps, ledger *budget.Ledger, approver policy.Approver, goal, taskID string) buildDeps {
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
		logPath:  *d.flags.logPath,
		blast:    d.blast, // share the serve process's blast fence across the supervise/project sandboxes
		approver: approver,
		ledger:   ledger,       // pin the per-conversation wall (§6)
		egress:   d.egressTree, // Pillar-5 widen-tree; empty ⇒ build stays deny-all (P11-T28)
		// Durable resume: with a checkpointer + a stable task id, buildStack wires the
		// supervisor's SaveState (snapshot → pin resume/<taskID> → SuperviseStatus row).
		// taskID empty (no checkpointer) ⇒ no durable snapshot, byte-identical.
		checkpoint: d.checkpoint,
		taskID:     taskID,
	}
}

// resolveProvider builds the model provider for the native backend and validates
// modelCallTimeout is the hard per-call ceiling on a single model round-trip in the
// Resilient wrapper: generous enough that a legitimate large/slow completion never
// trips it, tight enough that a truly WEDGED call (a stalled connection that never
// returns) is cut and retried/failed-over rather than hanging a deadline-less
// chat/serve conversation indefinitely.
const modelCallTimeout = 10 * time.Minute

// tuningFromConfig maps the operator's onboard.Config provider knobs onto a
// provider.Tuning so resolved providers honor them (reasoning_effort, service_tier,
// prompt_cache_key, parallel_tool_calls, the OpenAI max-tokens field, and the
// OpenRouter attribution headers). A zero Config yields a zero Tuning, which the
// resolver treats as a byte-identical pass-through — so the default path is
// unchanged. The API key is NEVER carried here (I3): only static routing strings.
func tuningFromConfig(cfg onboard.Config) provider.Tuning {
	return provider.Tuning{
		ReasoningEffort:   cfg.ReasoningEffort,
		MaxTokensField:    cfg.MaxTokensField,
		ServiceTier:       cfg.Routing.ServiceTier,
		PromptCacheKey:    cfg.Routing.PromptCacheKey,
		ParallelToolCalls: cfg.Routing.ParallelToolCalls,
		OpenRouterReferer: cfg.OpenRouterReferer,
		OpenRouterTitle:   cfg.OpenRouterTitle,
	}
}

// the backend name + required secret up front. The model spec is NILCORE_MODEL,
// else the configured executor, else the built-in default; the key resolves
// environment-first then SecretStore via b.cred. A missing key is reported with
// the actionable remedy (run init / export the var) rather than a bare error.
func resolveProvider(backendName string, b boot) (model.Provider, error) {
	switch backendName {
	case "native":
		spec := modelSpec(os.Getenv("NILCORE_MODEL"), b.cfg.Executor)
		p, err := provider.ResolveWithTuning(spec, b.cred, tuningFromConfig(b.cfg))
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
			// A hard per-call ceiling so a single WEDGED model call (a stalled
			// connection that never returns and never errors) cannot hang a
			// deadline-less chat/serve run forever — it times out, then retries/fails
			// over. Generous: a legitimate large completion (extended thinking + long
			// output) finishes well within this; only a true hang is cut. build/run
			// also have their own wall-clock deadline on top.
			CallTimeout: modelCallTimeout,
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

// defaultGUIModel is the model the GUI-agent features (computer-use / browser-use)
// run on when nothing is set. These features want a strong, current GUI-grounding
// model rather than the general executor config, so the default is Opus 4.8.
const defaultGUIModel = "claude-opus-4-8"

// guiModelSpec picks the SINGLE model spec for a GUI-agent feature run (CU-T11):
// the per-run flag wins, then the feature env var, then defaultGUIModel. It is one
// model for the whole feature — never multi-model routing.
func guiModelSpec(flag, env string) string {
	if s := strings.TrimSpace(flag); s != "" {
		return s
	}
	if s := strings.TrimSpace(env); s != "" {
		return s
	}
	return defaultGUIModel
}

// resolveNativeSpec resolves an EXPLICIT provider:model spec (used by the GUI-agent
// features, which pick their own single model via guiModelSpec) into a resilient
// provider, mirroring resolveProvider's native path but without the NILCORE_MODEL /
// executor-config lookup.
func resolveNativeSpec(spec string, b boot) (model.Provider, error) {
	p, err := provider.ResolveWithTuning(spec, b.cred, tuningFromConfig(b.cfg))
	if err != nil {
		if env := providerEnv(vendorOf(spec)); env != "" {
			return nil, fmt.Errorf("%w; run `nilcore init` to store the key, or set %s in the environment", err, env)
		}
		return nil, fmt.Errorf("%w; run `nilcore init` to store the key", err)
	}
	res, rerr := model.NewResilient([]model.Provider{p}, model.Options{
		MaxRetries:       2,
		Jitter:           200 * time.Millisecond,
		BreakerThreshold: 4,
		CallTimeout:      modelCallTimeout,
	})
	if rerr != nil {
		return p, nil
	}
	return res, nil
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
	p, err := provider.ResolveWithTuning(spec, b.cred, tuningFromConfig(b.cfg))
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

// attachBlast wires the shared blast-radius budget onto a container sandbox so its
// cumulative WALL-TIME axis is fenced (BR-T03). It is the single seam the env factories
// use; a nil budget (no -blast-radius preset, the default) or a non-container backend
// is returned UNCHANGED, so an unfenced run is byte-identical. The same *blastbudget
// instance is shared across every worktree of a run, so the wall fence bounds the run,
// not each box independently.
//
// KNOWN LIMITATION: the wall-time fence is wired into the container backend only (the
// Blast field lives on *sandbox.Container). The host-native Linux namespace backend
// (sandbox.Namespace, Phase 7) carries no wall fence, so under it the wall axis of
// -blast-radius is not enforced (the host/irreversible/$ axes still apply, at the egress
// proxy and the gate). The container backend is the default; this is a documented gap,
// not a silent failure.
func attachBlast(box sandbox.Sandbox, b *blastbudget.Budget) sandbox.Sandbox {
	if b == nil {
		return box
	}
	if c, ok := box.(*sandbox.Container); ok {
		c.Blast = b
	}
	return box
}

// proxyBindAddr picks the egress-proxy listen address for the sandbox backend that
// selectSandbox will resolve for (prefer, runtime). Only a BRIDGED CONTAINER consumes
// the proxy — it reaches the host-side listener across the runtime bridge via the
// host-gateway alias, so the listener must bind all interfaces (0.0.0.0). Every other
// backend leaves the proxy UNUSED (the namespace sandbox runs in an empty net
// namespace; host-local callers use loopback), so it binds 127.0.0.1 and the guarded
// proxy is never needlessly exposed to the LAN. Resolution mirrors selectSandbox
// exactly (prefer, then NILCORE_SANDBOX, then auto→namespace-if-available-else-container)
// so the bind can never disagree with the box actually chosen.
func proxyBindAddr(prefer, runtime string) string {
	if prefer == "" || prefer == string(sandbox.Auto) {
		if env := os.Getenv("NILCORE_SANDBOX"); env != "" {
			prefer = env
		}
	}
	// An explicit container always needs the proxy reachable across the runtime bridge.
	if sandbox.Backend(prefer) == sandbox.ContainerBackend {
		return "0.0.0.0:0"
	}
	// namespace (explicit) OR auto/empty: the namespace backend never uses the proxy and
	// needs only loopback — BUT selectSandbox DEGRADES an unsatisfiable request (an
	// explicit `-sandbox namespace`, or auto, on a host where the namespace backend is
	// unavailable, e.g. macOS) to a *sandbox.Container, which DOES need the proxy across
	// the bridge. So bind loopback only when the namespace backend is actually available;
	// otherwise fall through to the container default — mirroring selectSandbox's fallback
	// so the bind can never disagree with the box actually chosen.
	if ns, _, _ := sandbox.Available(runtime); ns {
		return "127.0.0.1:0"
	}
	return "0.0.0.0:0"
}

// envFactory builds the per-worktree backend+verifier factory. The optional blast
// budget fences the sandbox wall-time axis (BR-T04 threading); nil ⇒ unfenced,
// byte-identical.
func envFactory(c commonFlags, prov model.Provider, cred func(string) string, adv advisorCfg, log *eventlog.Log, mem *memory.Memory, project string, cfg onboard.Config, blast *blastbudget.Budget) func(string) agent.Env {
	return func(dir string) agent.Env {
		box := attachBlast(selectSandbox(*c.sandboxPref, *c.runtime, *c.image, dir), blast)
		v := behavioralVerifier(box, *c.checkCmd)
		be := buildBackend(*c.backendName, prov, cred, adv, box, v, log, *c.maxSteps, mem, project, cfg)
		// Operator steering (P10-T01): a committed NILCORE.md / AGENTS.md is present in
		// the worktree checkout; load it once and prepend as trusted instructions on
		// the native backend. nil/empty ⇒ byte-identical; only the native loop reads it.
		if n, ok := be.(*backend.Native); ok {
			if steer, _ := steering.DiscoverAndLoad(dir); steer != "" {
				n.SteeringContext = func() string { return steer }
			}
		}
		return agent.Env{Backend: be, Verifier: v, Box: box}
	}
}

// multiEnvFactory is the NewEnvFor analogue of envFactory for the Phase-13 multi-
// backend path: it builds the SAME per-worktree env the single factory does — the
// backend-INDEPENDENT sandbox + verifier for `dir` — but with the backend resolved by
// NAME (buildBackend(name, ...)) rather than by the single -backend flag. Each named
// backend resolves its OWN provider/advisor (native gets the executor provider + the
// advisor tier; codex/claude-code get a nil provider, exactly as resolveProvider /
// resolveAdvisor return today) and its OWN creds via the SecretStore-backed cred
// resolver — the model never sees a key (I3). The verifier is identical across
// backends (the project's checks for the worktree), so only the backend is swapped.
func multiEnvFactory(c commonFlags, b boot, log *eventlog.Log, mem *memory.Memory, project string, blast *blastbudget.Budget) func(dir, name string) agent.Env {
	return func(dir, name string) agent.Env {
		// Per-NAME deps: native needs the executor provider + advisor; codex/claude-code
		// get nil (resolveProvider/resolveAdvisor already special-case them). A provider
		// resolution failure here is FATAL — the operator listed a backend whose key is
		// missing, which would otherwise silently degrade the race.
		prov, perr := resolveProvider(name, b)
		if perr != nil {
			fatal(perr)
		}
		adv := resolveAdvisor(name, b, c)
		box := attachBlast(selectSandbox(*c.sandboxPref, *c.runtime, *c.image, dir), blast)
		v := behavioralVerifier(box, *c.checkCmd)
		be := buildBackend(name, prov, b.cred, adv, box, v, log, *c.maxSteps, mem, project, b.cfg)
		// Operator steering parity with envFactory: load committed NILCORE.md/AGENTS.md
		// once for the native backend (nil/empty ⇒ byte-identical; only native reads it).
		if n, ok := be.(*backend.Native); ok {
			if steer, _ := steering.DiscoverAndLoad(dir); steer != "" {
				n.SteeringContext = func() string { return steer }
			}
		}
		return agent.Env{Backend: be, Verifier: v, Box: box}
	}
}

// wireMultiBackend activates the Phase-13 multi-backend strength-routing path on o
// when -backends names two or more DISTINCT backends. It is ADDITIVE and default-off:
// names==nil or a single name ⇒ it does NOTHING (o keeps NewEnv only, byte-identical
// to the single path). Otherwise it sets o.Backends, o.NewEnvFor (the by-name env
// factory), and o.Selector (the Trust Ledger over the run's event log). A broken-chain
// Replay error is LOGGED and degrades to a nil Selector (configured order) — it never
// aborts the run; a clean/missing log yields an empty ledger ⇒ configured order until
// evidence accrues. The Selector only ORDERS; the verifier still governs "done" (I2).
func wireMultiBackend(o *agent.Orchestrator, c commonFlags, b boot, log *eventlog.Log, mem *memory.Memory, project string, blast *blastbudget.Budget) {
	// Trust-route (Phase 16, RTE-T08): activate the cost-aware oracle when
	// NILCORE_TRUST_DEFAULT=1 — independent of -backends, so a single-backend run
	// also sizes race/escalate by learned per-class data. Fail-soft on a broken
	// chain (no oracle ⇒ the static path); byte-identical default-off when the env
	// is unset (o.Oracle stays nil). The verifier still judges every race (I2).
	if os.Getenv("NILCORE_TRUST_DEFAULT") == "1" {
		if led, err := trust.Replay(*c.logPath); err == nil {
			o.Oracle = agent.NewTrustRouteOracle(led, nil)
		} else {
			fmt.Fprintf(os.Stderr, "trust-route: ledger unavailable (%v); using static routing\n", err)
		}
	}
	// Expand the "auto" token to the host's available backends BEFORE the len<=1
	// check, so `-backends auto` competes every available backend (ledger-ordered)
	// and `-backends auto` on a single-backend host collapses to the single path.
	names := expandAutoBackends(parseBackends(*c.backends), b.cfg, b.cred)
	if len(names) <= 1 {
		return // single path — leave Backends/NewEnvFor/Selector unset (byte-identical)
	}
	o.Backends = names
	o.NewEnvFor = multiEnvFactory(c, b, log, mem, project, blast)
	led, err := trust.Replay(*c.logPath)
	if err != nil {
		// Fail-soft on a broken chain: no trustworthy ranking, so fall back to the
		// configured order (nil Selector) rather than aborting the run.
		fmt.Fprintf(os.Stderr, "trust: ledger unavailable (%v); using configured backend order\n", err)
		log.Append(eventlog.Event{Kind: "trust_replay_error", Detail: map[string]any{"error": err.Error()}})
		return
	}
	o.Selector = trust.NewSelector(led)
}

// resolveDelegated merges a delegated CLI's config-file settings (onboard.Config)
// with runtime env overrides (R1): NILCORE_<CLI>_MODEL / _EFFORT take precedence
// over the config file, while ExtraArgs + Env come from the config file. dc is taken
// by value, so the override never mutates the loaded config. No secret flows through
// here — buildBackend adds the API key separately and it is never logged (I3).
func resolveDelegated(envPrefix string, dc onboard.DelegatedConfig) onboard.DelegatedConfig {
	if v := os.Getenv(envPrefix + "_MODEL"); v != "" {
		dc.Model = v
	}
	if v := os.Getenv(envPrefix + "_EFFORT"); v != "" {
		dc.Effort = v
	}
	return dc
}

func buildBackend(name string, prov model.Provider, cred func(string) string, adv advisorCfg, box sandbox.Sandbox, v verify.Verifier, log *eventlog.Log, maxSteps int, mem *memory.Memory, project string, cfg onboard.Config) backend.CodingBackend {
	switch name {
	case "codex":
		// Key resolved env-first then SecretStore (I3); injected into the container per run.
		// model/effort/extra-args/env come from config (env-overridden) — R1.
		c := resolveDelegated("NILCORE_CODEX", cfg.Codex)
		return &backend.Codex{Box: box, Key: cred("CODEX_API_KEY"), Log: log,
			Model: c.Model, Effort: c.Effort, ExtraArgs: c.ExtraArgs, Env: c.Env}
	case "claude-code":
		c := resolveDelegated("NILCORE_CLAUDE", cfg.Claude)
		return &backend.ClaudeCode{Box: box, Key: cred("ANTHROPIC_API_KEY"), Log: log,
			Model: c.Model, Effort: c.Effort, ExtraArgs: c.ExtraArgs, Env: c.Env}
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
			// Merged task-context view (LRN last mile): the lessons distiller writes
			// to GLOBAL scope, so a project-only query would hide the agent's own
			// distilled lessons. TaskContext folds both scopes under the same total
			// record budget as before, newest-first, with the I7 labels intact.
			n.MemoryContext = func(ctx context.Context, _ string) string {
				blk, _ := mem.TaskContext(ctx, project, 10)
				return blk
			}
		}
		// Repo orientation + window awareness (upgrade program): the map spares the
		// first steps of every drive from ls/cat structure discovery, and the window
		// resolver lets the loop compact BEFORE a context overflow instead of dying
		// on the 400. Both nil-safe seams; box.Workdir() is the per-task worktree.
		n.RepoContext = func(context.Context) string { return repoMap(box.Workdir(), repoMapBudget) }
		n.CtxWindow = meter.CtxWindow
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
func setupPersistence(log *eventlog.Log, logPath string) (*memory.Memory, *agent.Checkpoint, *store.Store) {
	dir, err := paths.EnsureDir(paths.DataDir())
	if err != nil {
		return nil, nil, nil
	}
	s, err := store.Open(filepath.Join(dir, "nilcore.db"))
	if err != nil {
		return nil, nil, nil
	}
	log.UseStore(s)
	wireExperience(log, s)
	mem := memory.New(s)
	// LRN-T03: fold distilled verifier-failure scars into memory so the next same-class
	// task sees them as context. Default-off (NILCORE_LESSONS unset ⇒ no-op).
	wireLessons(logPath, mem)
	// The store is returned so a long-running process (serve) can SHARE this single
	// *sql.DB across its folds (flywheel/autonomy orchestrators + the objective backlog)
	// rather than opening competing single-writer handles to the same file — see
	// buildRunOrchestratorWith / runAutonomyDaemon. One-shot commands ignore it.
	return mem, agent.NewCheckpoint(s), s
}

// wireExperience activates the Phase-16 experience projection (EXP-T03). When
// NILCORE_EXPERIENCE is set, every appended event is folded into the store-backed
// projection as it lands — only verifier-judged race_outcome events change state
// (I2) — so the OverStore reader stays warm for consumers without a full log replay.
// The fold is best-effort behind the authoritative append (a derived projection is
// rebuildable from the log, so a fold error never breaks the log). DEFAULT-OFF: with
// the env unset no hook is installed and Append is byte-identical. The projection is
// also (re)derivable on demand via `nilcore experience --rebuild`.
func wireExperience(log *eventlog.Log, s *store.Store) {
	if !envOptIn("NILCORE_EXPERIENCE") {
		return
	}
	proj := experience.NewProjector(s)
	log.OnAppend(func(e eventlog.Event) { _ = proj.Fold(context.Background(), e) })
}

// envOptIn reports whether a boolean opt-in environment variable is AFFIRMATIVELY
// enabled. Unset, empty, and the explicit negatives ("0"/"false"/"no"/"off") read as
// OFF; any other non-empty value is ON. This mirrors the killswitch/self-improve
// convention and avoids the footgun where NILCORE_EXPERIENCE=0 (meaning "off")
// silently enabled the projection because the old gate only checked != "". Note: this
// is for BOOLEAN flags only — value-carrying vars (e.g. NILCORE_LOG_HMAC_KEY, whose
// presence-or-absence IS the semantics) keep their own non-empty check.
func envOptIn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
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
	cfg := applyAutoApprovePreset(loadConfig(configPath))
	return boot{cfg: cfg, cred: compatCredOverlay(cfg, newCredResolver(cfg, detectStore(false), os.Getenv))}
}

// compatCredOverlay bridges the config file to provider resolution for the
// operator-typed "openai-compatible" vendor. provider.ResolveWith reads the
// endpoint knobs through the cred/getenv seam by NAME — NILCORE_COMPAT_BASE_URL,
// NILCORE_COMPAT_AUTH_SCHEME, NILCORE_COMPAT_KEY_ENV — but nothing maps the
// onboard.Config equivalents (BaseURL/AuthScheme/CompatKeyEnv) into that seam, so
// without this overlay the config-file path is dead and only real env vars work.
//
// The returned resolver populates exactly those three NAMES from the config when
// the corresponding field is set, and falls through to base for every other name
// (and for these names when the config field is empty). Precedence: a real env var
// WINS — base(name) is consulted first and only an empty result falls back to the
// configured value — so an operator can still override the file from the
// environment, and the file fills in only what the environment leaves unset.
//
// Only NAMES flow through this overlay: the compat KEY value itself is never read
// or carried here — provider.ResolveWith reads it via the (unmodified) base cred
// under whatever NAME CompatKeyEnv selects, so no secret VALUE is logged or
// duplicated (invariant I3).
//
// CRITICAL: when the config sets NONE of the three compat fields (the common
// case), this overlay is a pure pass-through — the returned func behaves
// identically to base for every name, so existing setups see zero behavior change.
func compatCredOverlay(cfg onboard.Config, base func(string) string) func(string) string {
	overrides := map[string]string{}
	if v := strings.TrimSpace(cfg.BaseURL); v != "" {
		overrides["NILCORE_COMPAT_BASE_URL"] = cfg.BaseURL
	}
	if v := strings.TrimSpace(cfg.AuthScheme); v != "" {
		overrides["NILCORE_COMPAT_AUTH_SCHEME"] = cfg.AuthScheme
	}
	if v := strings.TrimSpace(cfg.CompatKeyEnv); v != "" {
		overrides["NILCORE_COMPAT_KEY_ENV"] = cfg.CompatKeyEnv
	}
	if len(overrides) == 0 {
		return base // no compat config ⇒ pure pass-through, byte-identical to base.
	}
	return func(name string) string {
		// Real env wins: consult base first, fall back to the configured value only
		// when the environment leaves the name unset.
		if v := base(name); v != "" {
			return v
		}
		if v, ok := overrides[name]; ok {
			return v
		}
		return ""
	}
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
