// Package onboard is the `nilcore init` flow: one guided pass that captures
// providers + keys, model tiers, the container runtime, the chat channel, and
// detected delegated CLIs, then writes a JSON config holding *references* to
// secrets — never the secrets themselves (invariant I3). The keys go to the
// SecretStore (P1-T11). Works over SSH (line-based, stdlib only). P1-T12.
package onboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProviderConfig records a provider and the SecretStore name under which its key
// is stored — not the key.
type ProviderConfig struct {
	Name   string `json:"name"`    // anthropic | openai | openrouter
	KeyRef string `json:"key_ref"` // secret name in the SecretStore
}

// ChannelConfig records the chat channel and its token secret-references.
type ChannelConfig struct {
	Type      string   `json:"type"`       // telegram | slack | none
	TokenRefs []string `json:"token_refs"` // secret names (never tokens)
}

// Config is the on-disk NilCore configuration. It holds references to secrets,
// never secrets, so it is safe to read, diff, and commit-by-mistake.
type Config struct {
	Providers []ProviderConfig `json:"providers"`
	Executor  string           `json:"executor"` // role → provider:model
	Advisor   string           `json:"advisor"`  // role → provider:model
	Runtime   string           `json:"runtime"`  // podman | docker
	Image     string           `json:"image"`
	Channel   ChannelConfig    `json:"channel"`
	Delegated []string         `json:"delegated"` // detected CLIs: codex, claude
}

// Save writes the config as JSON (0600) to path, creating parent dirs.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Load reads a config from path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("config parse: %w", err)
	}
	return c, nil
}
