// Package software is the software-research verifier pack (Phase 11, Pillar 2). It
// registers a small, typed catalog of checks that assert facts about published
// software artifacts — a package version on npm/PyPI/crates.io exists, a GitHub
// release or tag exists, a repository's license matches — by curling the relevant
// public registry/API INSIDE the worker's sandbox (I4) and parsing the response
// host-side as trusted Go (encoding/json, no SDK — I6).
//
// Every check reduces to exactly ONE box.Exec (a curl), so the role's egress
// allowlist governs every reach: a nil box or a denied/unreachable host fails
// closed to StatusUnverifiable, never a fabricated Pass. The fetched body is DATA
// the verifier asserts over (I7) — it is never guard.Wrap'd before parsing, because
// this is host-side verifier code (trusted Go), not a model instruction channel;
// only a bounded, harness-authored detail tail leaves the pack.
//
// This package is a LEAF: it imports only artifact, evverify, sandbox, worktreefs
// (transitively), and the standard library — never the orchestrator.
package software

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// ids registered by this pack. Kept in one place so RegisterAll and the tests agree.
const (
	idNPM     = "software.npm_version_exists"
	idPyPI    = "software.pypi_version_exists"
	idCrate   = "software.crate_version_exists"
	idRelease = "software.github_release_exists"
	idTag     = "software.github_tag_exists"
	idLicense = "software.license_matches"
)

// RegisterAll registers exactly this pack's six verifier-ids into r. It is called
// once at wiring time (via packs.Select) before any verification runs.
func RegisterAll(r *evverify.Registry) {
	r.Register(idNPM, checkNPMVersion)
	r.Register(idPyPI, checkPyPIVersion)
	r.Register(idCrate, checkCrateVersion)
	r.Register(idRelease, checkGitHubRelease)
	r.Register(idTag, checkGitHubTag)
	r.Register(idLicense, checkLicenseMatches)
}

// maxDetail bounds the harness-authored detail tail (mirrors evverify's bound) so a
// note can never flood the artifact JSON or an event Detail.
const maxDetail = 512

// detail trims a harness-authored note to the bounded tail. It carries verifier
// commentary only — never the raw remote body, never a model-authored field.
func detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// fetchJSON runs ONE curl inside the box against url, returning the HTTP status code
// and the raw body. It fails closed: a nil box, an invalid URL, or a sandbox-level
// error returns ok=false with a bounded reason, so every caller maps that to
// StatusUnverifiable without a host-side fallback (I4). The status line is appended
// by curl's -w and split off host-side.
func fetchJSON(ctx context.Context, box sandbox.Sandbox, rawURL string, extraHeaders ...string) (code int, body string, reason string, ok bool) {
	if box == nil {
		return 0, "", "no sandbox available (refusing host-side request)", false
	}
	safeURL, err := validateURL(rawURL)
	if err != nil {
		return 0, "", detail(err.Error()), false
	}
	// -sS: silent but show errors; NO -f, because we WANT the body and status of a 4xx
	// (a 404 is a decisive Fail, not an unreachable host). -w appends the status code
	// on its own final line. A registry/API requires a User-Agent (npm/GitHub reject a
	// bare curl), so we send a fixed, non-identifying one. Hard time/redirect caps keep
	// the probe tight. The box enforces egress; this command never widens it.
	var sb strings.Builder
	sb.WriteString("curl -sSL --max-time 30 --max-redirs 5 -H 'User-Agent: nilcore-verifier'")
	for _, h := range extraHeaders {
		// Headers are pack-authored constants (never model input) — still single-quoted
		// for uniformity; validateURL already rejected a quote in the URL.
		sb.WriteString(" -H '")
		sb.WriteString(h)
		sb.WriteString("'")
	}
	sb.WriteString(" -w '\\n%{http_code}' '")
	sb.WriteString(safeURL)
	sb.WriteString("'")
	res, err := box.Exec(ctx, sb.String())
	if err != nil {
		return 0, "", detail("sandbox: " + err.Error()), false
	}
	if res.ExitCode != 0 {
		// curl exited non-zero without -f only on a transport failure (DNS, connect,
		// timeout, denied host): no HTTP response at all. Not decisive => caller maps
		// to Unverifiable.
		d := strings.TrimSpace(res.Stderr)
		if d == "" {
			d = fmt.Sprintf("curl exited %d", res.ExitCode)
		}
		return 0, "", detail(d), false
	}
	out := res.Stdout
	nl := strings.LastIndexByte(out, '\n')
	if nl < 0 {
		return 0, "", "malformed curl response (no status line)", false
	}
	code, perr := strconv.Atoi(strings.TrimSpace(out[nl+1:]))
	if perr != nil {
		return 0, "", "malformed curl status code", false
	}
	return code, out[:nl], "", true
}

// is2xx reports whether code is an HTTP success.
func is2xx(code int) bool { return code >= 200 && code < 300 }

// validateURL constrains a pack-built URL before it enters a sandbox command: only
// http/https, a host present, and no single quote/whitespace/control byte so the URL
// cannot break out of the single-quoting and smuggle a second command. Kept local to
// the leaf (mirrors evverify.validateURL) so the pack imports no orchestrator.
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

// normalize lowercases and collapses inner whitespace so a license/tag comparison is
// not defeated by trivial spacing differences. (Used for license matching.)
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
