// Package audit is the evidence-backed-findings verifier pack (Phase 12, SW-T02). An
// audit artifact's every finding cites a <relpath>:<line> locator that must REPRODUCE
// against the LOCAL worktree — there is no network leg at all. Each check reduces to
// exactly ONE box.Exec of a fixed, pack-authored verb (sed/grep) reading a file on
// disk inside the worker's sandbox (I4), followed by a trusted host-side assertion.
// Because the only input a model controls is the locator and the asserted pattern,
// the pack is fully hermetic and deterministic: a verdict is a pure function of the
// files in the worktree, never of a remote service (§13 — no standing authority).
//
// WHY local-only, and why that is the whole point: an "audit finding" is only worth
// shipping if a reviewer can re-run it. By forcing every finding to a file:line that
// the verifier itself re-reads in-box, a fabricated or stale finding fails closed —
// the cited line either is there and contains the asserted text, or it is not.
//
// Invariant compliance:
//   - I2 (verifier is sole authority, fail-closed): a missing file, an empty line, a
//     locator that does not parse, a path that tries to escape the worktree, or a
//     sandbox-level error is StatusUnverifiable or StatusFail — never a fabricated
//     StatusPass. An unregistered id elsewhere stays Unverifiable.
//   - I4 (model-emitted execution in the box): the only commands are sed/grep, which
//     are PACK CONSTANTS (never model-emitted), run via box.Exec; the model-authored
//     relpath/pattern are single-quoted so they stay DATA and cannot start a second
//     command. A path-escape is rejected BEFORE any box call (no reach at all).
//   - I6 (stdlib only): fmt/strconv/strings + the in-box coreutils; go.mod untouched.
//   - I7 (untrusted model fields are data): the cited line is read and parsed
//     host-side and is NEVER echoed into Detail — only a bounded, harness-authored
//     note describing the verdict leaves the pack. The model-authored Value/SourceURL
//     are matched against, never reflected back unfenced.
//
// This package is a LEAF: it imports only artifact, evverify, sandbox, and the
// standard library — never the orchestrator (super/agent/project/roster/swarm), never
// the schema package (the per-Kind shape is aggregated by the assembler, SW-T05).
package audit

import (
	"fmt"
	"strconv"
	"strings"

	"nilcore/internal/artifact/evverify"
)

// Verifier-ids registered by this pack, namespaced under "audit." so a claim names
// e.g. Evidence.Verifier = "audit.file_line_exists". Kept in one place so RegisterAll
// and the tests agree on the exact id set.
const (
	// IDFileLineExists asserts the cited <relpath>:<line> resolves to a non-empty line
	// in the worktree (the finding points at a real location).
	IDFileLineExists = "audit.file_line_exists"
	// IDPatternMatches asserts the cited line CONTAINS the model-authored Evidence.Value
	// (whitespace-normalized) — the finding's quoted text is actually on that line.
	IDPatternMatches = "audit.pattern_matches"
	// IDFindingReproduces asserts a grep of the asserted pattern over the cited file
	// yields the claimed number of matches AT the cited line (within reproWindow lines) —
	// the finding reproduces at the citation, not merely somewhere in the file.
	IDFindingReproduces = "audit.finding_reproduces"
)

// Pack-authored command verbs. These are CONSTANTS, never model input: the model only
// ever supplies the (single-quoted, validated) path/line/pattern that follows them, so
// it can never substitute its own program (I4).
const (
	verbSed = "sed -n"
	// grep -n -F: fixed-string, line-NUMBERED. We keep -n (and drop the old -c count-only
	// mode) because the reproduce check must know WHICH lines match — a match somewhere in
	// the file is not enough; it must fall at (or within reproWindow of) the CITED line.
	verbGrep = "grep -n -F"
)

// reproWindow is the half-width (in lines) of the band around the cited line within which
// a grep match counts as "reproducing AT the citation". A finding cites a single line, but
// trivial edits (a wrapped argument, an inserted comment) can nudge the exact match by a
// line or two, so we accept a tiny window rather than demanding the byte-exact line — while
// still rejecting a match that is hundreds of lines away (which is what the old whole-file
// grep silently accepted).
const reproWindow = 2

// RegisterAll registers exactly this pack's three verifier-ids into r. It is called
// once at wiring time (via packs.Select) before any verification runs. An id not
// registered here resolves to Unverifiable elsewhere — registration is the single seam
// that turns these checks on.
func RegisterAll(r *evverify.Registry) {
	r.Register(IDFileLineExists, checkFileLineExists)
	r.Register(IDPatternMatches, checkPatternMatches)
	r.Register(IDFindingReproduces, checkFindingReproduces)
}

