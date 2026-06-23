package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/onboard"
	"nilcore/internal/provider"
	"nilcore/internal/secrets"
)

// fakeStore is a SecretStore backed by a map, for hermetic resolver tests.
type fakeStore struct{ m map[string]string }

func (f fakeStore) Get(name string) (string, error) {
	if v, ok := f.m[name]; ok {
		return v, nil
	}
	return "", secrets.ErrNotFound
}
func (fakeStore) Set(string, string) error { return nil }
func (fakeStore) Delete(string) error      { return nil }
func (fakeStore) Name() string             { return "fake" }

func TestCredResolverPrefersEnvThenStore(t *testing.T) {
	cfg := onboard.Config{
		Providers: []onboard.ProviderConfig{{Name: "openrouter", KeyRef: "openrouter_api_key"}},
		Channel:   onboard.ChannelConfig{Type: "slack", TokenRefs: []string{"slack_app_token", "slack_bot_token"}},
	}
	store := fakeStore{m: map[string]string{
		"openrouter_api_key": "from-store",
		"slack_app_token":    "app-store",
		"slack_bot_token":    "bot-store",
	}}
	env := map[string]string{"OPENROUTER_API_KEY": "from-env"}
	cred := newCredResolver(cfg, store, func(k string) string { return env[k] })

	if got := cred("OPENROUTER_API_KEY"); got != "from-env" {
		t.Errorf("env should win: got %q", got)
	}
	// Channel tokens are not in the environment → resolved from the store by ref.
	if got := cred("SLACK_APP_TOKEN"); got != "app-store" {
		t.Errorf("slack app from store: got %q", got)
	}
	if got := cred("SLACK_BOT_TOKEN"); got != "bot-store" {
		t.Errorf("slack bot from store: got %q", got)
	}
	// No env and no configured reference → empty (caller reports the error).
	if got := cred("CODEX_API_KEY"); got != "" {
		t.Errorf("codex has no ref: got %q", got)
	}
}

func TestCredResolverStoreFallbackWhenEnvEmpty(t *testing.T) {
	cfg := onboard.Config{Providers: []onboard.ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}}}
	store := fakeStore{m: map[string]string{"anthropic_api_key": "sk-stored"}}
	cred := newCredResolver(cfg, store, func(string) string { return "" })
	if got := cred("ANTHROPIC_API_KEY"); got != "sk-stored" {
		t.Errorf("store fallback: got %q", got)
	}
}

// TestCompatCredOverlayPassThrough is the no-behavior-change keystone for P15-T16:
// when the config sets NONE of the compat fields (the overwhelmingly common case),
// the overlay MUST be a pure pass-through — wrapped(name) == base(name) for every
// name, including the compat NAMEs. Any divergence would silently change behavior
// for every existing setup.
func TestCompatCredOverlayPassThrough(t *testing.T) {
	cfg := onboard.Config{} // no BaseURL / AuthScheme / CompatKeyEnv
	base := func(name string) string {
		switch name {
		case "ANTHROPIC_API_KEY":
			return "sk-base"
		case "NILCORE_COMPAT_BASE_URL":
			return "https://env.example/v1" // even if the env happens to set it
		default:
			return ""
		}
	}
	wrapped := compatCredOverlay(cfg, base)
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY",
		"NILCORE_COMPAT_BASE_URL", "NILCORE_COMPAT_AUTH_SCHEME",
		"NILCORE_COMPAT_KEY_ENV", "NILCORE_COMPAT_API_KEY", "SOME_OTHER_VAR",
	} {
		if wrapped(name) != base(name) {
			t.Errorf("pass-through broken for %q: wrapped=%q base=%q", name, wrapped(name), base(name))
		}
	}
}

