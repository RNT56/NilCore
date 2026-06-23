package provider

import (
	"strings"
	"testing"
)

// fakeEnv returns a getenv func backed by a fixed map, proving resolveCompat
// reads everything through the injected seam (never the process environment).
func fakeEnv(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

// TestResolveCompatResolves checks the happy path: a compat spec resolves an
// OpenAI-compatible adapter using the getenv-supplied base URL and key, and the
// first-colon split preserves a model id containing ':' and '/' verbatim.
func TestResolveCompatResolves(t *testing.T) {
	cases := []struct {
		name      string
		spec      string
		wantModel string
	}{
		{"plain", "openai-compatible:my-model", "my-model"},
		{"alias", "compat:my-model", "my-model"},
		{"slash in model id", "openai-compatible:vendor/model-v2", "vendor/model-v2"},
		{"colon in model id", "openai-compatible:ns:model:tag", "ns:model:tag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := fakeEnv(map[string]string{
				"NILCORE_COMPAT_BASE_URL":    "https://self-hosted.example/v1",
				"NILCORE_COMPAT_API_KEY":     "local-secret",
				"NILCORE_COMPAT_AUTH_SCHEME": "bearer",
			})
			p, err := ResolveWith(c.spec, env)
			if err != nil {
				t.Fatalf("ResolveWith(%q): %v", c.spec, err)
			}
			if p.Model() != c.wantModel {
				t.Errorf("Model() = %q, want %q", p.Model(), c.wantModel)
			}
			oa, ok := p.(*OpenAI)
			if !ok {
				t.Fatalf("provider type = %T, want *OpenAI", p)
			}
			if oa.baseURL != "https://self-hosted.example/v1" {
				t.Errorf("baseURL = %q", oa.baseURL)
			}
		})
	}
}

// TestResolveCompatMissingBaseURL: a missing base URL errors, key-free.
func TestResolveCompatMissingBaseURL(t *testing.T) {
	env := fakeEnv(map[string]string{
		"NILCORE_COMPAT_API_KEY": "local-secret",
	})
	_, err := ResolveWith("openai-compatible:m", env)
	if err == nil {
		t.Fatal("expected error when NILCORE_COMPAT_BASE_URL is empty")
	}
	if strings.Contains(err.Error(), "local-secret") {
		t.Errorf("error leaked the key value: %v", err)
	}
}

// TestResolveCompatAuthSchemes verifies bearer (default), azure, and none map to
// the right header/prefix on the built adapter.
func TestResolveCompatAuthSchemes(t *testing.T) {
	cases := []struct {
		scheme     string
		key        string
		wantHeader string
		wantPrefix string
	}{
		{"", "k", "authorization", "Bearer "},       // default is bearer
		{"bearer", "k", "authorization", "Bearer "}, // explicit bearer
		{"azure", "k", "api-key", ""},
		{"none", "", "", ""},
	}
	for _, c := range cases {
		t.Run("scheme="+c.scheme, func(t *testing.T) {
			m := map[string]string{
				"NILCORE_COMPAT_BASE_URL":    "https://h.example/v1",
				"NILCORE_COMPAT_AUTH_SCHEME": c.scheme,
				"NILCORE_COMPAT_API_KEY":     c.key,
			}
			p, err := ResolveWith("openai-compatible:m", fakeEnv(m))
			if err != nil {
				t.Fatalf("ResolveWith: %v", err)
			}
			oa := p.(*OpenAI)
			if oa.authHeader != c.wantHeader || oa.authPrefix != c.wantPrefix {
				t.Errorf("auth = %q/%q, want %q/%q", oa.authHeader, oa.authPrefix, c.wantHeader, c.wantPrefix)
			}
			if oa.key != c.key {
				t.Errorf("key = %q, want %q", oa.key, c.key)
			}
		})
	}
}

// TestResolveCompatNoneAllowsEmptyKey: the "none" scheme targets keyless local
// servers, so an empty key resolves successfully.
func TestResolveCompatNoneAllowsEmptyKey(t *testing.T) {
	env := fakeEnv(map[string]string{
		"NILCORE_COMPAT_BASE_URL":    "http://localhost:11434/v1",
		"NILCORE_COMPAT_AUTH_SCHEME": "none",
		// no NILCORE_COMPAT_API_KEY set
	})
	p, err := ResolveWith("openai-compatible:llama", env)
	if err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	if p.(*OpenAI).key != "" {
		t.Errorf("expected empty key for none auth, got %q", p.(*OpenAI).key)
	}
}

