package onboard

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	input := "docker\n\nsk-ant-123\n\n\n\n\ntelegram\ntg-token-456\n"
	store := newMapStore()
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: store}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Runtime != "docker" || cfg.Image != DefaultImage {
		t.Errorf("runtime=%q image=%q", cfg.Runtime, cfg.Image)
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
}
