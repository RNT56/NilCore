package onboard

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"nilcore/internal/secrets"
)

// Wizard drives the interactive flow. In/Out are normally os.Stdin/os.Stdout;
// Secrets receives the captured keys (the config only ever sees their names).
type Wizard struct {
	In      io.Reader
	Out     io.Writer
	Secrets secrets.SecretStore
}

// Run prompts through the flow, stores any captured secrets, and returns the
// assembled config plus a readiness summary.
func (w *Wizard) Run() (Config, error) {
	sc := bufio.NewScanner(w.In)
	ask := func(prompt, def string) string {
		if def != "" {
			fmt.Fprintf(w.Out, "%s [%s]: ", prompt, def)
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
	// captureSecret reads a secret with terminal echo disabled so the key is not
	// shown on screen, stores it under name, and returns the reference (the name).
	// The value is never echoed, logged, or written to the config — only its name.
	captureSecret := func(prompt, name string) (string, error) {
		fmt.Fprintf(w.Out, "%s: ", prompt)
		restore := echoOff(w.In)
		ok := sc.Scan()
		restore()
		fmt.Fprintln(w.Out) // the hidden Enter left the cursor on the prompt line
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
		return name, nil
	}

	cfg := Config{Runtime: defaultRuntime()}
	cfg.Runtime = ask("Container runtime (podman|docker)", cfg.Runtime)
	cfg.Image = ask("Sandbox image", DefaultImage)

	// Providers + keys (keys → SecretStore, config keeps the reference).
	fmt.Fprintln(w.Out, "\nProvider keys (leave blank to skip a provider):")
	for _, p := range []struct{ name, prompt, secret string }{
		{"anthropic", "  Anthropic API key", "anthropic_api_key"},
		{"openai", "  OpenAI API key", "openai_api_key"},
		{"openrouter", "  OpenRouter API key", "openrouter_api_key"},
	} {
		ref, err := captureSecret(p.prompt, p.secret)
		if err != nil {
			return Config{}, err
		}
		if ref != "" {
			cfg.Providers = append(cfg.Providers, ProviderConfig{Name: p.name, KeyRef: ref})
		}
	}

	cfg.Executor = ask("\nExecutor model (role→provider:model)", "anthropic:claude-sonnet-4-6")
	cfg.Advisor = ask("Advisor model", "anthropic:claude-opus-4-8")

	// Chat channel.
	cfg.Channel.Type = ask("\nChat channel (telegram|slack|none)", "none")
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
		for _, r := range []string{app, bot} {
			if r != "" {
				cfg.Channel.TokenRefs = append(cfg.Channel.TokenRefs, r)
			}
		}
	}

	cfg.Delegated = detectDelegated()

	fmt.Fprintf(w.Out, "\nReadiness:\n%s\n", cfg.Readiness())
	return cfg, nil
}

// FromEnv assembles a config non-interactively from environment variables, for
// scripted provisioning. Keys present in the environment are copied into the
// SecretStore and referenced; absent ones are skipped.
func FromEnv(getenv func(string) string, store secrets.SecretStore) (Config, error) {
	cfg := Config{
		Runtime:  orDefault(getenv("NILCORE_RUNTIME"), defaultRuntime()),
		Image:    orDefault(getenv("NILCORE_IMAGE"), DefaultImage),
		Executor: orDefault(getenv("NILCORE_EXECUTOR"), "anthropic:claude-sonnet-4-6"),
		Advisor:  orDefault(getenv("NILCORE_ADVISOR"), "anthropic:claude-opus-4-8"),
	}
	for _, p := range []struct{ name, env, secret string }{
		{"anthropic", "ANTHROPIC_API_KEY", "anthropic_api_key"},
		{"openai", "OPENAI_API_KEY", "openai_api_key"},
		{"openrouter", "OPENROUTER_API_KEY", "openrouter_api_key"},
	} {
		if v := getenv(p.env); v != "" {
			if err := store.Set(p.secret, v); err != nil {
				return Config{}, err
			}
			cfg.Providers = append(cfg.Providers, ProviderConfig{Name: p.name, KeyRef: p.secret})
		}
	}
	if tok := getenv("TELEGRAM_BOT_TOKEN"); tok != "" {
		if err := store.Set("telegram_bot_token", tok); err != nil {
			return Config{}, err
		}
		cfg.Channel = ChannelConfig{Type: "telegram", TokenRefs: []string{"telegram_bot_token"}}
	}
	cfg.Delegated = detectDelegated()
	return cfg, nil
}

// Readiness is a short human summary of whether the config can run a task.
func (c Config) Readiness() string {
	var b strings.Builder
	ok := func(cond bool) string {
		if cond {
			return "✓"
		}
		return "✗"
	}
	fmt.Fprintf(&b, "  %s a provider key is configured (%d)\n", ok(len(c.Providers) > 0), len(c.Providers))
	fmt.Fprintf(&b, "  %s container runtime %q on PATH\n", ok(onPath(c.Runtime)), c.Runtime)
	fmt.Fprintf(&b, "  %s chat channel: %s\n", ok(c.Channel.Type != "" && c.Channel.Type != "none"), c.Channel.Type)
	if len(c.Delegated) > 0 {
		fmt.Fprintf(&b, "  ✓ delegated CLIs: %s\n", strings.Join(c.Delegated, ", "))
	}
	return b.String()
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

// echoOff disables terminal echo while a secret is typed and returns a function
// that restores it. Stdlib only (invariant I6: no x/term dependency) — it toggles
// echo via stty on the controlling terminal. It is a no-op when in is not a
// terminal (a pipe or a test reader) or when stty is unavailable, so
// non-interactive provisioning is never broken; it just degrades to visible
// input in that case (audit L8).
func echoOff(in io.Reader) func() {
	f, ok := in.(*os.File)
	if !ok {
		return func() {}
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return func() {}
	}
	if sttyEcho(f, false) != nil {
		return func() {}
	}
	return func() { _ = sttyEcho(f, true) }
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
