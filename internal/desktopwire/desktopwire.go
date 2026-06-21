// Package desktopwire is the stdlib-only wire contract shared by the desktop
// computer-use tool (internal/tools/computer.go), the host-side session
// (internal/desktopsession), and the in-sandbox nilcore-desktop driver
// (cmd/tools/nilcore-desktop). It is the sibling of internal/browserwire: the
// same shell-quote + observation + session-protocol discipline, but for a virtual
// X11 desktop instead of a headless browser (Phase CU, docs/ROADMAP-COMPUTER-USE.md).
//
// WHY a leaf: host and driver must agree on the Observation/Act/session-protocol
// shape byte-for-byte, and the single-quote escaper is the I4 quoting boundary for
// any model-supplied string the driver replays. Extracting them here makes that
// boundary ONE tested copy, with zero nilcore imports so the standalone driver can
// depend on it.
//
// Trust boundary (I7): every Role/Name/Value/Title/Text and every screenshot is
// SCREEN-controlled and therefore UNTRUSTED — the harness fences it as data, never
// instructions.
package desktopwire

import "strings"

// ShellSingleQuote wraps s for safe use in `sh -c`, escaping embedded single
// quotes, so a model-supplied selector/text/coordinate string replayed by the
// driver cannot break out of the quoting to smuggle a second command (mirrors
// browserwire.ShellSingleQuote — the I4 quoting boundary).
func ShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// NativeDisplayW/H are the FIXED virtual-display dimensions for Path A (the native
// Anthropic computer tool). The Xvfb geometry in images/sandbox-desktop, the
// driver's native-mode capture, and the native tool's declared display_*_px MUST all
// equal these — so coordinates map 1:1 with no rescale (the #1 mis-click bug avoided
// by construction). XGA-class, within every model's pixel budget.
const (
	NativeDisplayW = 1280
	NativeDisplayH = 800
)

// Box is an on-screen rectangle in true virtual-display pixels.
type Box struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Center returns the box's centre point, where a coordinate click lands.
func (b Box) Center() (int, int) { return b.X + b.W/2, b.Y + b.H/2 }

// Empty reports a zero-area box (not actionable).
func (b Box) Empty() bool { return b.W <= 0 || b.H <= 0 }

// Ref is one interactive element in a desktop set-of-marks snapshot — the
// accessibility (AT-SPI) twin of browserwire.Ref. The model references an element
// by its integer ID ("click ref 12") rather than a raw coordinate. Role/Name/Value
// are toolkit-/app-controlled and therefore UNTRUSTED (I7). Actions lists the
// AT-SPI actions the node exposes (e.g. "click","press") so the driver can prefer
// Action.DoAction (DPI-independent) over a coordinate click. Box is the on-screen
// extent for the SoM overlay / coordinate fallback.
type Ref struct {
	ID      int      `json:"id"`
	Role    string   `json:"role"`              // a11y role: push button, text, … (UNTRUSTED)
	Name    string   `json:"name"`              // accessible name (UNTRUSTED)
	Value   string   `json:"value,omitempty"`   // current value for editable nodes (UNTRUSTED)
	Box     Box      `json:"box"`               // on-screen extent (true display pixels)
	Actions []string `json:"actions,omitempty"` // AT-SPI actions available (DoAction targets)
}

// Rung names which perception rung produced an Observation (set-of-marks ladder).
const (
	RungATSPI      = 1 // accessibility refs (cheapest, exact)
	RungSoM        = 2 // SoM-annotated screenshot (boxes from AT-SPI/CV)
	RungCoordinate = 3 // raw coordinate pointing (last resort)
)

// Observation is the JSON contract the driver returns over the session protocol.
// Unknown fields are ignored; parsed as data, never executed (I7). Refs is the
// numbered set-of-marks; ScreenshotB64 is populated on Rung 2/3 (and on request);
// Rung records which ladder rung produced it.
type Observation struct {
	Title         string   `json:"title,omitempty"`          // focused window title (UNTRUSTED)
	Text          string   `json:"text,omitempty"`           // a bounded text excerpt (UNTRUSTED)
	FocusedWindow string   `json:"focused_window,omitempty"` // window identity for the ladder cache (UNTRUSTED)
	Console       []string `json:"console,omitempty"`        // driver/app diagnostics (UNTRUSTED)
	ScreenshotB64 string   `json:"screenshot_b64,omitempty"` // marked PNG (Rung 2) or raw (Rung 3), base64
	Version       uint64   `json:"version,omitempty"`        // snapshot version for ref staleness checks
	Rung          int      `json:"rung,omitempty"`           // which perception rung produced this
	Refs          []Ref    `json:"refs,omitempty"`           // the numbered set-of-marks
}

// Act op names — the closed set of desktop actions the model may emit. An unknown
// op fails loudly (fail-closed).
const (
	OpObserve = "observe" // re-snapshot only
	OpClick   = "click"   // click Ref (DoAction/box-centre) OR Coordinate
	OpType    = "type"    // type Text at current focus (or into Ref first)
	OpKey     = "key"     // press a key chord (e.g. "ctrl+s", "Return")
	OpScroll  = "scroll"  // scroll Dir × Amount at focus/Ref
	OpWait    = "wait"    // bounded settle (MS)
	OpClose   = "close"   // session protocol only: shut the daemon down
)

// Act is one instruction the host hands the driver. Only the fields relevant to Op
// are populated; the driver validates the combination. A model-supplied Act is
// UNTRUSTED data: every string is replayed as a driver/X11 parameter, never as
// shell or code (I7). Coordinate is in the RESIZED image space the model saw; the
// driver rescales it to true display pixels in one place (the #1 mis-click bug).
type Act struct {
	Op         string `json:"op"`
	Ref        int    `json:"ref,omitempty"`        // element id from the latest snapshot (Rung 1/2)
	Coordinate []int  `json:"coordinate,omitempty"` // [x,y] in resized image space (Rung 3)
	Text       string `json:"text,omitempty"`
	Key        string `json:"key,omitempty"`
	Dir        string `json:"dir,omitempty"`    // scroll direction
	Amount     int    `json:"amount,omitempty"` // scroll amount
	MS         int    `json:"ms,omitempty"`
}

// SessionRequest is one host→driver message over the file-queue protocol.
type SessionRequest struct {
	Seq int `json:"seq"`
	Act Act `json:"act"`
}

// SessionResponse is the driver→host reply. Observation is the post-act state;
// Error is a non-empty fail-closed detail when the act could not be applied. The
// driver NEVER fabricates a passing observation on failure.
type SessionResponse struct {
	Seq         int         `json:"seq"`
	Observation Observation `json:"observation"`
	Error       string      `json:"error,omitempty"`
}
