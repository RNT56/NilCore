package onboard

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/pool"
	"nilcore/internal/secrets"
)

// mapStore is a fake SecretStore for tests.
type mapStore struct{ m map[string]string }

func newMapStore() *mapStore { return &mapStore{m: map[string]string{}} }

func (s *mapStore) Get(name string) (string, error) {
	v, ok := s.m[name]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (s *mapStore) Set(name, value string) error { s.m[name] = value; return nil }
func (s *mapStore) Delete(name string) error     { delete(s.m, name); return nil }
func (s *mapStore) Name() string                 { return "map" }

func TestWizardRun(t *testing.T) {
	// runtime, image, backend, anthropic, openai, openrouter, executor, advisor,
	// channel, telegram-token, allowlist, web(n), codex, confirm.
	input := "docker\n\n\nsk-ant-123\n\n\n\n\ntelegram\ntg-token-456\n123, 456\n\n\n\n"
	store := newMapStore()
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: store}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Runtime != "docker" || cfg.Image != DefaultImage {
		t.Errorf("runtime=%q image=%q", cfg.Runtime, cfg.Image)
	}
	if cfg.Backend != "native" {
		t.Errorf("backend=%q, want native (default)", cfg.Backend)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "anthropic" || cfg.Providers[0].KeyRef != "anthropic_api_key" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	if cfg.Executor != "anthropic:claude-sonnet-4-6" || cfg.Advisor != "anthropic:claude-opus-4-8" {
		t.Errorf("executor=%q advisor=%q", cfg.Executor, cfg.Advisor)
	}
	if cfg.Channel.Type != "telegram" || len(cfg.Channel.TokenRefs) != 1 {
		t.Errorf("channel = %+v", cfg.Channel)
	}
	if strings.Join(cfg.Channel.Allow, ",") != "123,456" {
		t.Errorf("allowlist = %v, want [123 456]", cfg.Channel.Allow)
	}

	// Secrets went to the store, by name.
	if store.m["anthropic_api_key"] != "sk-ant-123" || store.m["telegram_bot_token"] != "tg-token-456" {
		t.Errorf("secrets not stored: %v", store.m)
	}
	// The config holds references only — never the secret values.
	b, _ := json.Marshal(cfg)
	if strings.Contains(string(b), "sk-ant-123") || strings.Contains(string(b), "tg-token-456") {
		t.Fatal("config leaked a secret value")
	}
}

// TestWizardRunSlack exercises the slack branch (two sequential token captures)
// and asserts the token refs land in app-then-bot order, which secretRefsByEnv
// maps positionally to SLACK_APP_TOKEN / SLACK_BOT_TOKEN.
func TestWizardRunSlack(t *testing.T) {
	// runtime, image, backend, anthropic, openai, openrouter, executor, advisor,
	// channel(slack), app, bot, allowlist, web(n), codex, confirm.
	input := "\n\n\nsk-ant-1\n\n\n\n\nslack\nxapp-1\nxoxb-2\nU123\n\n\n\n"
	store := newMapStore()
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: store}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Channel.Type != "slack" {
		t.Fatalf("channel type = %q", cfg.Channel.Type)
	}
	want := []string{"slack_app_token", "slack_bot_token"}
	if strings.Join(cfg.Channel.TokenRefs, ",") != strings.Join(want, ",") {
		t.Errorf("token refs = %v, want %v", cfg.Channel.TokenRefs, want)
	}
	if store.m["slack_app_token"] != "xapp-1" || store.m["slack_bot_token"] != "xoxb-2" {
		t.Errorf("slack secrets = %v", store.m)
	}
	if strings.Join(cfg.Channel.Allow, ",") != "U123" {
		t.Errorf("allowlist = %v", cfg.Channel.Allow)
	}
}

