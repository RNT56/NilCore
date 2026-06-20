// Package onboard is the `nilcore init` flow: one guided pass that captures
// providers + keys, model tiers, the container runtime, the backend, the chat
// channel + its serve allowlist, and detected delegated CLIs, then writes a JSON
// config holding *references* to secrets — never the secrets themselves (invariant
// I3). The keys go to the SecretStore (P1-T11). Works over SSH (line-based, stdlib
// only). P1-T12.
//
// The on-disk config is a *versioned* schema: Load decodes strictly (unknown
// fields are rejected, so a typo fails loudly), brings an older file forward with
// Migrate, and Validate-s the result before boot trusts it — configuration is
// data, parsed-migrated-validated, never silently "fixed".
package onboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultImage is the sandbox container image suggested by `nilcore init` and the
// fallback when a run sets none — a minimal, widely-pullable base. It matches the
// run path's default so a config written by the wizard is usable as-is.
const DefaultImage = "docker.io/library/debian:stable-slim"

// CurrentConfigVersion is the schema version this build reads and writes. Load
// migrates any older config to this version; anything newer is rejected with a
// clear "upgrade NilCore" error rather than silently misread.
const CurrentConfigVersion = 1

// ProviderConfig records a provider and the SecretStore name under which its key
// is stored — not the key.
type ProviderConfig struct {
	Name   string `json:"name"`    // anthropic | openai | openrouter | codex
	KeyRef string `json:"key_ref"` // secret name in the SecretStore
}

// ChannelConfig records the chat channel and its token secret-references.
type ChannelConfig struct {
	Type      string   `json:"type"`            // telegram | slack | none
	TokenRefs []string `json:"token_refs"`      // secret names (never tokens)
	Allow     []string `json:"allow,omitempty"` // principal ids permitted to drive serve (merged with NILCORE_ALLOWLIST)
}

// Config is the on-disk NilCore configuration. It holds references to secrets,
// never secrets, so it is safe to read, diff, and commit-by-mistake.
type Config struct {
	Version   int              `json:"version"`           // schema version (CurrentConfigVersion)
	Providers []ProviderConfig `json:"providers"`         //
	Executor  string           `json:"executor"`          // native model spec: provider:model
	Advisor   string           `json:"advisor"`           // advisor model spec: provider:model
	Backend   string           `json:"backend,omitempty"` // native | codex | claude-code
	Runtime   string           `json:"runtime"`           // podman | docker
	Image     string           `json:"image"`             //
	Channel   ChannelConfig    `json:"channel"`           //
	Web       WebConfig        `json:"web,omitempty"`     // sandboxed web access (egress allowlist + search)
	Delegated []string         `json:"delegated"`         // detected CLIs (informational): codex, claude
	Codex     DelegatedConfig  `json:"codex,omitempty"`   // optional config for the Codex delegated CLI (R1)
	Claude    DelegatedConfig  `json:"claude,omitempty"`  // optional config for the Claude Code delegated CLI (R1)
}

