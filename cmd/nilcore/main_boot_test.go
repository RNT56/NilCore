package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/onboard"
	"nilcore/internal/secrets"
)

// writableStore is a map-backed SecretStore for the chain/handshake tests.
type writableStore struct{ m map[string]string }

func (s *writableStore) Get(name string) (string, error) {
	if v, ok := s.m[name]; ok {
		return v, nil
	}
	return "", secrets.ErrNotFound
}
func (s *writableStore) Set(name, value string) error { s.m[name] = value; return nil }
func (s *writableStore) Delete(name string) error     { delete(s.m, name); return nil }
func (s *writableStore) Name() string                 { return "map" }

func TestUsageListsCommands(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	for _, want := range []string{"init", "serve", "doctor", "config show", "secret set", "version", "NilCore"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

func TestVersionString(t *testing.T) {
	if !strings.HasPrefix(versionString(), "nilcore ") {
		t.Errorf("versionString = %q", versionString())
	}
}

func TestResolveProviderErrors(t *testing.T) {
	// Unknown backend.
	if _, err := resolveProvider("bogus", boot{}); err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("unknown backend: %v", err)
	}
	// Delegated backends need no model provider.
	if p, err := resolveProvider("codex", boot{}); err != nil || p != nil {
		t.Errorf("codex: %v, %v", p, err)
	}
	// Native with no resolvable key reports the actionable remedy.
	b := boot{cfg: onboard.Config{Executor: "anthropic:claude-sonnet-4-6"}, cred: func(string) string { return "" }}
	_, err := resolveProvider("native", b)
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !strings.Contains(err.Error(), "nilcore init") {
		t.Errorf("native missing key error = %v", err)
	}
}

func TestBuildChannelErrors(t *testing.T) {
	empty := func(string) string { return "" }
	if _, _, err := buildChannel("telegram", empty, []string{"u"}, nil); err == nil || !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Errorf("telegram missing token: %v", err)
	}
	oneSlack := func(env string) string {
		if env == "SLACK_APP_TOKEN" {
			return "xapp"
		}
		return ""
	}
	if _, _, err := buildChannel("slack", oneSlack, []string{"u"}, nil); err == nil || !strings.Contains(err.Error(), "SLACK_BOT_TOKEN") {
		t.Errorf("slack missing bot token: %v", err)
	}
	if _, _, err := buildChannel("irc", empty, []string{"u"}, nil); err == nil || !strings.Contains(err.Error(), "unknown channel") {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestPrincipalAllowlistMergeAndDedup(t *testing.T) {
	t.Setenv("NILCORE_ALLOWLIST", "a,,b, ")
	cfg := onboard.Config{Channel: onboard.ChannelConfig{Allow: []string{"b", "c"}}}
	got := principalAllowlist(cfg)
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("allowlist = %v, want [a b c]", got)
	}
}

func TestPrincipalAllowlistEmpty(t *testing.T) {
	t.Setenv("NILCORE_ALLOWLIST", "  ,  ")
	if got := principalAllowlist(onboard.Config{}); len(got) != 0 {
		t.Errorf("whitespace-only allowlist must be empty, got %v", got)
	}
}

func TestApplyConfigDefaultsBackend(t *testing.T) {
	build := func(args []string) (commonFlags, map[string]bool) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		c := registerCommon(fs)
		_ = fs.Parse(args)
		return c, flagsSet(fs)
	}
	cfg := onboard.Config{Backend: "codex"}

	c, set := build(nil)
	applyConfigDefaults(c, cfg, set)
	if *c.backendName != "codex" {
		t.Errorf("config backend not applied: %q", *c.backendName)
	}
	c, set = build([]string{"-backend", "native"})
	applyConfigDefaults(c, cfg, set)
	if *c.backendName != "native" {
		t.Errorf("explicit -backend must win: %q", *c.backendName)
	}
}

