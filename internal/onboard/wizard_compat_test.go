package onboard

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestWizardConfiguresCompat drives the wizard through the optional
// OpenAI-compatible endpoint flow (offered after the write confirmation, so the
// legacy linear prompt sequence is untouched): opt in, supply a base URL, pick the
// bearer scheme, and name a dedicated key env var. It asserts the endpoint knobs
// land on the config, the provider is recorded by NAME only, and no secret VALUE
// is ever entered (the operator exports the key in their environment).
func TestWizardConfiguresCompat(t *testing.T) {
	// runtime, image, backend, anthropic, openai, openrouter, executor, advisor,
	// channel(none), web(n), codex, confirm(Y), compat-optin(y), base-url, auth,
	// key-env.
	input := "\n\n\n\n\n\n\n\nnone\n\n\n\ny\nhttps://vllm.internal/v1\nbearer\nMY_LOCAL_KEY\n"
	store := newMapStore()
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: store}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.BaseURL != "https://vllm.internal/v1" || cfg.AuthScheme != "bearer" {
		t.Errorf("compat endpoint = base %q scheme %q", cfg.BaseURL, cfg.AuthScheme)
	}
	if cfg.CompatKeyEnv != "MY_LOCAL_KEY" {
		t.Errorf("compat key env = %q, want MY_LOCAL_KEY", cfg.CompatKeyEnv)
	}
	if !cfg.hasCompatProvider() {
		t.Fatalf("compat provider not recorded: %+v", cfg.Providers)
	}
	for _, p := range cfg.Providers {
		if p.Name == compatProvider && p.KeyRef != "MY_LOCAL_KEY" {
			t.Errorf("compat provider key_ref = %q, want the env NAME MY_LOCAL_KEY", p.KeyRef)
		}
	}
	// No secret value was ever entered; the store holds nothing for the compat key.
	if v, _ := store.Get("MY_LOCAL_KEY"); v != "" {
		t.Errorf("wizard must not store a compat key VALUE, got %q", v)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("configured compat must validate: %v", err)
	}
}

// TestWizardCompatRejectsCanonicalKeyEnv proves the wizard refuses a first-party
// vendor key name for the compat endpoint (anti-exfiltration, I3) and falls back
// to the dedicated default rather than persisting the forbidden name. The result
// validates (a canonical name would otherwise be rejected key-free).
func TestWizardCompatRejectsCanonicalKeyEnv(t *testing.T) {
	// ...confirm(Y), compat-optin(y), base-url, auth(bearer), key-env(ANTHROPIC_API_KEY).
	input := "\n\n\n\n\n\n\n\nnone\n\n\n\ny\nhttps://vllm.internal/v1\nbearer\nANTHROPIC_API_KEY\n"
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: newMapStore()}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.CompatKeyEnv != defaultCompatKeyEnv {
		t.Errorf("canonical key env must be refused and defaulted, got %q", cfg.CompatKeyEnv)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("wizard must not persist a canonical key env (would fail Validate): %v", err)
	}
}

// TestWizardCompatKeylessNone proves the "none" auth scheme (keyless local
// servers like Ollama/vLLM) skips the key-env prompt entirely, recording the
// default name as a harmless reference, and validates.
func TestWizardCompatKeylessNone(t *testing.T) {
	// ...confirm(Y), compat-optin(y), base-url, auth(none).  No key-env prompt.
	input := "\n\n\n\n\n\n\n\nnone\n\n\n\ny\nhttp://localhost:11434/v1\nnone\n"
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: newMapStore()}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.AuthScheme != "none" || cfg.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("keyless compat = base %q scheme %q", cfg.BaseURL, cfg.AuthScheme)
	}
	if cfg.CompatKeyEnv != defaultCompatKeyEnv {
		t.Errorf("keyless compat key_ref = %q, want default reference %q", cfg.CompatKeyEnv, defaultCompatKeyEnv)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("keyless compat must validate: %v", err)
	}
}

// TestWizardSkipCompatLeavesConfigClean proves declining the optional compat
// prompt (the default) leaves every new field at its zero value, so the written
// config is byte-identical to a pre-P15 config.
func TestWizardSkipCompatLeavesConfigClean(t *testing.T) {
	// ...confirm(Y), compat-optin(n).
	input := "\n\n\nsk-ant-1\n\n\n\n\nnone\n\n\n\nn\n"
	w := &Wizard{In: strings.NewReader(input), Out: io.Discard, Secrets: newMapStore()}

	cfg, err := w.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.BaseURL != "" || cfg.AuthScheme != "" || cfg.CompatKeyEnv != "" {
		t.Errorf("declined compat must leave fields zero, got %+v", cfg)
	}
	if cfg.hasCompatProvider() {
		t.Errorf("declined compat must not record a provider: %+v", cfg.Providers)
	}
	// Marshal must not surface any compat keys (they are omitempty + unset).
	b, _ := json.Marshal(cfg)
	if strings.Contains(string(b), "base_url") || strings.Contains(string(b), "compat_key_env") {
		t.Errorf("declined compat leaked empty knob keys into JSON:\n%s", b)
	}
}

// TestFromEnvCompat proves non-interactive provisioning configures the compat
// endpoint from NILCORE_COMPAT_* names, records only the key-env NAME, and seeds a
// sensible compat model default when no other provider key is present.
func TestFromEnvCompat(t *testing.T) {
	env := map[string]string{
		"NILCORE_COMPAT_BASE_URL":    "https://groq.example/openai/v1",
		"NILCORE_COMPAT_AUTH_SCHEME": "bearer",
		"NILCORE_COMPAT_KEY_ENV":     "GROQ_KEY",
		"GROQ_KEY":                   "secret-value-should-not-persist",
	}
	store := newMapStore()
	cfg, err := FromEnv(func(k string) string { return env[k] }, store)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.BaseURL != "https://groq.example/openai/v1" || cfg.CompatKeyEnv != "GROQ_KEY" {
		t.Errorf("compat env not provisioned: %+v", cfg)
	}
	if !cfg.hasCompatProvider() {
		t.Fatalf("compat provider not recorded: %+v", cfg.Providers)
	}
	// Compat-only setup ⇒ executor defaults to the compat model spec, not anthropic.
	if cfg.Executor != execDefaults[compatProvider] {
		t.Errorf("executor = %q, want compat default %q", cfg.Executor, execDefaults[compatProvider])
	}
	// The key VALUE must never be read into the store (operator's env holds it).
	for k, v := range store.m {
		if strings.Contains(v, "secret-value-should-not-persist") {
			t.Fatalf("FromEnv leaked a compat key VALUE under %q", k)
		}
	}
}
