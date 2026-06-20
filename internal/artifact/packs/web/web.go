// Package web is the web-research verifier pack (Phase 11, Pillar 2). It registers
// four namespaced verifier-ids into an evverify.Registry, each one reducing to a
// SINGLE curl run inside the worker's sandbox (I4) whose response is parsed
// host-side as TRUSTED Go — never re-fed to the model, never guard.Wrapped before
// the parse, because the parsing code here IS the harness, not untrusted input:
//
//   - web.url_resolves — the source returns an HTTP 2xx.
//   - web.quote_exists — the asserted Value appears verbatim (whitespace-normalized)
//     in the fetched page body.
//   - web.date_matches — the asserted Value (a date string) appears in the body.
//   - web.not_stale    — the source is fresh, judged from a SERVER-sent timestamp
//     re-fetched in-box (Last-Modified / Date header), never from the
//     model-authored Evidence.RetrievedAt (which is only a hint, I2).
//
// Trust + sandbox discipline (mirrors evverify.checkURLResolves):
//   - Every external reach is box.Exec. A nil Box, an invalid/unsafe SourceURL, an
//     unreachable/denied host, or an unparseable response all fail closed to
//     StatusUnverifiable — never a fabricated StatusPass and never a host-side
//     request that would bypass the egress allowlist.
//   - The model-authored SourceURL is validated (http/https only, no quote /
//     whitespace / control byte) and single-quoted, so it cannot break out of the
//     command into a second one.
//
// This package is a LEAF: it imports only artifact, evverify, sandbox, worktreefs,
// and the standard library — never the orchestrator (super/roster/agent) and never
// internal/tools or internal/model.
package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// Verifier-id catalog this pack owns. Kept as constants so the wiring and tests
// reference the same strings the registry binds.
const (
	IDURLResolves = "web.url_resolves"
	IDQuoteExists = "web.quote_exists"
	IDDateMatches = "web.date_matches"
	IDNotStale    = "web.not_stale"
)

// maxBody caps the bytes curl transfers for a body fetch, so a huge page can never
// flood the parse. The header probe is capped by curl's own --head (headers only).
const maxBody = 1 << 20 // 1 MiB

// defaultMaxAge is the freshness window web.not_stale applies when a claim does not
// override it. It is deliberately generous — staleness here only DEMOTES a source
// whose server timestamp is older than the window; it can never be the basis to
// PASS a claim (I2).
const defaultMaxAge = 30 * 24 * time.Hour

// RegisterAll binds this pack's four verifier-ids into r. It is the one seam the
// aggregator (P11-T11) calls; before it runs, none of the four ids resolve, so a
// web claim is Unverifiable until the pack is opted in (byte-identical default).
func RegisterAll(r *evverify.Registry) {
	r.Register(IDURLResolves, checkURLResolves)
	r.Register(IDQuoteExists, checkQuoteExists)
	r.Register(IDDateMatches, checkDateMatches)
	r.Register(IDNotStale, checkNotStale)
}

// checkURLResolves asserts the claim's SourceURL is reachable and returns HTTP 2xx,
// via one curl -f in the box. 2xx ⇒ Pass; non-2xx / unreachable / denied ⇒
// Unverifiable (nothing about the VALUE is asserted, so it is never a Fail).
func checkURLResolves(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	if box == nil {
		return artifact.StatusUnverifiable, "no sandbox available (refusing host-side request)"
	}
	safeURL, err := validateURL(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, trim(err.Error())
	}
	// -fsSL: silent, fail on an HTTP error status, follow redirects. Discard the body
	// (-o /dev/null): we only care that a 2xx was reached.
	cmd := fmt.Sprintf("curl -fsSL -o /dev/null --max-time 30 --max-redirs 5 '%s'", safeURL)
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		return artifact.StatusUnverifiable, trim("sandbox: " + err.Error())
	}
	if res.ExitCode != 0 {
		return artifact.StatusUnverifiable, trim(curlFail(res.Stderr, res.ExitCode))
	}
	return artifact.StatusPass, "HTTP 2xx"
}

// checkQuoteExists fetches the page body and asserts the model's Value appears in
// it as a whitespace-normalized substring. Present ⇒ Pass; absent ⇒ Fail (the
// quote is decisively WRONG — re-derive route). A missing Value, an unreachable
// source, or a non-2xx body ⇒ Unverifiable (we could not even look).
func checkQuoteExists(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	needle := normalizeSpace(c.Evidence.Value)
	if needle == "" {
		return artifact.StatusUnverifiable, "no value to look for"
	}
	body, st, d := fetchBody(ctx, box, c.Evidence.SourceURL)
	if st != artifact.StatusPass {
		return st, d
	}
	if strings.Contains(normalizeSpace(body), needle) {
		return artifact.StatusPass, "quote present in source body"
	}
	return artifact.StatusFail, "quote not found in source body"
}

// checkDateMatches fetches the body and asserts the model's Value (a date string)
// appears in it. It normalizes whitespace only — date FORMAT canonicalization is
// out of scope for a leaf pack; the asserted Value must appear as the source writes
// it. Present ⇒ Pass; absent ⇒ Fail; unreachable / non-2xx ⇒ Unverifiable.
func checkDateMatches(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	needle := normalizeSpace(c.Evidence.Value)
	if needle == "" {
		return artifact.StatusUnverifiable, "no date value to match"
	}
	body, st, d := fetchBody(ctx, box, c.Evidence.SourceURL)
	if st != artifact.StatusPass {
		return st, d
	}
	if strings.Contains(normalizeSpace(body), needle) {
		return artifact.StatusPass, "date present in source body"
	}
	return artifact.StatusFail, "date not found in source body"
}

