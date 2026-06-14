package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/onboard"
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