// DelegatedConfig configures a delegated coding CLI (Codex / Claude Code). All
// fields are optional; empty ⇒ the CLI's own defaults, so the delegated command is
// byte-identical to before. Model/Effort map to the CLI's model + reasoning-effort
// knobs; ExtraArgs are raw extra CLI tokens (e.g. "-c", "key=value"); Env is extra
// per-run environment merged with the API key (e.g. CODEX_HOME / CLAUDE_CONFIG_DIR
// to surface a config dir despite the sandbox's HOME=/tmp). Env values are injected
// per run and never logged or given to the model (I3). Env-var overrides at runtime:
// NILCORE_CODEX_MODEL / NILCORE_CODEX_EFFORT and NILCORE_CLAUDE_MODEL /
// NILCORE_CLAUDE_EFFORT take precedence over the config file.
type DelegatedConfig struct {
	Model     string            `json:"model,omitempty"`
	Effort    string            `json:"effort,omitempty"`
	ExtraArgs []string          `json:"extra_args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// WebConfig records the agent's web-access setup (so it survives a restart and is
// not a flag the operator must remember). It references secrets, never secrets.
// Empty/zero ⇒ web access off (default-deny), the prior behavior.
type WebConfig struct {
	Enabled      bool     `json:"enabled,omitempty"`        // master switch for sandboxed web access
	Allow        []string `json:"allow,omitempty"`          // egress host allowlist (the search host is auto-added)
	Search       string   `json:"search,omitempty"`         // "" (auto) | off | ddg (keyless) | brave (keyed)
	SearchKeyRef string   `json:"search_key_ref,omitempty"` // SecretStore ref for the brave key (never the key)
}

// Recognized values. Kept as closed sets so Validate and the wizard share one
// vocabulary and a typo fails loudly instead of surfacing as a runtime error.
var (
	validRuntimes  = map[string]bool{"podman": true, "docker": true}
	validBackends  = map[string]bool{"native": true, "codex": true, "claude-code": true}
	validProviders = map[string]bool{"anthropic": true, "openai": true, "openrouter": true, "codex": true}
	validChannels  = map[string]bool{"": true, "none": true, "telegram": true, "slack": true}
	validSearch    = map[string]bool{"": true, "off": true, "ddg": true, "brave": true}
)

// Validate reports whether c is internally consistent enough for boot to trust
// it. It returns a specific, actionable error for the first problem and never
// mutates c. Empty optional fields are allowed (boot fills runtime/backend
// defaults and keys may come from the environment) — only a *present, wrong*
// value is rejected, so an env-only config.json (no providers) is still valid.
func (c Config) Validate() error {
	if c.Version != CurrentConfigVersion {
		return fmt.Errorf("version %d is not supported (this build uses %d)", c.Version, CurrentConfigVersion)
	}
	if c.Runtime != "" && !validRuntimes[c.Runtime] {
		return fmt.Errorf("unknown runtime %q; valid values are %s", c.Runtime, oneOf(validRuntimes))
	}
	if c.Backend != "" && !validBackends[c.Backend] {
		return fmt.Errorf("unknown backend %q; valid values are %s", c.Backend, oneOf(validBackends))
	}
	if !validChannels[c.Channel.Type] {
		return fmt.Errorf("unknown channel %q; valid values are none, telegram, slack", c.Channel.Type)
	}
	if !validSearch[c.Web.Search] {
		return fmt.Errorf("unknown web.search %q; valid values are off, ddg, brave", c.Web.Search)
	}
	for _, p := range c.Providers {
		if !validProviders[p.Name] {
			return fmt.Errorf("unknown provider %q; valid values are %s", p.Name, oneOf(validProviders))
		}
		if strings.TrimSpace(p.KeyRef) == "" {
			return fmt.Errorf("provider %q has no key_ref", p.Name)
		}
	}
	return nil
}

// Save writes the config as JSON (0600) to path, creating parent dirs. It stamps
// the current schema version so a file written today is recognizably current.
func (c Config) Save(path string) error {
	if c.Version == 0 {
		c.Version = CurrentConfigVersion
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Load reads a config from path, decoding strictly (unknown fields are an error,
// to catch typos), migrating an older version forward, and validating the result.
// A read error (including a missing file) is returned verbatim so callers can
// distinguish "no config" from "bad config".
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// parse decodes-migrates-validates a config's raw bytes. It is the single place
// the "configuration is data, brought current then checked" discipline lives.
func parse(b []byte) (Config, error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return Config{}, fmt.Errorf("parse: %w", err)
	}
	v := probe.Version
	if v == 0 {
		v = 1 // pre-versioning configs are version 1 by definition.
	}
	if v > CurrentConfigVersion {
		return Config{}, fmt.Errorf("version %d is newer than this build supports (max %d); upgrade NilCore",
			v, CurrentConfigVersion)
	}
	// No field migrations exist yet (CurrentConfigVersion == 1). When the schema
	// bumps, rewrite b one version at a time here before the strict decode.

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode: %w", err)
	}
	c.Version = CurrentConfigVersion
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// oneOf renders a set's keys as a stable, comma-separated list for error
// messages, so the same misconfiguration always produces the same text.
func oneOf(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