// checkNotStale judges freshness from a SERVER-sent timestamp, re-fetched in-box
// via a HEAD request — NEVER from the model-authored Evidence.RetrievedAt, which is
// only a hint and could be a fabricated "now" over a stale source (I2). It reads
// Last-Modified (preferred) or Date from the response headers; a source whose
// server timestamp is within the freshness window ⇒ Pass, older ⇒ Stale (re-fetch
// route). No usable header, unreachable, or non-2xx ⇒ Unverifiable. The freshness
// window can never PASS on RetrievedAt — staleness only ever DEMOTES.
func checkNotStale(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	if box == nil {
		return artifact.StatusUnverifiable, "no sandbox available (refusing host-side request)"
	}
	safeURL, err := validateURL(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, trim(err.Error())
	}
	// -fsSLI: a HEAD request (-I) so the server emits only headers; -f makes a non-2xx
	// a non-zero exit. We parse the header block host-side as trusted Go.
	cmd := fmt.Sprintf("curl -fsSLI --max-time 30 --max-redirs 5 '%s'", safeURL)
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		return artifact.StatusUnverifiable, trim("sandbox: " + err.Error())
	}
	if res.ExitCode != 0 {
		return artifact.StatusUnverifiable, trim(curlFail(res.Stderr, res.ExitCode))
	}

	served, ok := serverTimestamp(res.Stdout)
	if !ok {
		// The source resolved but carries no usable freshness header. We cannot assert
		// it is fresh, and we refuse to lean on the model's RetrievedAt — Unverifiable.
		return artifact.StatusUnverifiable, "no Last-Modified/Date header to judge freshness"
	}

	maxAge := defaultMaxAge
	age := time.Since(served)
	if age <= maxAge {
		return artifact.StatusPass, "source fresh (server timestamp within window)"
	}
	return artifact.StatusStale, fmt.Sprintf("source stale: server timestamp %s old", age.Round(time.Hour))
}

// fetchBody runs one curl GET in the box and returns the body on a 2xx, or a typed
// non-pass status the caller propagates. It centralizes the nil-box / invalid-URL /
// non-2xx fail-closed handling the body checks share.
func fetchBody(ctx context.Context, box sandbox.Sandbox, rawURL string) (string, artifact.Status, string) {
	if box == nil {
		return "", artifact.StatusUnverifiable, "no sandbox available (refusing host-side request)"
	}
	safeURL, err := validateURL(rawURL)
	if err != nil {
		return "", artifact.StatusUnverifiable, trim(err.Error())
	}
	cmd := fmt.Sprintf("curl -fsSL --max-filesize %d --max-time 30 --max-redirs 5 '%s'", maxBody, safeURL)
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		return "", artifact.StatusUnverifiable, trim("sandbox: " + err.Error())
	}
	if res.ExitCode != 0 {
		return "", artifact.StatusUnverifiable, trim(curlFail(res.Stderr, res.ExitCode))
	}
	return res.Stdout, artifact.StatusPass, ""
}

// serverTimestamp extracts the freshest usable timestamp from a curl -I header
// block, preferring Last-Modified over Date. Headers are parsed host-side as
// trusted Go. It returns ok=false when neither header is present or parseable —
// the caller then refuses to assert freshness (Unverifiable), rather than trusting
// the model's RetrievedAt.
func serverTimestamp(headerBlock string) (time.Time, bool) {
	var lastMod, date time.Time
	var haveLastMod, haveDate bool
	for _, line := range strings.Split(headerBlock, "\n") {
		k, v, ok := splitHeader(line)
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "last-modified":
			if t, err := parseHTTPDate(v); err == nil {
				lastMod, haveLastMod = t, true
			}
		case "date":
			if t, err := parseHTTPDate(v); err == nil {
				date, haveDate = t, true
			}
		}
	}
	if haveLastMod {
		return lastMod, true
	}
	if haveDate {
		return date, true
	}
	return time.Time{}, false
}

// splitHeader splits "Key: value" into its trimmed key and value. It returns
// ok=false for a status line or any line without a colon.
func splitHeader(line string) (string, string, bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// parseHTTPDate parses an HTTP-date header value. RFC1123 (the modern form) is
// tried first, then the two legacy forms RFC 7231 still allows.
func parseHTTPDate(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized HTTP-date %q", v)
}

// normalizeSpace collapses every run of whitespace to a single space and trims, so
// a quote/date match is robust to reflowing and incidental indentation in the page.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// curlFail builds a bounded reason from curl's stderr, defaulting to the exit code
// when stderr is empty.
func curlFail(stderr string, code int) string {
	d := strings.TrimSpace(stderr)
	if d == "" {
		return fmt.Sprintf("curl exited %d", code)
	}
	return d
}

// maxDetail bounds the harness-authored detail tail, so a verifier note can never
// flood the artifact JSON or an event Detail.
const maxDetail = 512

// trim bounds a harness-authored note. It is for verifier commentary only — never
// the raw remote body and never a model-authored field.
func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// validateURL constrains a model-authored SourceURL before it enters a sandbox
// command: http/https only, a host present, and no single quote / whitespace /
// control byte, so it cannot break out of the single-quoting and smuggle a second
// command. (Kept local to the leaf so the pack imports no orchestrator package —
// the same defense-in-depth as evverify.validateURL and tools.validateFetchURL.)
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
