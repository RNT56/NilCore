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
	case "openai-compatible", "compat":
		return resolveCompat(getenv, vendor, modelID)
	default:
		return nil, fmt.Errorf("unknown provider %q (want anthropic | openai | openrouter | openai-compatible)", vendor)
	}
}

// resolveCompat builds a generic OpenAI-compatible adapter for an operator-typed,
// self-hosted endpoint. Every knob is read through the injected getenv seam — the
// base URL, the auth scheme, and the NAME of the env var that holds the key — so
// this leaf never imports secrets and the key only ever rides a request header
// (invariant I3). The key value is never logged and never appears in any error.
//
// NILCORE_COMPAT_BASE_URL    — REQUIRED full endpoint prefix (carries any "/v1").
// NILCORE_COMPAT_AUTH_SCHEME — "bearer" (default) | "azure" | "none".
// NILCORE_COMPAT_KEY_ENV     — NAME of the env var holding the key
//
//	(default "NILCORE_COMPAT_API_KEY").
func resolveCompat(getenv func(string) string, vendor, modelID string) (model.Provider, error) {
	baseURL := getenv("NILCORE_COMPAT_BASE_URL")
	if baseURL == "" {
		return nil, fmt.Errorf("NILCORE_COMPAT_BASE_URL is required for provider %q", vendor)
	}

	scheme := getenv("NILCORE_COMPAT_AUTH_SCHEME")
	var authHeader, authPrefix string
	switch scheme {
	case "", "bearer":
		authHeader, authPrefix = "authorization", "Bearer "
	case "azure":
		authHeader, authPrefix = "api-key", ""
	case "none":
		authHeader, authPrefix = "", ""
	default:
		return nil, fmt.Errorf("NILCORE_COMPAT_AUTH_SCHEME %q is not supported for provider %q (want bearer | azure | none)", scheme, vendor)
	}

	keyEnv := getenv("NILCORE_COMPAT_KEY_ENV")
	if keyEnv == "" {
		keyEnv = "NILCORE_COMPAT_API_KEY"
	}
	// Anti-exfiltration (invariant I3): a real first-party vendor key must never be
	// shipped to an operator-typed self-hosted base URL. Reject by NAME — the error
	// carries the rejected variable name only, never its value.
	if isCanonicalVendorKeyEnv(keyEnv) {
		return nil, fmt.Errorf("NILCORE_COMPAT_KEY_ENV %q is a first-party vendor key and may not be forwarded to a self-hosted endpoint (provider %q)", keyEnv, vendor)
	}
	key := getenv(keyEnv)

	// "none" targets keyless local servers (vLLM/Ollama/LM-Studio): the adapter
	// emits no auth header, so an empty key is allowed. For bearer/azure, require a
	// key with the same key-free error shape the other vendors use via withKey.
	if scheme != "none" && key == "" {
		return nil, fmt.Errorf("%s is required for provider %q", keyEnv, vendor)
	}

	return NewOpenAICompatible(modelID,
		WithBaseURL(baseURL),
		WithAuth(authHeader, authPrefix),
		WithKey(key),
	), nil
}

// isCanonicalVendorKeyEnv reports whether name is a first-party vendor's canonical
// API-key variable. Such keys must never be forwarded to a self-hosted base URL.
func isCanonicalVendorKeyEnv(name string) bool {
	switch name {
	case "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY":
		return true
	default:
		return false
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