// TestWizardSlackIncomplete proves that supplying only one of the two slack
// tokens leaves the channel unconfigured rather than mislabeling the entered
// token as the missing one (positional-corruption guard).
func TestWizardSlackIncomplete(t *testing.T) {
	// ...channel(slack), app(blank), bot(xoxb-2), web(n), codex, confirm.
	input := "\n\n\nsk-ant-1\n\n\n\n\nslack\n\nxoxb-2\n\n\n\n"
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: newMapStore()}
	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Channel.Type != "none" {
		t.Errorf("incomplete slack should leave channel unconfigured, got %q", cfg.Channel.Type)
	}
	if len(cfg.Channel.TokenRefs) != 0 {
		t.Errorf("token refs should be empty, got %v", cfg.Channel.TokenRefs)
	}
}

// TestWizardAbort proves declining the final write confirmation returns ErrAborted
// and assembles nothing the caller would persist.
func TestWizardAbort(t *testing.T) {
	// runtime, image, backend, anthropic, openai, openrouter, executor, advisor,
	// channel(none), web(n), codex, confirm(n).
	input := "\n\n\nsk-ant-1\n\n\n\n\nnone\n\n\nn\n"
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: newMapStore()}
	_, err := w.Run()
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("expected ErrAborted, got %v", err)
	}
}

// TestWizardWebEnabled exercises the Web access branch: opting in, listing a host,
// choosing the keyless ddg search, and confirming it lands on the config.
func TestWizardWebEnabled(t *testing.T) {
	// ...channel(none), web(y), hosts, search(ddg), codex, confirm.
	input := "\n\n\nsk-ant-1\n\n\n\n\nnone\ny\ndocs.python.org\nddg\n\n\n"
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: newMapStore()}
	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !cfg.Web.Enabled {
		t.Fatal("web should be enabled")
	}
	if cfg.Web.Search != "ddg" {
		t.Errorf("search = %q, want ddg", cfg.Web.Search)
	}
	if strings.Join(cfg.Web.Allow, ",") != "docs.python.org" {
		t.Errorf("allow = %v", cfg.Web.Allow)
	}
	if cfg.Web.SearchKeyRef != "" {
		t.Errorf("ddg needs no key ref, got %q", cfg.Web.SearchKeyRef)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestFromEnvWeb proves non-interactive provisioning enables web from a Brave key
// (implying the brave backend) and an allowlist.
func TestFromEnvWeb(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "sk-x",
		"BRAVE_API_KEY":     "brave-k",
		"NILCORE_WEB_ALLOW": "docs.io",
	}
	store := newMapStore()
	cfg, err := FromEnv(func(k string) string { return env[k] }, store)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.Web.Enabled || cfg.Web.Search != "brave" {
		t.Errorf("web = %+v, want enabled brave", cfg.Web)
	}
	if cfg.Web.SearchKeyRef != "brave_api_key" || store.m["brave_api_key"] != "brave-k" {
		t.Errorf("brave key not captured: ref=%q store=%v", cfg.Web.SearchKeyRef, store.m)
	}
	if strings.Join(cfg.Web.Allow, ",") != "docs.io" {
		t.Errorf("allow = %v", cfg.Web.Allow)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY":  "sk-x",
		"TELEGRAM_BOT_TOKEN": "tg-y",
		"NILCORE_EXECUTOR":   "openai:gpt-5.5",
	}
	store := newMapStore()
	cfg, err := FromEnv(func(k string) string { return env[k] }, store)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Executor != "openai:gpt-5.5" {
		t.Errorf("executor = %q", cfg.Executor)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].KeyRef != "anthropic_api_key" {
		t.Errorf("providers = %+v", cfg.Providers)
	}
	if cfg.Channel.Type != "telegram" {
		t.Errorf("channel = %+v", cfg.Channel)
	}
	if store.m["anthropic_api_key"] != "sk-x" || store.m["telegram_bot_token"] != "tg-y" {
		t.Errorf("secrets = %v", store.m)
	}
}

