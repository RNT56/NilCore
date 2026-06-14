package onboard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"nilcore/internal/secrets"
)

// ErrAborted is returned by Run when the operator declines the final write
// confirmation. The config is not written; any keys already entered remain in the
// secret store (re-running init reuses them).
var ErrAborted = errors.New("onboarding aborted")

// Wizard drives the interactive flow. In/Out are normally os.Stdin/os.Stdout;
// Secrets receives the captured keys (the config only ever sees their names).
// ConfigPath, when set, is echoed up front so the operator knows where the config
// will land before typing anything.
type Wizard struct {
	In         io.Reader
	Out        io.Writer
	Secrets    secrets.SecretStore
	ConfigPath string
}

// providerPrompts is the ordered set of model-provider keys the wizard captures.
var providerPrompts = []struct{ name, prompt, secret string }{
	{"anthropic", "  Anthropic API key", "anthropic_api_key"},
	{"openai", "  OpenAI API key", "openai_api_key"},
	{"openrouter", "  OpenRouter API key", "openrouter_api_key"},
}

// Sensible per-provider model defaults, used to seed the executor/advisor prompts
// from whichever key the operator actually entered (so an OpenAI-only setup does
// not default to an Anthropic model it has no key for).
var (
	execDefaults = map[string]string{
		"anthropic":  "anthropic:claude-sonnet-4-6",
		"openai":     "openai:gpt-5.5",
		"openrouter": "openrouter:openrouter/fusion",
	}
	advDefaults = map[string]string{
		"anthropic":  "anthropic:claude-opus-4-8",
		"openai":     "openai:gpt-5.5",
		"openrouter": "openrouter:openrouter/fusion",
	}
)