// TestCompatCredOverlayPopulatesFromConfig proves the overlay maps the three
// onboard.Config fields onto the env NAMES provider.ResolveWith reads, and ONLY
// those names — every other name falls through to base unchanged.
func TestCompatCredOverlayPopulatesFromConfig(t *testing.T) {
	cfg := onboard.Config{
		BaseURL:      "https://llm.internal/v1",
		AuthScheme:   "bearer",
		CompatKeyEnv: "MY_COMPAT_KEY",
	}
	base := func(name string) string {
		if name == "MY_COMPAT_KEY" {
			return "secret-value" // the key VALUE rides base, not the overlay
		}
		return ""
	}
	wrapped := compatCredOverlay(cfg, base)

	if got := wrapped("NILCORE_COMPAT_BASE_URL"); got != "https://llm.internal/v1" {
		t.Errorf("base url from config: got %q", got)
	}
	if got := wrapped("NILCORE_COMPAT_AUTH_SCHEME"); got != "bearer" {
		t.Errorf("auth scheme from config: got %q", got)
	}
	if got := wrapped("NILCORE_COMPAT_KEY_ENV"); got != "MY_COMPAT_KEY" {
		t.Errorf("key-env name from config: got %q", got)
	}
	// The key VALUE is read by NAME via base, never carried by the overlay.
	if got := wrapped("MY_COMPAT_KEY"); got != "secret-value" {
		t.Errorf("key value should fall through to base: got %q", got)
	}
	// An unrelated name is untouched.
	if got := wrapped("ANTHROPIC_API_KEY"); got != "" {
		t.Errorf("unrelated name should fall through: got %q", got)
	}
}

// TestCompatCredOverlayEnvWins pins the chosen precedence: a real env var (surfaced
// by base) OVERRIDES the configured value, while the config fills in only names the
// environment leaves unset.
func TestCompatCredOverlayEnvWins(t *testing.T) {
	cfg := onboard.Config{
		BaseURL:      "https://from-config/v1",
		AuthScheme:   "azure",
		CompatKeyEnv: "CFG_KEY",
	}
	base := func(name string) string {
		if name == "NILCORE_COMPAT_BASE_URL" {
			return "https://from-env/v1" // env set for the base URL only
		}
		return ""
	}
	wrapped := compatCredOverlay(cfg, base)

	if got := wrapped("NILCORE_COMPAT_BASE_URL"); got != "https://from-env/v1" {
		t.Errorf("real env must win: got %q", got)
	}
	// The other two names have no env, so the config value fills in.
	if got := wrapped("NILCORE_COMPAT_AUTH_SCHEME"); got != "azure" {
		t.Errorf("config fills unset name: got %q", got)
	}
	if got := wrapped("NILCORE_COMPAT_KEY_ENV"); got != "CFG_KEY" {
		t.Errorf("config fills unset name: got %q", got)
	}
}

// TestCompatCredOverlayResolvesProvider is the end-to-end acceptance: a wrapped cred
// built from a config with BaseURL/AuthScheme/CompatKeyEnv lets
// provider.ResolveWith("openai-compatible:...") succeed using those values, against
// a fake base cred that holds ONLY the compat key under the configured NAME.
func TestCompatCredOverlayResolvesProvider(t *testing.T) {
	cfg := onboard.Config{
		BaseURL:      "https://llm.internal/v1",
		AuthScheme:   "bearer",
		CompatKeyEnv: "MY_COMPAT_KEY",
	}
	base := func(name string) string {
		if name == "MY_COMPAT_KEY" {
			return "sk-compat"
		}
		return "" // no NILCORE_COMPAT_* in the real env — config must supply them
	}
	wrapped := compatCredOverlay(cfg, base)

	prov, err := provider.ResolveWith("openai-compatible:some-model", wrapped)
	if err != nil {
		t.Fatalf("ResolveWith should succeed via config-supplied compat knobs: %v", err)
	}
	if prov == nil {
		t.Fatal("resolved provider is nil")
	}

	// And with NONE of the fields set, resolution fails exactly as the raw env path
	// would (NILCORE_COMPAT_BASE_URL is required) — proving the overlay adds nothing
	// when unconfigured.
	bare := compatCredOverlay(onboard.Config{}, func(string) string { return "" })
	if _, err := provider.ResolveWith("openai-compatible:some-model", bare); err == nil {
		t.Fatal("expected resolution to fail with no base url configured")
	}
}

// A configured reference whose secret is absent from the store must resolve to ""
// (so the caller reports the specific missing-credential error), not panic or leak.
func TestCredResolverStoreMissYieldsEmpty(t *testing.T) {
	cfg := onboard.Config{Providers: []onboard.ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}}}
	store := fakeStore{m: map[string]string{}} // ref configured, but not present
	cred := newCredResolver(cfg, store, func(string) string { return "" })
	if got := cred("ANTHROPIC_API_KEY"); got != "" {
		t.Errorf("store miss should yield empty, got %q", got)
	}
}