// TestFromEnvSlack proves non-interactive provisioning can configure a slack
// channel (both tokens) and an allowlist — at parity with the interactive wizard.
func TestFromEnvSlack(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_API_KEY": "sk-x",
		"SLACK_APP_TOKEN":   "xapp-1",
		"SLACK_BOT_TOKEN":   "xoxb-2",
		"NILCORE_ALLOWLIST": "U1, U2",
	}
	store := newMapStore()
	cfg, err := FromEnv(func(k string) string { return env[k] }, store)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.Channel.Type != "slack" || len(cfg.Channel.TokenRefs) != 2 {
		t.Fatalf("channel = %+v", cfg.Channel)
	}
	if strings.Join(cfg.Channel.Allow, ",") != "U1,U2" {
		t.Errorf("allowlist = %v", cfg.Channel.Allow)
	}
	if store.m["slack_app_token"] != "xapp-1" || store.m["slack_bot_token"] != "xoxb-2" {
		t.Errorf("slack secrets = %v", store.m)
	}
}

func TestSaveLoad(t *testing.T) {
	cfg := Config{
		Providers: []ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}},
		Executor:  "anthropic:claude-sonnet-4-6",
		Runtime:   "podman",
		Channel:   ChannelConfig{Type: "telegram", TokenRefs: []string{"telegram_bot_token"}},
	}
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("config perms = %v, want 0600", info.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Executor != cfg.Executor || got.Channel.Type != "telegram" || len(got.Providers) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Version != CurrentConfigVersion {
		t.Errorf("loaded version = %d, want %d", got.Version, CurrentConfigVersion)
	}
}

func TestValidate(t *testing.T) {
	base := Config{Version: CurrentConfigVersion, Runtime: "podman", Backend: "native",
		Providers: []ProviderConfig{{Name: "anthropic", KeyRef: "k"}}, Channel: ChannelConfig{Type: "none"}}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"version", func(c *Config) { c.Version = 99 }},
		{"runtime", func(c *Config) { c.Runtime = "lxc" }},
		{"backend", func(c *Config) { c.Backend = "magic" }},
		{"channel", func(c *Config) { c.Channel.Type = "discord" }},
		{"provider", func(c *Config) { c.Providers = []ProviderConfig{{Name: "groq", KeyRef: "k"}} }},
		{"keyref", func(c *Config) { c.Providers = []ProviderConfig{{Name: "anthropic", KeyRef: ""}} }},
	}
	for _, b := range bad {
		c := base
		b.mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", b.name)
		}
	}
}

// TestPoolRoundTrip proves a Config carrying a swarm Pool block survives
// Save→Load through the strict (DisallowUnknownFields) decoder unchanged: the
// additive `pool` key is recognized, and its tier specs + caps come back intact.
func TestPoolRoundTrip(t *testing.T) {
	cfg := Config{
		Version:   CurrentConfigVersion,
		Providers: []ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}},
		Executor:  "anthropic:claude-sonnet-4-6",
		Runtime:   "podman",
		Channel:   ChannelConfig{Type: "none"},
		Pool: &pool.PoolConfig{
			Planner:  pool.TierSpec{Spec: "anthropic:claude-opus-4-8", Cap: 4},
			Verifier: pool.TierSpec{Spec: "anthropic:claude-opus-4-8"},
			Worker:   pool.TierSpec{Spec: "anthropic:claude-haiku-4-5", Cap: 40},
			Caps:     map[string]int{"openai:gpt-5.5": 8},
			Jitter:   "750ms",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid pool config rejected: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Pool == nil {
		t.Fatal("pool block lost on round-trip")
	}
	if got.Pool.Worker.Spec != "anthropic:claude-haiku-4-5" || got.Pool.Worker.Cap != 40 {
		t.Errorf("worker tier = %+v, want haiku cap 40", got.Pool.Worker)
	}
	if got.Pool.Caps["openai:gpt-5.5"] != 8 || got.Pool.Jitter != "750ms" {
		t.Errorf("pool extras lost: caps=%v jitter=%q", got.Pool.Caps, got.Pool.Jitter)
	}
}

