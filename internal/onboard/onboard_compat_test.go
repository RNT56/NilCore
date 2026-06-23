package onboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidProvidersIncludesCompat pins that the closed provider vocabulary now
// admits the operator-typed openai-compatible vendor (and the canonical default
// key-env name is what the package documents).
func TestValidProvidersIncludesCompat(t *testing.T) {
	if !validProviders[compatProvider] {
		t.Fatalf("validProviders must include %q", compatProvider)
	}
	if defaultCompatKeyEnv != "NILCORE_COMPAT_API_KEY" {
		t.Fatalf("defaultCompatKeyEnv = %q, want NILCORE_COMPAT_API_KEY", defaultCompatKeyEnv)
	}
}

// TestCompatConfigRoundTrip proves a config carrying the full P15 provider-knob
// set survives Save→Load through the strict (DisallowUnknownFields) decoder
// unchanged: every additive key is recognized, not rejected as a typo, and the
// values come back intact.
func TestCompatConfigRoundTrip(t *testing.T) {
	parallel := false
	cfg := Config{
		Version:           CurrentConfigVersion,
		Providers:         []ProviderConfig{{Name: compatProvider, KeyRef: "MY_LOCAL_KEY"}},
		Executor:          "openai-compatible:llama-3.3-70b",
		Runtime:           "podman",
		Channel:           ChannelConfig{Type: "none"},
		BaseURL:           "https://vllm.internal/v1",
		AuthScheme:        "bearer",
		CompatKeyEnv:      "MY_LOCAL_KEY",
		MaxTokensField:    "max_completion_tokens",
		ReasoningEffort:   "high",
		OpenRouterReferer: "https://example.com",
		OpenRouterTitle:   "NilCore",
		Routing: RoutingConfig{
			ServiceTier:       "flex",
			PromptCacheKey:    "nilcore-prefix",
			ParallelToolCalls: &parallel,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid compat config rejected: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.BaseURL != cfg.BaseURL || got.AuthScheme != "bearer" || got.CompatKeyEnv != "MY_LOCAL_KEY" {
		t.Errorf("compat endpoint fields lost: %+v", got)
	}
	if got.MaxTokensField != "max_completion_tokens" || got.ReasoningEffort != "high" {
		t.Errorf("token/effort fields lost: field=%q effort=%q", got.MaxTokensField, got.ReasoningEffort)
	}
	if got.OpenRouterReferer != "https://example.com" || got.OpenRouterTitle != "NilCore" {
		t.Errorf("openrouter attribution lost: referer=%q title=%q", got.OpenRouterReferer, got.OpenRouterTitle)
	}
	if got.Routing.ServiceTier != "flex" || got.Routing.PromptCacheKey != "nilcore-prefix" {
		t.Errorf("routing lost: %+v", got.Routing)
	}
	if got.Routing.ParallelToolCalls == nil || *got.Routing.ParallelToolCalls != false {
		t.Errorf("parallel_tool_calls pointer lost: %v", got.Routing.ParallelToolCalls)
	}
}

// TestV1ConfigMigratesCleanly is the core migration guarantee: a config written by
// a prior schema version (v1, none of the P15 fields present) loads through the
// strict decoder, is stamped to the current version, and validates — proving the
// version bump did not break older configs. The additive defaults leave behavior
// byte-identical (no new field is set).
func TestV1ConfigMigratesCleanly(t *testing.T) {
	if CurrentConfigVersion <= 1 {
		t.Fatalf("CurrentConfigVersion must be bumped past 1, got %d", CurrentConfigVersion)
	}
	p := filepath.Join(t.TempDir(), "v1.json")
	// A representative pre-P15 (v1) config: no base_url/auth_scheme/compat_key_env/
	// reasoning_effort/routing keys at all.
	body := `{"version":1,"runtime":"podman","backend":"native",` +
		`"providers":[{"name":"anthropic","key_ref":"anthropic_api_key"}],` +
		`"executor":"anthropic:claude-sonnet-4-6","channel":{"type":"none"}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("v1 config must migrate cleanly, got: %v", err)
	}
	if got.Version != CurrentConfigVersion {
		t.Errorf("migrated version = %d, want %d", got.Version, CurrentConfigVersion)
	}
	// The new fields are absent in the source ⇒ zero values, behavior unchanged.
	if got.BaseURL != "" || got.AuthScheme != "" || got.CompatKeyEnv != "" || got.ReasoningEffort != "" {
		t.Errorf("absent P15 fields must decode to zero values, got %+v", got)
	}
	if (got.Routing != RoutingConfig{}) {
		t.Errorf("absent routing block must decode to zero, got %+v", got.Routing)
	}
}

// TestCompatRejectsCanonicalKeyEnv proves Validate refuses a first-party vendor
// key name as the compat endpoint's key-env (anti-exfiltration, I3) — a real
// vendor key may never be forwarded to an operator-typed self-hosted endpoint. The
// rejection is key-free: the error names the variable, never any value.
func TestCompatRejectsCanonicalKeyEnv(t *testing.T) {
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		cfg := Config{
			Version:      CurrentConfigVersion,
			Runtime:      "podman",
			Channel:      ChannelConfig{Type: "none"},
			Providers:    []ProviderConfig{{Name: compatProvider, KeyRef: name}},
			BaseURL:      "https://vllm.internal/v1",
			AuthScheme:   "bearer",
			CompatKeyEnv: name,
		}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("compat_key_env %q (a first-party vendor key) must be rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should name the rejected variable %q, got %v", name, err)
		}
	}
}

// TestCompatRequiresEndpointAndKeyEnv proves a compat provider is rejected
// key-free: without a base URL, or (for a keyed scheme) without a dedicated
// key-env name, Validate fails loudly at config time rather than at run time. A
// keyless ("none") scheme needs no key-env.
func TestCompatRequiresEndpointAndKeyEnv(t *testing.T) {
	base := Config{Version: CurrentConfigVersion, Runtime: "podman", Channel: ChannelConfig{Type: "none"}}

	noURL := base
	noURL.Providers = []ProviderConfig{{Name: compatProvider, KeyRef: defaultCompatKeyEnv}}
	noURL.AuthScheme = "bearer"
	noURL.CompatKeyEnv = defaultCompatKeyEnv
	if err := noURL.Validate(); err == nil {
		t.Error("compat provider without base_url must be rejected")
	}

	noKeyEnv := base
	noKeyEnv.Providers = []ProviderConfig{{Name: compatProvider, KeyRef: "x"}}
	noKeyEnv.BaseURL = "https://vllm.internal/v1"
	noKeyEnv.AuthScheme = "bearer"
	noKeyEnv.CompatKeyEnv = "" // keyed scheme, but no dedicated key-env name
	if err := noKeyEnv.Validate(); err == nil {
		t.Error("keyed compat provider without compat_key_env must be rejected")
	}

	keyless := base
	keyless.Providers = []ProviderConfig{{Name: compatProvider, KeyRef: defaultCompatKeyEnv}}
	keyless.BaseURL = "http://localhost:11434/v1"
	keyless.AuthScheme = "none" // keyless local server — no key required
	keyless.CompatKeyEnv = defaultCompatKeyEnv
	if err := keyless.Validate(); err != nil {
		t.Errorf("keyless (none) compat provider with a base_url must be valid, got %v", err)
	}
}

// TestValidateRejectsBadProviderKnobs proves the new knob fields are validated:
// an unknown auth scheme, reasoning effort, or service tier each fails loudly,
// while their empty (absent) values are accepted.
func TestValidateRejectsBadProviderKnobs(t *testing.T) {
	base := Config{Version: CurrentConfigVersion, Runtime: "podman", Channel: ChannelConfig{Type: "none"},
		Providers: []ProviderConfig{{Name: "anthropic", KeyRef: "k"}}}

	if err := base.Validate(); err != nil {
		t.Fatalf("base config (no knobs) must validate: %v", err)
	}
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"auth_scheme", func(c *Config) { c.AuthScheme = "oauth2" }},
		{"reasoning_effort", func(c *Config) { c.ReasoningEffort = "extreme" }},
		{"service_tier", func(c *Config) { c.Routing.ServiceTier = "platinum" }},
	}
	for _, b := range bad {
		c := base
		b.mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", b.name)
		}
	}
	good := []struct {
		name string
		mut  func(*Config)
	}{
		{"auth_scheme bearer", func(c *Config) { c.AuthScheme = "bearer" }},
		{"auth_scheme azure", func(c *Config) { c.AuthScheme = "azure" }},
		{"reasoning_effort minimal", func(c *Config) { c.ReasoningEffort = "minimal" }},
		{"service_tier priority", func(c *Config) { c.Routing.ServiceTier = "priority" }},
	}
	for _, g := range good {
		c := base
		g.mut(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: expected valid, got %v", g.name, err)
		}
	}
}

// TestCompatConfigNeverSerializesSecretValue is the I3 guard at the config layer:
// the only secret-shaped data a compat config holds is the env-var NAME / KeyRef.
// No matter what is set, the serialized config must never contain a key VALUE — we
// stuff an obvious sentinel "value" into the env and confirm it never appears.
func TestCompatConfigNeverSerializesSecretValue(t *testing.T) {
	const sentinel = "sk-super-secret-value-DO-NOT-PERSIST"
	store := newMapStore()
	// Provision via FromEnv with the sentinel sitting in the *value-bearing* env var.
	// FromEnv must record only the NAME (NILCORE_COMPAT_API_KEY), never read/store
	// the value.
	env := map[string]string{
		"NILCORE_COMPAT_BASE_URL":    "https://vllm.internal/v1",
		"NILCORE_COMPAT_API_KEY":     sentinel, // the VALUE — must never be persisted
		"NILCORE_COMPAT_KEY_ENV":     "NILCORE_COMPAT_API_KEY",
		"NILCORE_COMPAT_AUTH_SCHEME": "bearer",
	}
	cfg, err := FromEnv(func(k string) string { return env[k] }, store)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.CompatKeyEnv != "NILCORE_COMPAT_API_KEY" {
		t.Errorf("compat key env name = %q, want NILCORE_COMPAT_API_KEY", cfg.CompatKeyEnv)
	}
	if !cfg.hasCompatProvider() {
		t.Fatalf("compat provider not recorded: %+v", cfg.Providers)
	}
	// The SecretStore must NOT have captured the compat key value — the operator's
	// environment holds it; FromEnv records only the name.
	if v, _ := store.Get("NILCORE_COMPAT_API_KEY"); strings.Contains(v, sentinel) {
		t.Errorf("compat key VALUE leaked into the secret store")
	}
	for _, v := range store.m {
		if strings.Contains(v, sentinel) {
			t.Fatalf("secret VALUE leaked into the store")
		}
	}
	// Serialized config must reference names only — never the value.
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), sentinel) {
		t.Fatalf("config leaked a secret VALUE:\n%s", b)
	}
	// And it must round-trip through the strict decoder.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("on-disk config leaked a secret VALUE:\n%s", raw)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("compat config must reload cleanly: %v", err)
	}
}