// TestLoadConfigDegradesGracefully is the regression-safety keystone: a host with
// no config.json — or a corrupt one — must degrade to the zero Config (no panic,
// no error propagation), so the run falls back to the environment + built-in
// defaults exactly as before this wiring existed.
func TestLoadConfigDegradesGracefully(t *testing.T) {
	// Missing file → zero Config.
	if cfg := loadConfig(filepath.Join(t.TempDir(), "does-not-exist.json")); cfg.Executor != "" || len(cfg.Providers) != 0 || cfg.Runtime != "" || cfg.Channel.Type != "" {
		t.Errorf("missing config should yield the zero Config, got %+v", cfg)
	}
	// Malformed JSON → zero Config (not a fatal error).
	bad := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(bad, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(bad); cfg.Executor != "" || len(cfg.Providers) != 0 {
		t.Errorf("malformed config should yield the zero Config, got %+v", cfg)
	}
}

// TestFileVaultRoundTrip proves the headless path: a key written by `nilcore init`
// (write handle) is readable by the run path (a fresh handle on the same dir), and
// the plaintext never lands in the on-disk vault (invariant I3).
func TestFileVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := fileVault(dir)
	if w.Name() != "file" {
		t.Fatalf("expected encrypted file vault, got %q", w.Name())
	}
	if err := w.Set("openrouter_api_key", "sk-or-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A separate handle (same dir/key) mimics the run path reading what init wrote.
	if got, err := fileVault(dir).Get("openrouter_api_key"); err != nil || got != "sk-or-secret" {
		t.Fatalf("Get = %q, %v; want the stored secret", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "secrets.vault"))
	if strings.Contains(string(raw), "sk-or-secret") {
		t.Fatal("vault file contains plaintext — I3 violation")
	}
}

func TestSecretRefsByEnv(t *testing.T) {
	cfg := onboard.Config{
		Providers: []onboard.ProviderConfig{
			{Name: "anthropic", KeyRef: "anthropic_api_key"},
			{Name: "openrouter", KeyRef: "openrouter_api_key"},
		},
		Channel: onboard.ChannelConfig{Type: "telegram", TokenRefs: []string{"telegram_bot_token"}},
	}
	m := secretRefsByEnv(cfg)
	for env, want := range map[string]string{
		"ANTHROPIC_API_KEY":  "anthropic_api_key",
		"OPENROUTER_API_KEY": "openrouter_api_key",
		"TELEGRAM_BOT_TOKEN": "telegram_bot_token",
	} {
		if m[env] != want {
			t.Errorf("refByEnv[%q] = %q, want %q", env, m[env], want)
		}
	}
	if _, ok := m["OPENAI_API_KEY"]; ok {
		t.Error("unconfigured provider must not appear")
	}
}

// A slack channel with only one token ref must map the app token and leave the bot
// token unmapped — pinning the len>1 guard against an index-out-of-range panic.
func TestSecretRefsByEnvSlackSingleRef(t *testing.T) {
	cfg := onboard.Config{Channel: onboard.ChannelConfig{Type: "slack", TokenRefs: []string{"slack_app_token"}}}
	m := secretRefsByEnv(cfg)
	if m["SLACK_APP_TOKEN"] != "slack_app_token" {
		t.Errorf("app token = %q", m["SLACK_APP_TOKEN"])
	}
	if _, ok := m["SLACK_BOT_TOKEN"]; ok {
		t.Error("bot token must be absent when only one ref is configured")
	}
}

func TestModelSpecPrecedence(t *testing.T) {
	cases := []struct{ env, exec, want string }{
		{"openrouter", "anthropic:claude-x", "openrouter"}, // NILCORE_MODEL wins
		{"", "anthropic:claude-x", "anthropic:claude-x"},   // then configured executor
		{"", "", "claude-sonnet-4-6"},                      // then built-in default
	}
	for _, c := range cases {
		if got := modelSpec(c.env, c.exec); got != c.want {
			t.Errorf("modelSpec(%q,%q) = %q, want %q", c.env, c.exec, got, c.want)
		}
	}
}

func TestChannelSpecPrecedence(t *testing.T) {
	cases := []struct{ flag, cfg, want string }{
		{"slack", "telegram", "slack"}, // flag wins
		{"", "slack", "slack"},         // then config
		{"", "none", "telegram"},       // "none" → default
		{"", "", "telegram"},           // unset → default
	}
	for _, c := range cases {
		got := channelSpec(c.flag, onboard.Config{Channel: onboard.ChannelConfig{Type: c.cfg}})
		if got != c.want {
			t.Errorf("channelSpec(%q, cfg=%q) = %q, want %q", c.flag, c.cfg, got, c.want)
		}
	}
}

func TestApplyConfigDefaultsRespectsExplicitFlags(t *testing.T) {
	build := func(args []string) (commonFlags, *flag.FlagSet) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		c := registerCommon(fs)
		_ = fs.Parse(args)
		return c, fs
	}
	cfg := onboard.Config{Runtime: "docker", Image: "img:cfg"}

	// No flags: config fills both.
	c, fs := build(nil)
	applyConfigDefaults(c, cfg, flagsSet(fs))
	if *c.runtime != "docker" || *c.image != "img:cfg" {
		t.Errorf("config defaults: runtime=%q image=%q", *c.runtime, *c.image)
	}
	// Explicit -runtime wins; image still from config.
	c, fs = build([]string{"-runtime", "podman"})
	applyConfigDefaults(c, cfg, flagsSet(fs))
	if *c.runtime != "podman" || *c.image != "img:cfg" {
		t.Errorf("explicit flag: runtime=%q image=%q", *c.runtime, *c.image)
	}
}

