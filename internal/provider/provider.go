// Package provider holds the model-vendor adapters behind the model.Provider
// seam: anthropic (Messages API), openai (Chat Completions), and openrouter
// (OpenAI-compatible). Model selection is role → "provider:model", so the native
// loop never edits when a provider is added. Stdlib only (invariant I6).
package provider

import (
	"fmt"
	"os"
	"strings"

	"nilcore/internal/model"
)

// Resolve builds a provider from a "provider:model" spec (a bare model defaults
// to anthropic), reading the vendor API key from the process environment. The key
// is passed only into the adapter's request header — never logged (invariant I3).
func Resolve(spec string) (model.Provider, error) {
	return ResolveWith(spec, os.Getenv)
}

// ResolveWith is Resolve with an injectable key lookup: getenv maps a vendor's
// API-key variable name (e.g. "OPENROUTER_API_KEY") to its value. The composition
// root passes a lookup that prefers the process environment and falls back to the
// configured SecretStore, so this leaf adapter never imports secrets or onboard.
func ResolveWith(spec string, getenv func(string) string) (model.Provider, error) {
	vendor, modelID := split(spec)
	switch vendor {
	case "anthropic":
		return withKey(getenv, "ANTHROPIC_API_KEY", vendor, func(k string) model.Provider { return NewAnthropic(k, modelID) })
	case "openai":
		return withKey(getenv, "OPENAI_API_KEY", vendor, func(k string) model.Provider { return NewOpenAI(k, modelID) })
	case "openrouter":
		return withKey(getenv, "OPENROUTER_API_KEY", vendor, func(k string) model.Provider { return NewOpenRouter(k, modelID) })
	default:
		return nil, fmt.Errorf("unknown provider %q (want anthropic | openai | openrouter)", vendor)
	}
}

func withKey(getenv func(string) string, env, vendor string, build func(string) model.Provider) (model.Provider, error) {
	key := getenv(env)
	if key == "" {
		return nil, fmt.Errorf("%s is required for provider %q", env, vendor)
	}
	return build(key), nil
}

// split separates "provider:model"; a spec with no colon is a bare Anthropic
// model, except a bare "openrouter" which selects that provider's default model
// (Fusion). Only the first colon splits, so OpenRouter "provider/model" ids
// survive, and "openrouter:" (empty model) also takes the default.
func split(spec string) (vendor, modelID string) {
	if i := strings.Index(spec, ":"); i >= 0 {
		return spec[:i], spec[i+1:]
	}
	if spec == "openrouter" {
		return "openrouter", "" // NewOpenRouter applies DefaultOpenRouterModel
	}
	return "anthropic", spec
}
