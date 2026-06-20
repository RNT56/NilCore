// Package evverify binds an artifact's claims to runnable verifier checks — the
// seam that makes "GREEN because every claim passed a runnable check" true (I2).
//
// A CheckFunc is the unit of verification: given a claim and the worker's sandbox,
// it runs ONE reach into the box and returns a trusted Status plus a bounded detail
// tail. The Registry is the catalog of CheckFuncs keyed by a verifier-id (e.g.
// "web.url_resolves", "finance.sec_fact"). A claim names Evidence.Verifier; the
// ArtifactVerifier (P11-T04) resolves it through the Registry and runs it.
//
// Two trust rules are structural here, not optional:
//
//   - An UNREGISTERED id is never green. Lookup returns (nil,false) and the caller
//     maps that to StatusUnverifiable — never StatusPass. Default() therefore
//     deliberately registers NO always-pass/noop verifier: a claim can only become
//     green by an affirmative check, never by the absence of one.
//   - Every external reach is through the box (I4). A nil Box, or a box that denies
//     the host, makes a network check fail closed to StatusUnverifiable — there is
//     no host-side fallback that could bypass the sandbox or the egress allowlist.
//
// The Registry is the single seam Pillar 2 fills: a domain pack's RegisterAll(r)
// adds its namespaced ids, and from then on those claims resolve to real checks.
// This package is a LEAF — it imports only artifact, sandbox, worktreefs, verify,
// and the standard library; it never imports the orchestrator (super/roster/agent).
package evverify

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// CheckFunc runs ONE verification for a single claim, inside the supplied sandbox.
// It returns the trusted Status the verifier asserts and a bounded, harness-authored
// detail tail (never the raw remote body, never a model-authored field echoed
// unfenced). It must NOT panic and must NOT reach the network host-side: a nil box
// or an unreachable/denied host is a StatusUnverifiable result, not a Go error. A Go
// error is reserved for a genuinely broken invocation (it is surfaced by the caller
// as Unverifiable, never as a pass).
//
// The signature is STABLE: Pillar 2 packs are written against it, so changing it is
// a breaking change to every pack.
type CheckFunc func(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string)

// Registry is the catalog of CheckFuncs keyed by verifier-id. It is not safe for
// concurrent registration — registration happens once at wiring time (Default plus
// each opted-in pack's RegisterAll), before any verification runs; Lookup during a
// run is read-only over the then-frozen map.
type Registry struct {
	checks map[string]CheckFunc
}

// New returns an empty Registry with NO checks registered. It is the base every
// pack and Default build on. An empty Registry resolves every id to (nil,false),
// so without registration every claim is Unverifiable — fail-closed by default.
func New() *Registry {
	return &Registry{checks: make(map[string]CheckFunc)}
}

// Default returns a Registry preloaded with ONLY safe, generic, stdlib-reachable
// checks (currently web.url_resolves). It deliberately registers NO always-pass or
// noop verifier: a claim is green only via an affirmative check, never via a
// permissive default. Domain-specific ids (web.quote_exists, finance.*, ui.*, …)
// are added by their packs' RegisterAll, not here, so the default binary with packs
// off still resolves those ids to Unverifiable.
func Default() *Registry {
	r := New()
	r.Register("web.url_resolves", checkURLResolves)
	return r
}

// Register binds a verifier-id to a CheckFunc. A later Register for the same id
// overwrites the earlier one (last writer wins) — packs are expected to own
// disjoint namespaces, so a collision is a wiring bug the caller controls, not a
// runtime condition this package guards. A nil fn is rejected (it would otherwise
// resolve to a non-callable check); registering an empty id is rejected.
func (r *Registry) Register(id string, fn CheckFunc) {
	if id == "" || fn == nil {
		return
	}
	r.checks[id] = fn
}

// Lookup resolves a verifier-id to its CheckFunc. The second return is false for an
// unregistered id; the caller MUST treat that as StatusUnverifiable (never Pass) —
// an unbound claim has had nothing asserted about it.
func (r *Registry) Lookup(id string) (CheckFunc, bool) {
	fn, ok := r.checks[id]
	return fn, ok
}

// maxDetail bounds the harness-authored detail tail carried back per claim, so a
// verifier's note can never flood the artifact JSON or an event Detail.
const maxDetail = 512

// detail trims a harness-authored note to the bounded tail. It is for verifier
// commentary only — never the raw remote body and never a model-authored field.
func detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// checkURLResolves is the one built-in generic check: it asserts that the claim's
// SourceURL is reachable and returns an HTTP 2xx, run as a single curl INSIDE the
// box (I4) so the role's egress allowlist governs reachability. A nil box, an
// invalid/missing URL, or a non-2xx/unreachable host all fail closed to
// StatusUnverifiable — never StatusPass. It mirrors the webfetch curl discipline:
// the URL is validated (http/https only, no quote/whitespace/control byte) and
// single-quoted, so a model-authored URL cannot break out to a second command.
func checkURLResolves(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	if box == nil {
		// No sandbox to reach through. We refuse rather than fall back to a host-side
		// request, which would bypass the sandbox boundary and the egress policy (I4).
		return artifact.StatusUnverifiable, "no sandbox available (refusing host-side request)"
	}

	safeURL, err := validateURL(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}

	// -fsS: silent, but fail (non-zero exit) on an HTTP error status, so a 4xx/5xx is
	// a non-2xx => Unverifiable. -L follows redirects; hard time/redirect caps keep
	// the probe tight. The box enforces egress; this command never widens it.
	cmd := fmt.Sprintf("curl -fsSL -o /dev/null --max-time 30 --max-redirs 5 '%s'", safeURL)
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		// A sandbox-level error (the box could not run the command at all) is not a
		// decisive verdict — fail closed.
		return artifact.StatusUnverifiable, detail("sandbox: " + err.Error())
	}
	if res.ExitCode != 0 {
		// curl -f exits non-zero on a non-2xx status or an unreachable/denied host.
		// Either way no 2xx was observed: not a Pass, not a Fail (nothing about the
		// value was asserted), but Unverifiable.
		d := strings.TrimSpace(res.Stderr)
		if d == "" {
			d = fmt.Sprintf("curl exited %d", res.ExitCode)
		}
		return artifact.StatusUnverifiable, detail(d)
	}
	return artifact.StatusPass, "HTTP 2xx"
}

// validateURL constrains a model-authored SourceURL before it is placed into a
// sandbox command — the same defense-in-depth as tools.validateFetchURL: http/https
// only, a host present, and no single quote/whitespace/control byte so the URL
// cannot break out of the single-quoting and smuggle a second command. (Kept local
// to the leaf so evverify imports no orchestrator package.)
func validateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("source_url is required")
	}
	for _, r := range raw {
		if r == '\'' {
			return "", fmt.Errorf("source_url may not contain a single quote")
		}
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("source_url may not contain whitespace or control characters")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid source_url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("only http and https source_url is allowed (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("source_url has no host")
	}
	return raw, nil
}

// Resolve is the small helper the ArtifactVerifier (P11-T04) uses to run a claim's
// bound check, centralizing the unregistered-id => Unverifiable rule so it is
// expressed once. An empty Verifier id, or one absent from the Registry, yields
// StatusUnverifiable with a reason — never StatusPass.
func (r *Registry) Resolve(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	id := strings.TrimSpace(c.Evidence.Verifier)
	if id == "" {
		return artifact.StatusUnverifiable, "no verifier bound to claim"
	}
	fn, ok := r.Lookup(id)
	if !ok {
		return artifact.StatusUnverifiable, detail("unregistered verifier-id: " + id)
	}
	return fn(ctx, box, c)
}