func TestDiagnose(t *testing.T) {
	cfg := onboard.Config{
		Providers: []onboard.ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}},
		Executor:  "anthropic:claude-sonnet-4-6",
		Runtime:   "podman",
	}
	report, ready := diagnose(cfg, func(env string) string {
		if env == "ANTHROPIC_API_KEY" {
			return "sk-x"
		}
		return ""
	}, nil)
	if !ready {
		t.Errorf("should be ready:\n%s", report)
	}
	if !strings.Contains(report, "anthropic key resolves") {
		t.Errorf("report missing credential line:\n%s", report)
	}

	_, ready = diagnose(cfg, func(string) string { return "" }, nil)
	if ready {
		t.Error("no resolvable key → not ready")
	}
	_, ready = diagnose(onboard.Config{}, func(string) string { return "" }, nil)
	if ready {
		t.Error("no providers → not ready")
	}
}

// TestDiagnoseLiveCheck proves the optional live check folds into run-readiness:
// a failing probe makes the host not-ready even when the key resolves.
func TestDiagnoseLiveCheck(t *testing.T) {
	cfg := onboard.Config{
		Backend:   "native",
		Providers: []onboard.ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}},
		Executor:  "anthropic:claude-sonnet-4-6",
	}
	cred := func(env string) string {
		if env == "ANTHROPIC_API_KEY" {
			return "sk"
		}
		return ""
	}
	report, ready := diagnose(cfg, cred, func(string) error { return errors.New("401 unauthorized") })
	if ready {
		t.Error("failing live check must make the host not-ready")
	}
	if !strings.Contains(report, "responds") || !strings.Contains(report, "401") {
		t.Errorf("report should show the live failure:\n%s", report)
	}
	if _, ready := diagnose(cfg, cred, func(string) error { return nil }); !ready {
		t.Error("passing live check should be ready")
	}
}

func TestLiveSpecs(t *testing.T) {
	native := onboard.Config{Backend: "native", Executor: "anthropic:x", Advisor: "anthropic:y"}
	if got := liveSpecs(native); strings.Join(got, ",") != "anthropic:x,anthropic:y" {
		t.Errorf("native specs = %v", got)
	}
	// A distinct advisor is added once; an advisor equal to the executor is not duplicated.
	same := onboard.Config{Backend: "native", Executor: "anthropic:x", Advisor: "anthropic:x"}
	if got := liveSpecs(same); len(got) != 1 {
		t.Errorf("duplicate advisor should collapse, got %v", got)
	}
	if got := liveSpecs(onboard.Config{Backend: "codex", Executor: "anthropic:x"}); got != nil {
		t.Errorf("delegated backend has no live spec, got %v", got)
	}
}

// TestDiagnoseBackendAware proves doctor's run-readiness keys on the configured
// backend's credential, not merely on some provider resolving: a codex backend
// with only an anthropic key is NOT ready.
func TestDiagnoseBackendAware(t *testing.T) {
	cfg := onboard.Config{
		Backend:   "codex",
		Providers: []onboard.ProviderConfig{{Name: "anthropic", KeyRef: "anthropic_api_key"}},
		Executor:  "anthropic:claude-sonnet-4-6",
	}
	anthropicOnly := func(env string) string {
		if env == "ANTHROPIC_API_KEY" {
			return "sk-a"
		}
		return ""
	}
	if _, ready := diagnose(cfg, anthropicOnly, nil); ready {
		t.Error("codex backend with no CODEX_API_KEY must not be ready")
	}
	withCodex := func(env string) string {
		if env == "CODEX_API_KEY" {
			return "sk-codex"
		}
		return ""
	}
	if _, ready := diagnose(cfg, withCodex, nil); !ready {
		t.Error("codex backend with CODEX_API_KEY must be ready")
	}
}

// TestAssembleStoreReadDoesNotProvisionKey proves the read path never writes a
// fresh master key: if the vault exists but its key file is gone, the read store
// is env-only and no key is recreated (which could not decrypt the vault anyway).
func TestAssembleStoreReadDoesNotProvisionKey(t *testing.T) {
	dir := t.TempDir()
	w := assembleStore(dir, true, secrets.EnvStore{}) // provisions key + vault
	if err := w.Set("anthropic_api_key", "sk"); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "secrets.key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	s := assembleStore(dir, false, secrets.EnvStore{})
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Error("read path must not recreate the master key file")
	}
	if s.Name() != "env" {
		t.Errorf("read store with no usable vault = %q, want env", s.Name())
	}
}