// Run prompts through the flow, stores any captured secrets, and returns the
// assembled config plus a readiness summary. It returns ErrAborted if the
// operator declines the final confirmation.
func (w *Wizard) Run() (Config, error) {
	st := newStyle(w.Out)
	sc := bufio.NewScanner(w.In)

	fmt.Fprintln(w.Out, st.bold("NilCore setup"))
	fmt.Fprintln(w.Out, st.dim("One guided pass: providers & keys, runtime, backend, chat channel."))
	if w.ConfigPath != "" {
		fmt.Fprintln(w.Out, st.dim("Config → "+w.ConfigPath))
	}
	fmt.Fprintln(w.Out, st.dim(fmt.Sprintf("Secrets → %s backend (never written to the config).", w.Secrets.Name())))

	ask := func(prompt, def string) string {
		if def != "" {
			fmt.Fprintf(w.Out, "%s [%s]: ", prompt, st.dim(def))
		} else {
			fmt.Fprintf(w.Out, "%s: ", prompt)
		}
		if !sc.Scan() {
			return def
		}
		v := strings.TrimSpace(sc.Text())
		if v == "" {
			return def
		}
		return v
	}
	// captureSecret reads a secret with terminal echo disabled, stores it under
	// name, prints a masked confirmation (never the value), and returns the
	// reference (the name). If echo cannot be disabled on a real terminal it warns
	// loudly that input will be visible rather than masking the failure silently.
	captureSecret := func(prompt, name string) (string, error) {
		restore, masked := echoOff(w.In)
		if isTTY(w.In) && !masked {
			fmt.Fprintln(w.Out, st.dim("  warning: terminal echo could not be disabled — your input will be visible"))
		}
		fmt.Fprintf(w.Out, "%s: ", prompt)
		ok := sc.Scan()
		restore()
		fmt.Fprintln(w.Out) // the (hidden) Enter left the cursor on the prompt line
		if !ok {
			return "", nil
		}
		v := strings.TrimSpace(sc.Text())
		if v == "" {
			return "", nil
		}
		if err := w.Secrets.Set(name, v); err != nil {
			return "", fmt.Errorf("store %s: %w", name, err)
		}
		fmt.Fprintln(w.Out, st.dim(fmt.Sprintf("  stored %s (%s)", name, maskTail(v))))
		return name, nil
	}

	cfg := Config{Version: CurrentConfigVersion, Runtime: defaultRuntime()}

	section(w.Out, st, "Sandbox")
	cfg.Runtime = ask("Container runtime (podman|docker)", cfg.Runtime)
	cfg.Image = ask("Sandbox image", DefaultImage)

	section(w.Out, st, "Backend")
	fmt.Fprintln(w.Out, st.dim("native runs the in-process loop; codex/claude-code delegate to that CLI."))
	cfg.Backend = ask("Coding backend (native|codex|claude-code)", "native")

	section(w.Out, st, "Provider keys")
	fmt.Fprintln(w.Out, st.dim("Leave blank to skip a provider. Keys go to the secret store, not the config."))
	for _, p := range providerPrompts {
		ref, err := captureSecret(p.prompt, p.secret)
		if err != nil {
			return Config{}, err
		}
		if ref != "" {
			cfg.Providers = append(cfg.Providers, ProviderConfig{Name: p.name, KeyRef: ref})
		}
	}

	section(w.Out, st, "Models")
	fmt.Fprintln(w.Out, st.dim("The executor runs every task; the advisor is a stronger model it escalates to when stuck."))
	exec, adv := defaultModels(cfg.Providers)
	cfg.Executor = ask("Executor model (provider:model)", exec)
	cfg.Advisor = ask("Advisor model", adv)

	section(w.Out, st, "Chat channel")
	fmt.Fprintln(w.Out, st.dim("serve listens here and routes approval gates back as chat replies (Enter to skip)."))
	cfg.Channel.Type = ask("Chat channel (telegram|slack|none)", "none")
	switch cfg.Channel.Type {
	case "telegram":
		ref, err := captureSecret("  Telegram bot token", "telegram_bot_token")
		if err != nil {
			return Config{}, err
		}
		if ref != "" {
			cfg.Channel.TokenRefs = []string{ref}
		}
	case "slack":
		app, err := captureSecret("  Slack app token", "slack_app_token")
		if err != nil {
			return Config{}, err
		}
		bot, err := captureSecret("  Slack bot token", "slack_bot_token")
		if err != nil {
			return Config{}, err
		}
		// Slack needs both tokens; keep them in fixed app-then-bot order
		// (secretRefsByEnv maps positionally). If either is missing, leave the
		// channel unconfigured rather than mislabel one token as the other.
		if app != "" && bot != "" {
			cfg.Channel.TokenRefs = []string{app, bot}
		} else {
			fmt.Fprintln(w.Out, st.dim("  slack needs both an app and a bot token — channel left unconfigured"))
			cfg.Channel.Type = "none"
		}
	}
	if cfg.Channel.Type == "telegram" || cfg.Channel.Type == "slack" {
		fmt.Fprintln(w.Out, st.dim("serve refuses to start without an allowlist — only these ids can drive the agent."))
		cfg.Channel.Allow = splitList(ask("  Allowed principal ids (comma-separated)", ""))
	}

	// Delegated-backend key (optional): the codex backend reads CODEX_API_KEY;
	// claude-code reuses the Anthropic key captured above.
	if ref, err := captureSecret("  Codex API key (for the codex backend; blank to skip)", "codex_api_key"); err != nil {
		return Config{}, err
	} else if ref != "" {
		cfg.Providers = append(cfg.Providers, ProviderConfig{Name: "codex", KeyRef: ref})
	}

	cfg.Delegated = detectDelegated()

	if err := sc.Err(); err != nil {
		return Config{}, fmt.Errorf("reading input: %w", err)
	}

	fmt.Fprintf(w.Out, "\n%s\n%s", st.bold("Readiness"), st.colorGlyphs(cfg.Readiness()))

	fmt.Fprint(w.Out, "\n"+st.bold("Write this config?")+" [Y/n]: ")
	if sc.Scan() && strings.HasPrefix(strings.ToLower(strings.TrimSpace(sc.Text())), "n") {
		return Config{}, ErrAborted
	}
	return cfg, nil
}

