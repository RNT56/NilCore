// config.go is the *declarative* half of the swarm provider pool: the data the
// operator (or onboard config) hands in, and the validation that keeps key
// material and nonsense out before a single provider is constructed. It is
// deliberately a plain struct of strings + ints — every field is a
// "provider:model" spec, a duration string, or a count. There is NO key field
// anywhere in this file by design (invariant I3): keys are resolved at Build
// time, by NAME, through the injected `cred func(string) string` resolver, and
// never travel inside config that might be serialized, logged, or echoed.
//
// WHY a separate Validate that takes the valid-provider list as an argument: the
// pool is a leaf and must not import the provider registry's notion of "which
// vendors exist" (that would couple the leaf to a sibling and risk an import
// cycle once onboard depends on pool). Instead the caller — onboard or cmd —
// owns the canonical vendor list and passes it down, so the leaf validates
// against the caller's source of truth without importing it. Fail-closed: an
// unknown vendor or a negative cap is a loud error, never a silent default.
package pool

import (
	"fmt"
	"strings"
	"time"
)

// TierSpec describes one model tier (planner, verifier, or worker) as pure
// composition inputs. Every string is a "provider:model" spec or a backend
// selector — NEVER a key (invariant I3). The zero value is a valid "unset" tier:
// an empty Spec means "fall back to the pool's default cheap worker", so a
// zero-value PoolConfig reproduces today's single cheap-worker wiring exactly.
type TierSpec struct {
	// Spec is the primary "provider:model" for this tier (e.g.
	// "anthropic:claude-opus-4-8"). Empty ⇒ the pool's built-in default.
	Spec string
	// Fallback is an optional secondary "provider:model" tried by the Resilient
	// decorator when the primary fails over. Empty ⇒ no failover target.
	Fallback string
	// Cap is the per-provider concurrency cap for this tier's primary+fallback
	// stack. 0 (or negative, rejected by Validate) ⇒ uncapped (no strongcap).
	Cap int
	// CodeBackend selects the coding-shard delegation backend: "" or "native"
	// for the in-process native loop, or "codex"/"claude-code" for a delegated
	// CLI referenced BY NAME (the pool never holds the CLI itself — that is a
	// different seam: cmd's buildBackend). It only matters on the worker tier.
	CodeBackend string
}

// PoolConfig is the whole declarative pool description. Its zero value is the
// "today" wiring: empty tiers (so every tier resolves to the default cheap
// worker), no caps, no jitter, no breaker. That zero-equivalence is load-bearing
// — it lets onboard add an optional *PoolConfig that, when absent or zero, leaves
// the single-provider path byte-identical.
type PoolConfig struct {
	// Planner and Verifier are the strong-tier specs (the coordination channel:
	// decomposition and the I2 ship verdict). Worker is the cheap-tier spec the
	// fan-out shards share.
	Planner, Verifier, Worker TierSpec
	// Caps is an OPTIONAL per-"provider:model" concurrency cap, keyed by the
	// exact spec string. It composes with TierSpec.Cap: the effective cap for a
	// spec is the max of the two (a tier opting into a tighter cap cannot be
	// loosened by Caps, and vice-versa). A negative value is rejected by Validate.
	Caps map[string]int
	// Jitter and CallTimeout are duration strings (time.ParseDuration form, e.g.
	// "750ms", "30s"). A large Jitter is intentional for a 300-agent fleet so a
	// shared 429 does not trigger a synchronized retry storm. Empty ⇒ the
	// Resilient decorator's own defaults.
	Jitter, CallTimeout string
	// Breaker is the consecutive-failure threshold after which a provider's
	// circuit breaker opens. 0 ⇒ disabled (the Resilient default).
	Breaker int
}

// Validate checks the config WITHOUT constructing anything or touching the
// network. validProviders is the caller-owned set of known vendor names (e.g.
// {"anthropic","openai","openrouter"}); an empty/nil set disables the vendor
// check (so a caller that does not want vendor validation can pass nil). It
// rejects:
//
//   - any non-empty Spec/Fallback whose vendor is not in validProviders,
//   - any negative cap (TierSpec.Cap or a Caps entry),
//   - a malformed Jitter/CallTimeout duration string.
//
// It ACCEPTS a fully zero config (the "today" wiring) and accepts a zero cap
// (uncapped). Errors are wrapped with the offending field so the message points
// the operator at the exact spec.
func (c PoolConfig) Validate(validProviders []string) error {
	valid := map[string]bool{}
	for _, p := range validProviders {
		valid[p] = true
	}
	// checkSpec validates one "provider:model" spec's vendor against the set.
	// An empty spec is the "unset"/default case and always passes. We only
	// reject a SPELLED-OUT vendor we do not recognize, so a bare model (no
	// colon, defaults to anthropic) is fine.
	checkSpec := func(field, spec string) error {
		if spec == "" {
			return nil
		}
		if len(valid) == 0 {
			return nil
		}
		vendor := vendorOf(spec)
		if !valid[vendor] {
			return fmt.Errorf("pool: %s spec %q: unknown provider %q (want one of %v)", field, spec, vendor, validProviders)
		}
		return nil
	}

	tiers := []struct {
		name string
		t    TierSpec
	}{
		{"planner", c.Planner},
		{"verifier", c.Verifier},
		{"worker", c.Worker},
	}
	for _, tier := range tiers {
		if err := checkSpec(tier.name+".spec", tier.t.Spec); err != nil {
			return err
		}
		if err := checkSpec(tier.name+".fallback", tier.t.Fallback); err != nil {
			return err
		}
		if tier.t.Cap < 0 {
			return fmt.Errorf("pool: %s cap %d is negative (want >= 0; 0 = uncapped)", tier.name, tier.t.Cap)
		}
	}
	for spec, cap := range c.Caps {
		if cap < 0 {
			return fmt.Errorf("pool: caps[%q] = %d is negative (want >= 0; 0 = uncapped)", spec, cap)
		}
		if err := checkSpec("caps key "+spec, spec); err != nil {
			return err
		}
	}
	if c.Jitter != "" {
		if _, err := time.ParseDuration(c.Jitter); err != nil {
			return fmt.Errorf("pool: jitter %q: %w", c.Jitter, err)
		}
	}
	if c.CallTimeout != "" {
		if _, err := time.ParseDuration(c.CallTimeout); err != nil {
			return fmt.Errorf("pool: call_timeout %q: %w", c.CallTimeout, err)
		}
	}
	return nil
}

// vendorOf extracts the vendor portion of a "provider:model" spec, mirroring the
// provider package's split rule (first colon splits; a bare model with no colon
// defaults to anthropic, except a bare "openrouter" which is its own vendor).
// Kept local so the leaf does not import provider's unexported split.
func vendorOf(spec string) string {
	if i := strings.Index(spec, ":"); i >= 0 {
		return spec[:i]
	}
	if spec == "openrouter" {
		return "openrouter"
	}
	return "anthropic"
}

// parseDur parses a duration string, returning 0 for the empty string (meaning
// "use the decorator default"). A non-empty malformed string is a programming
// error here because Validate already rejected it; Build still tolerates it by
// returning the parse error so a caller that skipped Validate fails loudly
// rather than silently mis-configuring the pool.
func parseDur(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", s, err)
	}
	return d, nil
}