func TestAssembleStoreReadCreatesNoFiles(t *testing.T) {
	dir := t.TempDir()
	s := assembleStore(dir, false, secrets.EnvStore{})
	if s.Name() != "env" {
		t.Fatalf("read store on empty dir = %q, want env", s.Name())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("read path must not create files, found: %v", entries)
	}
}

func TestAssembleStoreWriteThenReadHandshake(t *testing.T) {
	dir := t.TempDir()
	w := assembleStore(dir, true, secrets.EnvStore{}) // no keychain → file vault
	if w.Name() != "file" {
		t.Fatalf("write store = %q, want file", w.Name())
	}
	if err := w.Set("anthropic_api_key", "sk-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	r := assembleStore(dir, false, secrets.EnvStore{}) // vault now exists → chain finds it
	got, err := r.Get("anthropic_api_key")
	if err != nil || got != "sk-secret" {
		t.Fatalf("read-back = %q, %v; want the stored secret", got, err)
	}
}

func TestPassphraseVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := passphraseVault(dir, "pw", true) // first-time: generates the salt
	if w.Name() != "file" {
		t.Fatalf("write store = %q, want file", w.Name())
	}
	if err := w.Set("k", "v"); err != nil {
		t.Fatal(err)
	}
	if !passphraseInUse(dir) {
		t.Error("salt file should mark passphrase mode")
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.key")); !os.IsNotExist(err) {
		t.Error("passphrase mode must not write a key file")
	}
	if got, err := passphraseVault(dir, "pw", false).Get("k"); err != nil || got != "v" {
		t.Fatalf("read-back = %q, %v", got, err)
	}
	if _, err := passphraseVault(dir, "wrong", false).Get("k"); err == nil {
		t.Error("a wrong passphrase must fail to decrypt")
	}
	if passphraseVault(dir, "", false).Name() != "env" {
		t.Error("empty passphrase must degrade to the env store")
	}
}

func TestWriteStoreSelectsPassphrase(t *testing.T) {
	t.Setenv("NILCORE_VAULT_PASSPHRASE", "pw")
	dir := t.TempDir()
	if s := writeStore(dir, true, false); s.Name() != "file" || !passphraseInUse(dir) {
		t.Fatalf("requested passphrase: store=%q salt=%v", s.Name(), passphraseInUse(dir))
	}
	// Once the salt exists the mode is auto-detected, even without re-requesting it.
	if s := writeStore(dir, false, false); s.Name() != "file" {
		t.Errorf("auto-detected passphrase: store=%q", s.Name())
	}
}

func TestAssembleStoreReadsPassphraseVault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NILCORE_VAULT_PASSPHRASE", "pw")
	w := passphraseVault(dir, "pw", true)
	if err := w.Set("anthropic_api_key", "sk"); err != nil {
		t.Fatal(err)
	}
	if got, err := assembleStore(dir, false, secrets.EnvStore{}).Get("anthropic_api_key"); err != nil || got != "sk" {
		t.Fatalf("read path should resolve the passphrase vault: %q, %v", got, err)
	}
	// Without the passphrase the vault is skipped → env-only (no silent decrypt failure path).
	t.Setenv("NILCORE_VAULT_PASSPHRASE", "")
	if s := assembleStore(dir, false, secrets.EnvStore{}); s.Name() != "env" {
		t.Errorf("no passphrase → vault skipped, got %q", s.Name())
	}
}

func TestChainStore(t *testing.T) {
	miss := secrets.EnvStore{} // read-only: Get misses, Set is rejected
	hit := &writableStore{m: map[string]string{"x": "1"}}
	c := chainStore{stores: []secrets.SecretStore{miss, hit}}

	if v, err := c.Get("x"); err != nil || v != "1" {
		t.Errorf("Get x = %q, %v", v, err)
	}
	if _, err := c.Get("missing"); err == nil {
		t.Error("missing key must error")
	}
	if err := c.Set("z", "9"); err != nil {
		t.Errorf("Set: %v", err)
	}
	if hit.m["z"] != "9" {
		t.Error("Set must land in the first writable backend")
	}
	if c.Name() != "env+map" {
		t.Errorf("Name = %q, want env+map", c.Name())
	}
}