// FromEnv assembles a config non-interactively from environment variables, for
// scripted provisioning. Keys present in the environment are copied into the
// SecretStore and referenced; absent ones are skipped. The executor/advisor
// default to a model of whichever provider key was supplied, and either chat
// channel (telegram or slack) plus its allowlist can be provisioned.
func FromEnv(getenv func(string) string, store secrets.SecretStore) (Config, error) {
	cfg := Config{
		Version: CurrentConfigVersion,
		Runtime: orDefault(getenv("NILCORE_RUNTIME"), defaultRuntime()),
		Image:   orDefault(getenv("NILCORE_IMAGE"), DefaultImage),
		Backend: orDefault(getenv("NILCORE_BACKEND"), "native"),
	}
	for _, p := range []struct{ name, env, secret string }{
		{"anthropic", "ANTHROPIC_API_KEY", "anthropic_api_key"},
		{"openai", "OPENAI_API_KEY", "openai_api_key"},
		{"openrouter", "OPENROUTER_API_KEY", "openrouter_api_key"},
		{"codex", "CODEX_API_KEY", "codex_api_key"}, // delegated codex backend key
	} {
		if v := getenv(p.env); v != "" {
			if err := store.Set(p.secret, v); err != nil {
				return Config{}, err
			}
			cfg.Providers = append(cfg.Providers, ProviderConfig{Name: p.name, KeyRef: p.secret})
		}
	}
	// Seed executor/advisor from the supplied keys, letting an explicit env spec win.
	exec, adv := defaultModels(cfg.Providers)
	cfg.Executor = orDefault(getenv("NILCORE_EXECUTOR"), exec)
	cfg.Advisor = orDefault(getenv("NILCORE_ADVISOR"), adv)

	switch {
	case getenv("TELEGRAM_BOT_TOKEN") != "":
		if err := store.Set("telegram_bot_token", getenv("TELEGRAM_BOT_TOKEN")); err != nil {
			return Config{}, err
		}
		cfg.Channel = ChannelConfig{Type: "telegram", TokenRefs: []string{"telegram_bot_token"}}
	case getenv("SLACK_APP_TOKEN") != "" && getenv("SLACK_BOT_TOKEN") != "":
		if err := store.Set("slack_app_token", getenv("SLACK_APP_TOKEN")); err != nil {
			return Config{}, err
		}
		if err := store.Set("slack_bot_token", getenv("SLACK_BOT_TOKEN")); err != nil {
			return Config{}, err
		}
		cfg.Channel = ChannelConfig{Type: "slack", TokenRefs: []string{"slack_app_token", "slack_bot_token"}}
	}
	if cfg.Channel.Type != "" && cfg.Channel.Type != "none" {
		cfg.Channel.Allow = splitList(getenv("NILCORE_ALLOWLIST"))
	}

	cfg.Delegated = detectDelegated()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// PromptSecret reads a single secret from in with terminal echo disabled,
// printing prompt to out. It returns the trimmed value (empty if none was
// entered) and never echoes or logs it. Used by `nilcore secret set` to store or
// rotate one credential outside the full wizard.
func PromptSecret(prompt string, in io.Reader, out io.Writer) (string, error) {
	sc := bufio.NewScanner(in)
	restore, masked := echoOff(in)
	if isTTY(in) && !masked {
		fmt.Fprintln(out, "  warning: terminal echo could not be disabled — your input will be visible")
	}
	fmt.Fprintf(out, "%s: ", prompt)
	ok := sc.Scan()
	restore()
	fmt.Fprintln(out)
	if !ok {
		return "", sc.Err()
	}
	return strings.TrimSpace(sc.Text()), nil
}

// Readiness is a short human summary of whether the config can run a task — and,
// when a channel is set, whether it can serve one. ✓/✗ per check; honest about
// the serve-only allowlist requirement that `run` does not need.
func (c Config) Readiness() string {
	var b strings.Builder
	ok := func(cond bool) string {
		if cond {
			return "✓"
		}
		return "✗"
	}
	names := map[string]bool{}
	for _, p := range c.Providers {
		names[p.Name] = true
	}
	fmt.Fprintf(&b, "  %s a provider key is configured (%d)\n", ok(len(c.Providers) > 0), len(c.Providers))
	switch c.Backend {
	case "codex":
		fmt.Fprintf(&b, "  %s codex backend key configured\n", ok(names["codex"]))
	case "claude-code":
		fmt.Fprintf(&b, "  %s claude-code backend key (anthropic) configured\n", ok(names["anthropic"]))
	default: // native
		if c.Executor != "" {
			v := providerOf(c.Executor)
			fmt.Fprintf(&b, "  %s executor %q has a configured key (%s)\n", ok(names[v]), c.Executor, v)
		}
	}
	fmt.Fprintf(&b, "  %s container runtime %q on PATH\n", ok(onPath(c.Runtime)), c.Runtime)
	fmt.Fprintf(&b, "  %s chat channel: %s\n", ok(c.Channel.Type != "" && c.Channel.Type != "none"), c.Channel.Type)
	if c.Channel.Type == "telegram" || c.Channel.Type == "slack" {
		fmt.Fprintf(&b, "  %s serve allowlist set (%d) — required to serve a channel\n", ok(len(c.Channel.Allow) > 0), len(c.Channel.Allow))
	}
	if len(c.Delegated) > 0 {
		fmt.Fprintf(&b, "  ✓ delegated CLIs detected: %s\n", strings.Join(c.Delegated, ", "))
	}
	return b.String()
}

// defaultModels picks executor/advisor model defaults from the first captured
// provider (anthropic → openai → openrouter), falling back to the Anthropic
// defaults when none were captured, so the suggested model matches the key the
// operator actually entered.
func defaultModels(ps []ProviderConfig) (executor, advisor string) {
	has := map[string]bool{}
	for _, p := range ps {
		has[p.Name] = true
	}
	for _, name := range []string{"anthropic", "openai", "openrouter"} {
		if has[name] {
			return execDefaults[name], advDefaults[name]
		}
	}
	return execDefaults["anthropic"], advDefaults["anthropic"]
}

// providerOf returns the vendor of a "provider:model" spec (mirroring
// provider.split): a bare model is Anthropic, a bare "openrouter" is OpenRouter.
func providerOf(spec string) string {
	if i := strings.Index(spec, ":"); i >= 0 {
		return spec[:i]
	}
	if spec == "openrouter" {
		return "openrouter"
	}
	return "anthropic"
}

// splitList parses a comma-separated list into a trimmed, de-duplicated slice.
func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// maskTail renders a short, non-revealing tail of a secret for a confirmation
// line — never more than the last 4 characters, never the whole value (I3).
func maskTail(v string) string {
	if len(v) <= 4 {
		return "…"
	}
	return "…" + v[len(v)-4:]
}

func detectDelegated() []string {
	var found []string
	for _, bin := range []string{"codex", "claude"} {
		if onPath(bin) {
			found = append(found, bin)
		}
	}
	return found
}

func onPath(bin string) bool {
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// isTTY reports whether r is a real terminal (a char device), so the wizard can
// tell "echo failed on a tty" (warn) from "input is a pipe/test reader" (silent).
func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// echoOff disables terminal echo while a secret is typed and returns a restore
// func plus whether masking actually took effect. Stdlib only (invariant I6: no
// x/term) — it toggles echo via stty on the controlling terminal. It is a no-op
// (masked=false) when in is not a terminal or stty is unavailable, so
// non-interactive provisioning is never broken (audit L8). While echo is off it
// installs an interrupt handler that re-enables echo before exiting, so Ctrl-C
// mid-prompt never leaves the operator's terminal stuck with echo disabled.
func echoOff(in io.Reader) (restore func(), masked bool) {
	f, ok := in.(*os.File)
	if !ok {
		return func() {}, false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return func() {}, false
	}
	if sttyEcho(f, false) != nil {
		return func() {}, false
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			_ = sttyEcho(f, true) // un-hide the terminal before we die
			os.Exit(130)          // 128 + SIGINT, the conventional interrupt code
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
		_ = sttyEcho(f, true)
	}, true
}

// sttyEcho turns terminal echo on or off for the given tty.
func sttyEcho(tty *os.File, on bool) error {
	mode := "-echo"
	if on {
		mode = "echo"
	}
	cmd := exec.Command("stty", mode)
	cmd.Stdin = tty
	return cmd.Run()
}

func defaultRuntime() string {
	if onPath("podman") {
		return "podman"
	}
	if onPath("docker") {
		return "docker"
	}
	return "podman"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// style applies ANSI styling only when Out is a real terminal that opted in
// (honoring NO_COLOR and TERM=dumb), so piped/redirected/SSH-without-color output
// stays clean. Stdlib only — plain escape constants, no lipgloss.
type style struct{ on bool }

func newStyle(out io.Writer) style {
	f, ok := out.(*os.File)
	if !ok {
		return style{}
	}
	fi, err := f.Stat()
	on := err == nil && fi.Mode()&os.ModeCharDevice != 0 &&
		os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return style{on: on}
}

func (s style) wrap(code, t string) string {
	if !s.on {
		return t
	}
	return "\033[" + code + "m" + t + "\033[0m"
}

func (s style) bold(t string) string { return s.wrap("1", t) }
func (s style) dim(t string) string  { return s.wrap("2", t) }

// colorGlyphs tints the ✓/✗ readiness markers green/red when styling is on, and
// leaves the string untouched otherwise — so the interactive wizard's readiness
// reads at a glance, while the same plain Readiness() text stays clean when piped
// (e.g. `nilcore doctor` to a logfile).
func (s style) colorGlyphs(t string) string {
	if !s.on {
		return t
	}
	t = strings.ReplaceAll(t, "✓", "\033[32m✓\033[0m")
	t = strings.ReplaceAll(t, "✗", "\033[31m✗\033[0m")
	return t
}

// section prints a blank line and a styled group header, giving the wizard a
// consistent vertical rhythm instead of ad-hoc newline prefixes.
func section(w io.Writer, st style, title string) {
	fmt.Fprintf(w, "\n%s\n", st.bold(title))
}
