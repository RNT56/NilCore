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

// Ref is one interactive element in an accessibility "set-of-marks" snapshot
// (Phase 14). The model references an element by its integer ID (e.g. "click ref
// 12") rather than a CSS selector or pixel coordinate — deterministic identity,
// 20–50× cheaper than a screenshot. Role and Name are the accessibility role and
// accessible name; both are page-controlled and therefore UNTRUSTED (I7).
type Ref struct {
	ID      int    `json:"id"`
	Role    string `json:"role"`              // a11y role: button, link, textbox, … (UNTRUSTED)
	Name    string `json:"name"`              // accessible name (UNTRUSTED)
	Value   string `json:"value,omitempty"`   // current value for inputs (UNTRUSTED)
	Version uint64 `json:"version,omitempty"` // snapshot version this ref was stamped in; Session.Resolve rejects a Ref whose Version != the latest Observation's (the Cancel→Delete / same-id-after-re-render defense, host-enforced — not membership-only)
}

// Tab is one open browser target (page/tab) in a session.
type Tab struct {
	ID     string `json:"id"`
	Title  string `json:"title"` // UNTRUSTED
	URL    string `json:"url"`   // UNTRUSTED
	Active bool   `json:"active"`
}

// Observation is the JSON contract the in-sandbox driver prints on stdout (batch
// and flow modes) and returns over the session protocol (serve mode). Unknown
// fields are ignored; it is parsed as data, never executed (I7). Title/Text/
// Console/ScreenshotB64 are the original D1/R3 contract; URL/Version/Refs/Tabs are
// the additive Phase-14 accessibility-snapshot fields (omitempty, so the existing
// one-shot browser_view tool and the ui pack parse it unchanged).
type Observation struct {
	Title         string   `json:"title"`
	Text          string   `json:"text"`
	Console       []string `json:"console"`
	ScreenshotB64 string   `json:"screenshot_b64"`    // delivered to the model as an image block (D1-T02)
	URL           string   `json:"url,omitempty"`     // current page URL (UNTRUSTED)
	Version       uint64   `json:"version,omitempty"` // snapshot version for ref staleness checks
	Refs          []Ref    `json:"refs,omitempty"`    // the numbered set-of-marks (interactive elements)
	Tabs          []Tab    `json:"tabs,omitempty"`    // open tabs/targets
}

// Act op names — the closed set of browser actions the model may emit. An unknown
// op fails loudly (fail-closed) rather than being silently skipped.
const (
	OpObserve  = "observe"  // re-snapshot only (no mutation)
	OpClick    = "click"    // click Ref or Selector
	OpType     = "type"     // type Text into Ref or Selector (focuses first)
	OpKey      = "key"      // press a discrete Key (e.g. "Enter") at current focus
	OpScroll   = "scroll"   // scroll by Dir (up/down/left/right) × Amount
	OpNavigate = "navigate" // load URL
	OpBack     = "back"     // history back
	OpForward  = "forward"  // history forward
	OpSelect   = "select"   // set a <select> Ref to option Text/Value
	OpWait     = "wait"     // bounded sleep MS, then settle (DOM-stability)
	OpExtract  = "extract"  // DEPRECATED back-compat alias for observe (no mutation). Extraction → the record_finding tool, NOT this op; no longer advertised to the model.
	OpClose    = "close"    // session protocol only: shut the daemon down
)

// Act is one instruction the host hands the driver. Only the fields relevant to
// Op are populated; the driver validates the combination. A model-supplied Act is
// UNTRUSTED data: every string is replayed as a CDP parameter (a selector, typed
// text, a URL), never as shell or code (I7).
type Act struct {
	Op       string `json:"op"`
	Ref      int    `json:"ref,omitempty"`      // element id from the latest snapshot (0 = unset → use Selector)
	Selector string `json:"selector,omitempty"` // CSS fallback when no Ref
	URL      string `json:"url,omitempty"`
	Text     string `json:"text,omitempty"`
	Key      string `json:"key,omitempty"`
	MS       int    `json:"ms,omitempty"`
	Dir      string `json:"dir,omitempty"`    // scroll direction
	Amount   int    `json:"amount,omitempty"` // scroll amount in px (driver applies a default)
}

// SessionRequest is one host→driver message over the file-queue protocol. Seq is a
// monotonically increasing request id the response echoes, so a reader never
// mismatches a reply.
type SessionRequest struct {
	Seq int `json:"seq"`
	Act Act `json:"act"`
}

// SessionResponse is the driver→host reply for one SessionRequest. Observation is
// the post-act page state; Error is a non-empty fail-closed detail when the act
// could not be applied. The driver NEVER fabricates a passing observation on
// failure.
type SessionResponse struct {
	Seq         int         `json:"seq"`
	Observation Observation `json:"observation"`
	Error       string      `json:"error,omitempty"`
}
