// Package browserwire holds the two shell-and-wire primitives that both the
// interactive browser tool (internal/tools/browser.go) and the UI verifier pack
// (internal/artifact/packs/ui) must use IDENTICALLY: the single-quote shell
// escaper that keeps model-supplied flow data DATA (never a second command —
// the I4 quoting boundary) and the JSON contract the in-sandbox nilcore-browser
// driver prints on stdout.
//
// WHY a leaf: these are the most security-load-bearing few lines on the browser
// path. If the tool and the pack each hand-rolled their own copy, a fix to the
// quoting rule could land in one and not the other. Extracting them here makes
// the boundary ONE tested copy. The package is a stdlib-only leaf (zero nilcore
// imports) so any caller can depend on it without dragging in the orchestrator.
package browserwire

import "strings"

// ShellSingleQuote wraps s in single quotes for safe use in `sh -c`, escaping
// any embedded single quote — so model-supplied actions JSON (selectors, typed
// text, URLs) cannot break out of the quoting to smuggle a second command. The
// driver consumes the result as DATA, not shell. Mirrors backend.shellQuote.
//
// The classic POSIX trick: a single-quoted string cannot contain a single
// quote, so each `'` is rewritten as `'\”` — close the quote, an escaped
// literal quote, reopen the quote. Backslashes, `$()`, newlines, and every
// other byte are inert inside single quotes and need no further treatment.
func ShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Observation is the JSON contract the in-sandbox driver prints on stdout.
// Unknown fields are ignored; it is parsed as data, never executed (I7).
type Observation struct {
	Title         string   `json:"title"`
	Text          string   `json:"text"`
	Console       []string `json:"console"`
	ScreenshotB64 string   `json:"screenshot_b64"` // delivered to the model as an image block (D1-T02)
}