// Hosts is the documented egress host-set this pack reaches. The audit pack reads only
// files on the local worktree disk — it never makes a network request — so the answer
// is nil. Exposed so the packs aggregator's HostsFor("audit") (SW-T05) and the
// no-egress cross-check have a definite, empty answer to assert against.
func Hosts() []string { return nil }

// maxDetail bounds the harness-authored detail tail (mirrors evverify's bound) so a
// verifier note can never flood the artifact JSON or an event Detail.
const maxDetail = 512

// detail trims a harness-authored note to the bounded tail. It carries verifier
// commentary ONLY — never the raw file line and never a model-authored field echoed
// unfenced (I7).
func detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// fileLine is a parsed, worktree-safe locator: a relative path plus a 1-based line
// number, both already validated. It is produced only by validateFileLine, so a value
// of this type is always safe to single-quote into a sed/grep command.
type fileLine struct {
	path string // relative, no "..", no leading "/", no quote/whitespace/control byte
	line int    // positive (1-based)
}

// validateFileLine parses a "<relpath>:<N>" locator carried in Evidence.SourceURL and
// constrains it to a single, worktree-confined relative path plus a positive line
// number. It is the security boundary for both checks: a path-escape (a "..", a leading
// "/") or a path carrying a single quote / whitespace / control byte is rejected HERE,
// BEFORE any box.Exec, so a malicious locator never reaches the sandbox at all (the
// caller maps the error to Unverifiable with NO box call). Mirrors the defense-in-depth
// of software.validateName: even though the path is single-quoted downstream, a
// surprising path can never form a command.
//
// The locator is split on the LAST ':' so a path may itself contain a ':' is NOT
// supported (a colon in a path would be ambiguous with the line separator); such a
// locator is rejected. The path is checked rune-by-rune to forbid the quote that would
// break single-quoting and any whitespace/control byte.
func validateFileLine(raw string) (fileLine, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fileLine{}, fmt.Errorf("locator is required (want <relpath>:<line>)")
	}
	idx := strings.LastIndexByte(raw, ':')
	if idx < 0 {
		return fileLine{}, fmt.Errorf("locator %q has no ':<line>' suffix", raw)
	}
	path := raw[:idx]
	lineStr := raw[idx+1:]

	line, err := strconv.Atoi(strings.TrimSpace(lineStr))
	if err != nil {
		return fileLine{}, fmt.Errorf("locator line %q is not an integer", lineStr)
	}
	if line < 1 {
		return fileLine{}, fmt.Errorf("locator line must be positive (got %d)", line)
	}

	if err := validateRelPath(path); err != nil {
		return fileLine{}, err
	}
	return fileLine{path: path, line: line}, nil
}

// validateRelPath enforces that path is a single, worktree-confined relative path. It
// rejects an empty path, an absolute path (leading "/"), any ".." segment (the classic
// traversal), and any single quote / whitespace / control byte (which would break the
// single-quoting that keeps the path DATA in the shell command). It is deliberately
// strict: the verifier reads only inside the worktree, so anything that could point
// elsewhere or smuggle a second command is refused.
func validateRelPath(path string) error {
	if path == "" {
		return fmt.Errorf("locator path is empty")
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("locator path %q must be relative (no leading '/')", path)
	}
	for _, r := range path {
		if r == '\'' {
			return fmt.Errorf("locator path may not contain a single quote")
		}
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("locator path may not contain whitespace or control characters")
		}
	}
	// Reject any ".." segment regardless of separator position: "..", "../x", "a/..",
	// "a/../b" all escape. Splitting on "/" and matching the exact segment avoids
	// rejecting a legitimate file like "a..b".
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return fmt.Errorf("locator path %q may not contain a '..' segment", path)
		}
	}
	return nil
}

// quote single-quotes a validated string for safe interpolation into a /bin/sh command.
// validateRelPath has already rejected any single quote, so wrapping in single quotes is
// sufficient: nothing inside can terminate the quoting or expand. Kept tiny and local so
// the leaf imports no shell-quoting helper from the orchestrator.
func quote(s string) string { return "'" + s + "'" }

// normalize collapses runs of whitespace to a single space and trims, so a substring or
// match assertion is not defeated by incidental indentation/spacing differences between
// the model-authored Value and the on-disk line. (It does NOT lowercase — source code is
// case-sensitive, and an audit finding's quoted token must match case.)
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ensure the package compiles against the stable CheckFunc signature.
var (
	_ evverify.CheckFunc = checkFileLineExists
	_ evverify.CheckFunc = checkPatternMatches
	_ evverify.CheckFunc = checkFindingReproduces
)
