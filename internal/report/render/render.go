// Package render holds the three PURE renderers for the verification report
// (Phase 11, Pillar 6, P11-T32): RenderText (TTY-styled, plain on a pipe/CI),
// RenderHTML (self-contained, script-free, inline CSS), and RenderMarkdown
// (stdlib-hand-rolled markup). Each consumes a *report.ReportModel and returns a
// string — no stdout, no I/O, no globals — so they are unit-testable directly.
//
// It is a LEAF over the projection: it imports only `report` (the model),
// `termui` (the Style type, for the one TTY-colour case), and stdlib. It NEVER
// imports eventlog/artifact directly or the orchestrator, so the trust story
// stays a pure read.
//
// Trust + invariants. The renderers are the display boundary for UNTRUSTED,
// model-authored data (I7): a claim's Value/SourceURL and a verifier's
// Output/Detail are escaped (html.EscapeString in HTML) and passed through a
// secret redactor (I3) before they reach any output, so a <script> payload
// cannot execute and a leaked key shape cannot be printed. GREEN is a verifier
// projection, never a citations claim (I2 / NON-GOAL guard): no renderer prints a
// GREEN final-pass headline unless ChainVerified AND every status is "pass" — a
// broken chain forces a RED "report not trustworthy" banner in all three formats.
package render

import (
	"regexp"
	"strings"

	"nilcore/internal/report"
)

// statusPass is the one Status value (artifact.StatusPass) that counts as green.
// We compare on the string rather than import internal/artifact — the import
// boundary for this package is report + termui + stdlib only (go list -deps).
const statusPass = "pass"

// brokenChainBanner is the single RED warning every format shows when the event
// chain failed eventlog.Verify: the evidence may have been tampered with, so the
// report is explicitly NOT trustworthy and no GREEN headline is shown (I5/I2).
const brokenChainBanner = "CHAIN BROKEN — report not trustworthy"

// greenHeadline / redHeadline are the final-pass verdict lines. The green one is
// emitted ONLY on a verified chain with every status pass (the NON-GOAL guard: a
// projection of harness-computed green, never a self-claimed summary).
const (
	greenHeadline = "GREEN — every check passed"
	redHeadline   = "RED — verification did not pass"
)

// allStatusesPass reports whether every folded claim across every artifact is
// StatusPass. It is the second half of the GREEN-headline gate (the first is
// ChainVerified): an artifact with no claims, or any non-pass claim, is not green.
// An entirely artifact-free model defers to FinalPass (check-level) for its
// headline, so this only GATES the green text, never fabricates it.
func allStatusesPass(m *report.ReportModel) bool {
	for i := range m.Artifacts {
		a := m.Artifacts[i]
		if len(a.Claims) == 0 {
			return false
		}
		for j := range a.Claims {
			if string(a.Claims[j].Status) != statusPass {
				return false
			}
		}
	}
	return true
}

// showGreen is the shared headline gate used by all three renderers. GREEN shows
// ONLY when the chain verified, the run's checks finally passed, AND every claim
// status is pass — so the green text is provably a verifier projection.
func showGreen(m *report.ReportModel) bool {
	return m.ChainVerified && m.FinalPass && allStatusesPass(m)
}

// --- secret redaction (I3) ---
//
// This package cannot import internal/eventlog (its redactor is unexported and
// the import boundary forbids it). Per the T32 spec ("the eventlog redact path
// OR a shared redact") we mirror the same anchored secret shapes here so a key
// that leaked into a SourceURL or a verifier Detail tail is masked in all three
// formats before render. The pattern set is kept in sync with
// internal/eventlog/redact.go's secretRe.
var secretRe = regexp.MustCompile(strings.Join([]string{
	`(?:sk|xoxb|xoxp|xoxa|xoxr|xoxs|xapp|ghp|gho|ghu|ghs|glpat)[-_][A-Za-z0-9_\-]{12,}`, // prefixed provider/CI tokens
	`AKIA[0-9A-Z]{16}`,                  // AWS access key id
	`ASIA[0-9A-Z]{16}`,                  // AWS temporary access key id
	`github_pat_[A-Za-z0-9_]{20,}`,      // GitHub fine-grained PAT
	`AIza[0-9A-Za-z\-_]{35}`,            // Google API key
	`-----BEGIN[ A-Z]*PRIVATE KEY-----`, // PEM private-key header
	`api_key=[A-Za-z0-9_\-]{8,}`,        // an api_key query param (keyed-source leak shape)
	`token=[A-Za-z0-9_\-]{8,}`,          // a token query param
}, "|"))

// redact masks secret-looking substrings in any model-authored or verifier string
// before it is rendered. It is applied to every untrusted field (Value, SourceURL,
// check Output, claim Detail) by all three renderers.
func redact(s string) string {
	if s == "" {
		return s
	}
	return secretRe.ReplaceAllString(s, "[redacted]")
}

// dash returns a placeholder for an empty cell so a table column never collapses.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
