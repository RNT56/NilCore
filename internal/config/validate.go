package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Validate reports whether c is internally consistent and usable. It returns a
// specific, actionable error for the first problem it finds and never mutates c:
// validation is a verdict, not a normalizer. The path to a valid value is
// Default() plus your overrides — Default() is guaranteed to pass.
//
// Validate intentionally does not "fill defaults": silently rewriting a missing
// or wrong value hides misconfiguration. Callers that want defaults start from
// Default() and overlay, which keeps the rules in one place.
func (c Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("config: version %d is not supported; run Migrate to reach version %d",
			c.Version, CurrentVersion)
	}
	if strings.TrimSpace(c.Executor) == "" {
		return fmt.Errorf("config: executor is empty; set one of %s", oneOf(validExecutors))
	}
	if !validExecutors[c.Executor] {
		return fmt.Errorf("config: unknown executor %q; valid values are %s", c.Executor, oneOf(validExecutors))
	}
	if strings.TrimSpace(c.Runtime) == "" {
		return fmt.Errorf("config: runtime is empty; set one of %s", oneOf(validRuntimes))
	}
	if !validRuntimes[c.Runtime] {
		return fmt.Errorf("config: unknown runtime %q; valid values are %s", c.Runtime, oneOf(validRuntimes))
	}
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("config: model is empty; set a model id (e.g. " + DefaultModel + ")")
	}
	if c.MaxSteps <= 0 {
		return fmt.Errorf("config: max_steps must be positive, got %d", c.MaxSteps)
	}
	return nil
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