// TestPoolAbsentParses pins the v1-compatibility promise: a config written before
// P12 (no `pool` key at all) still loads under DisallowUnknownFields, and its Pool
// is nil so no pool clause runs.
func TestPoolAbsentParses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.json")
	body := `{"version":1,"runtime":"podman","providers":[{"name":"anthropic","key_ref":"k"}],"channel":{"type":"none"}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("old config without pool must parse: %v", err)
	}
	if cfg.Pool != nil {
		t.Errorf("absent pool should decode to nil, got %+v", cfg.Pool)
	}
}

// TestValidatePool proves the Validate pool clause is fail-closed: an unknown
// vendor in a tier spec and a negative cap are both loud errors, while a valid
// pool passes. The vendor set is exactly onboard's validProviders.
func TestValidatePool(t *testing.T) {
	base := Config{Version: CurrentConfigVersion, Runtime: "podman", Backend: "native",
		Providers: []ProviderConfig{{Name: "anthropic", KeyRef: "k"}}, Channel: ChannelConfig{Type: "none"}}

	ok := base
	ok.Pool = &pool.PoolConfig{Worker: pool.TierSpec{Spec: "anthropic:claude-haiku-4-5", Cap: 40}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid pool rejected: %v", err)
	}

	unknownVendor := base
	unknownVendor.Pool = &pool.PoolConfig{Worker: pool.TierSpec{Spec: "groq:mixtral"}}
	if err := unknownVendor.Validate(); err == nil {
		t.Error("unknown pool vendor must be rejected")
	}

	negativeCap := base
	negativeCap.Pool = &pool.PoolConfig{Worker: pool.TierSpec{Spec: "anthropic:claude-haiku-4-5", Cap: -1}}
	if err := negativeCap.Validate(); err == nil {
		t.Error("negative pool cap must be rejected")
	}
}

// TestLoadVersioning proves: a legacy (no-version) file loads and is stamped
// current; an unknown field is rejected (typo guard); and a too-new version is
// rejected with an upgrade hint.
func TestLoadVersioning(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	legacy := write("legacy.json", `{"runtime":"podman","providers":[{"name":"anthropic","key_ref":"k"}],"channel":{"type":"none"}}`)
	cfg, err := Load(legacy)
	if err != nil {
		t.Fatalf("legacy load: %v", err)
	}
	if cfg.Version != CurrentConfigVersion {
		t.Errorf("legacy version not stamped: %d", cfg.Version)
	}

	typo := write("typo.json", `{"version":1,"runtimee":"podman","channel":{"type":"none"}}`)
	if _, err := Load(typo); err == nil {
		t.Error("unknown field must be rejected")
	}

	future := write("future.json", `{"version":999,"channel":{"type":"none"}}`)
	if _, err := Load(future); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("too-new version must be rejected with an upgrade hint, got %v", err)
	}
}

// TestReadinessHonesty pins that Readiness flags the serve-only allowlist and the
// executor/provider-key mismatch rather than green-lighting a config that can't run.
func TestReadinessHonesty(t *testing.T) {
	// Executor is anthropic, but only an OpenAI key was captured → ✗ on the
	// executor line; a channel is set but no allowlist → ✗ on the serve line.
	cfg := Config{
		Providers: []ProviderConfig{{Name: "openai", KeyRef: "openai_api_key"}},
		Executor:  "anthropic:claude-sonnet-4-6",
		Runtime:   "podman",
		Channel:   ChannelConfig{Type: "telegram", TokenRefs: []string{"telegram_bot_token"}},
	}
	r := cfg.Readiness()
	if !strings.Contains(r, "✗ executor") {
		t.Errorf("expected executor mismatch flagged:\n%s", r)
	}
	if !strings.Contains(r, "✗ serve allowlist") {
		t.Errorf("expected missing allowlist flagged:\n%s", r)
	}
}

func TestPromptSecretEchoOffPiped(t *testing.T) {
	v, err := PromptSecret("Value", strings.NewReader("s3cr3t\n"), io.Discard)
	if err != nil {
		t.Fatalf("PromptSecret: %v", err)
	}
	if v != "s3cr3t" {
		t.Errorf("got %q, want s3cr3t", v)
	}
}