// TestResolveCompatBearerEmptyKeyErrors: bearer/azure require a key; an empty one
// errors key-free, like the other vendors.
func TestResolveCompatBearerEmptyKeyErrors(t *testing.T) {
	for _, scheme := range []string{"", "bearer", "azure"} {
		env := fakeEnv(map[string]string{
			"NILCORE_COMPAT_BASE_URL":    "https://h.example/v1",
			"NILCORE_COMPAT_AUTH_SCHEME": scheme,
		})
		if _, err := ResolveWith("openai-compatible:m", env); err == nil {
			t.Errorf("scheme=%q: expected error when key is empty", scheme)
		}
	}
}

// TestResolveCompatUnknownAuthScheme: an unsupported scheme errors, key-free.
func TestResolveCompatUnknownAuthScheme(t *testing.T) {
	env := fakeEnv(map[string]string{
		"NILCORE_COMPAT_BASE_URL":    "https://h.example/v1",
		"NILCORE_COMPAT_AUTH_SCHEME": "mtls",
		"NILCORE_COMPAT_API_KEY":     "shh",
	})
	_, err := ResolveWith("openai-compatible:m", env)
	if err == nil {
		t.Fatal("expected error for unknown auth scheme")
	}
	if strings.Contains(err.Error(), "shh") {
		t.Errorf("error leaked the key value: %v", err)
	}
}

// TestResolveCompatDefaultKeyEnv: with NILCORE_COMPAT_KEY_ENV unset, the key is
// read from the default NILCORE_COMPAT_API_KEY.
func TestResolveCompatDefaultKeyEnv(t *testing.T) {
	env := fakeEnv(map[string]string{
		"NILCORE_COMPAT_BASE_URL": "https://h.example/v1",
		"NILCORE_COMPAT_API_KEY":  "default-key",
	})
	p, err := ResolveWith("openai-compatible:m", env)
	if err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	if got := p.(*OpenAI).key; got != "default-key" {
		t.Errorf("key = %q, want default-key", got)
	}
}

// TestResolveCompatCustomKeyEnv: NILCORE_COMPAT_KEY_ENV names a custom variable;
// the key is read from that variable.
func TestResolveCompatCustomKeyEnv(t *testing.T) {
	env := fakeEnv(map[string]string{
		"NILCORE_COMPAT_BASE_URL": "https://h.example/v1",
		"NILCORE_COMPAT_KEY_ENV":  "MY_LOCAL_KEY",
		"MY_LOCAL_KEY":            "custom-secret",
		"NILCORE_COMPAT_API_KEY":  "should-not-be-used",
	})
	p, err := ResolveWith("openai-compatible:m", env)
	if err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	if got := p.(*OpenAI).key; got != "custom-secret" {
		t.Errorf("key = %q, want custom-secret (from the named env)", got)
	}
}

// TestResolveCompatRejectsCanonicalVendorKeyEnv is the anti-exfiltration guard
// (invariant I3): pointing NILCORE_COMPAT_KEY_ENV at a first-party vendor key is
// rejected so a real key can never be shipped to a self-hosted base URL. The
// error names the rejected variable but must NOT contain the secret value.
func TestResolveCompatRejectsCanonicalVendorKeyEnv(t *testing.T) {
	for _, canonical := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY"} {
		t.Run(canonical, func(t *testing.T) {
			secret := "sk-real-first-party-" + canonical
			env := fakeEnv(map[string]string{
				"NILCORE_COMPAT_BASE_URL": "https://operator-typed.evil/v1",
				"NILCORE_COMPAT_KEY_ENV":  canonical,
				canonical:                 secret,
			})
			_, err := ResolveWith("openai-compatible:m", env)
			if err == nil {
				t.Fatalf("expected rejection when NILCORE_COMPAT_KEY_ENV=%s (anti-exfiltration)", canonical)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaked the secret value: %v", err)
			}
		})
	}
}