// TestResolveDelegatedEnvOverrides covers the R1 precedence rule: NILCORE_<CLI>_MODEL
// / _EFFORT override the config file, an empty env var does NOT clobber a configured
// value, ExtraArgs/Env pass through untouched, the input config is not mutated (it is
// taken by value), and the prefix isolates one CLI's env from the other's.
func TestResolveDelegatedEnvOverrides(t *testing.T) {
	base := onboard.DelegatedConfig{
		Model:     "cfg-model",
		Effort:    "cfg-effort",
		ExtraArgs: []string{"-c", "k=v"},
		Env:       map[string]string{"CODEX_HOME": "/work/.codex"},
	}

	t.Run("env unset keeps config", func(t *testing.T) {
		got := resolveDelegated("NILCORE_CODEX", base)
		if got.Model != "cfg-model" || got.Effort != "cfg-effort" {
			t.Errorf("unset env should preserve config: model=%q effort=%q", got.Model, got.Effort)
		}
	})

	t.Run("model and effort env override", func(t *testing.T) {
		t.Setenv("NILCORE_CODEX_MODEL", "env-model")
		t.Setenv("NILCORE_CODEX_EFFORT", "env-effort")
		got := resolveDelegated("NILCORE_CODEX", base)
		if got.Model != "env-model" || got.Effort != "env-effort" {
			t.Errorf("env should win: model=%q effort=%q", got.Model, got.Effort)
		}
	})

	t.Run("empty env var does not clobber config", func(t *testing.T) {
		t.Setenv("NILCORE_CODEX_MODEL", "")
		got := resolveDelegated("NILCORE_CODEX", base)
		if got.Model != "cfg-model" {
			t.Errorf("empty env should keep config model, got %q", got.Model)
		}
	})

	t.Run("extra args and env pass through; input not mutated", func(t *testing.T) {
		t.Setenv("NILCORE_CODEX_MODEL", "env-model")
		got := resolveDelegated("NILCORE_CODEX", base)
		if len(got.ExtraArgs) != 2 || got.ExtraArgs[0] != "-c" {
			t.Errorf("ExtraArgs should pass through, got %v", got.ExtraArgs)
		}
		if got.Env["CODEX_HOME"] != "/work/.codex" {
			t.Errorf("Env should pass through, got %v", got.Env)
		}
		if base.Model != "cfg-model" {
			t.Errorf("input config mutated (should be by value): base.Model=%q", base.Model)
		}
	})

	t.Run("prefix isolates CLIs", func(t *testing.T) {
		t.Setenv("NILCORE_CLAUDE_MODEL", "claude-only")
		got := resolveDelegated("NILCORE_CODEX", base)
		if got.Model != "cfg-model" {
			t.Errorf("CLAUDE env leaked into CODEX resolution: got %q", got.Model)
		}
	})
}
