// Package config defines NilCore's on-disk configuration as a *versioned*
// schema, so a binary can read a file written by an older release and bring it
// forward deterministically. Configuration is data, not code: it is parsed,
// migrated to the current version, and validated before anything uses it.
//
// The lifecycle is:
//
//	raw JSON --Migrate--> current-version Config --Validate--> usable Config
//
// Default() supplies a complete, valid baseline; callers overlay their own
// fields on top of it. Validate() reports clear, specific errors (it never
// silently "fixes" a value) so a misconfiguration fails loudly at startup
// rather than surfacing as confusing behavior later. Stdlib only (invariant I6).
package config

import "encoding/json"

// CurrentVersion is the schema version this build reads and writes. Migrate
// upgrades any older config to this version; anything newer is rejected.
const CurrentVersion = 2

// Config is the full NilCore configuration. Every field carries a json tag so
// the on-disk form is stable and explicit. Unknown fields in a config file are
// rejected by Migrate (see DisallowUnknownFields) to catch typos early.
type Config struct {
	// Version is the schema version of this config. It is always written and
	// must equal CurrentVersion after a successful Migrate.
	Version int `json:"version"`

	// Executor selects the coding backend that runs a task: the in-process
	// native loop, or a delegated CLI. See validExecutors.
	Executor string `json:"executor"`

	// Runtime selects the sandbox runtime that executes model-emitted shell
	// commands. Every executable runs inside it (invariant I4). See
	// validRuntimes.
	Runtime string `json:"runtime"`

	// Model is the Anthropic model id used by the native loop. Free-form: the
	// model API rejects an unknown id, so config only checks it is non-empty.
	Model string `json:"model"`

	// MaxSteps bounds the agent loop; the loop is bounded and fully logged
	// (north star). Must be positive.
	MaxSteps int `json:"max_steps"`
}

// Default returns a complete, valid configuration. It is the canonical baseline:
// load a file by overlaying its fields onto Default(), or hand Default() to a
// fresh install. The returned value passes Validate.
func Default() Config {
	return Config{
		Version:  CurrentVersion,
		Executor: ExecutorNative,
		Runtime:  RuntimeContainer,
		Model:    DefaultModel,
		MaxSteps: DefaultMaxSteps,
	}
}

// Recognized field values. Keeping them as named constants gives callers a typed
// vocabulary and keeps the validation tables and Default() in sync.
const (
	ExecutorNative     = "native"
	ExecutorCodex      = "codex"
	ExecutorClaudeCode = "claude-code"

	RuntimeContainer = "container"
	RuntimeNone      = "none"

	DefaultModel    = "claude-sonnet-4"
	DefaultMaxSteps = 50
)

// validExecutors and validRuntimes are the closed sets Validate enforces.
var (
	validExecutors = map[string]bool{
		ExecutorNative:     true,
		ExecutorCodex:      true,
		ExecutorClaudeCode: true,
	}
	validRuntimes = map[string]bool{
		RuntimeContainer: true,
		RuntimeNone:      true,
	}
)

// MarshalJSON-free: Config marshals with the standard encoder. This helper
// keeps a single, tested round-trip used by Migrate's tests and callers that
// persist a config back to disk.
func (c Config) marshal() ([]byte, error) {
	return json.Marshal(c)
}
