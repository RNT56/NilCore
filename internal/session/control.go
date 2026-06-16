package session

// control.go is the SHARED control-verb parser used by BOTH front doors — the
// terminal REPL (cmd/nilcore/chat.go) and the serve intake (internal/server) — so
// /discuss /plan /execute /auto /add /clear /mode /status /cancel behave identically
// over the keyboard and over Telegram/Slack, defined once.
//
// THE TRUST BOUNDARY (I7): ParseControl is pure string classification with no IO,
// and it is called ONLY on PRINCIPAL top-level input — a REPL line, or a serve
// req.Goal that has already passed channel.Authorized.Permit. It is NEVER called
// inside Turn, on an inbox-folded follow-up, or on tool/web output. So untrusted
// content (a tool result or fetched page containing "/execute") can never trigger a
// control — only the authenticated principal at the front door can. A bare `!` and
// `/steer` are deliberately NOT controls: they remain steer MESSAGES, classified by
// classifyInterrupt on Turn. `/quit` and `/help` are front-door-local (terminal
// only) and are intentionally not recognized here.

import "strings"

// ControlKind names a parsed control command.
type ControlKind int

const (
	// CtrlNone: the line is not a control verb (an ordinary message / a steer).
	CtrlNone ControlKind = iota
	// CtrlMode: set the behavioral mode. Mode is the target; Arg is any trailing
	// text to ALSO submit as a turn ("/plan add a limiter" pins plan AND asks).
	CtrlMode
	// CtrlAdd: attach context. Arg is the raw path-or-URL (resolution/fetch is the
	// front door's job — session stays pure). Empty Arg ⇒ show usage/list.
	CtrlAdd
	// CtrlClear: reset the conversation's in-memory history (keep mode + roots).
	CtrlClear
	// CtrlModeShow: report the current mode.
	CtrlModeShow
	// CtrlStatus: report phase + mode + attached context.
	CtrlStatus
	// CtrlContext: report context-window usage (the gauge as text).
	CtrlContext
	// CtrlCancel: abort the in-flight run, stay in the conversation.
	CtrlCancel
)

// Control is the parsed result of a control line.
type Control struct {
	Kind ControlKind
	Mode Mode   // set for CtrlMode
	Arg  string // set for CtrlMode (trailing request) and CtrlAdd (path/url)
}

// controlModeVerbs maps each mode control verb to the Mode it pins.
var controlModeVerbs = map[string]Mode{
	"/discuss": ModeDiscuss,
	"/plan":    ModePlan,
	"/execute": ModeExecute,
	"/auto":    ModeAuto,
}

// ParseControl classifies one principal top-level line. ok is false for an ordinary
// message, a steer (`!`/`/steer`), or a front-door-local verb (`/quit`/`/help`). See
// the file header for the I7 trust boundary — callers MUST apply this only to
// principal front-door input.
func ParseControl(line string) (Control, bool) {
	t := strings.TrimSpace(line)
	if t == "" || !strings.HasPrefix(t, "/") {
		return Control{}, false
	}
	first, rest := t, ""
	if i := strings.IndexAny(t, " \t"); i >= 0 {
		first, rest = t[:i], strings.TrimSpace(t[i+1:])
	}
	switch first {
	case "/steer":
		return Control{}, false // a steer message, not a control (classified on Turn)
	case "/cancel", "/stop":
		return Control{Kind: CtrlCancel}, true
	case "/clear":
		return Control{Kind: CtrlClear}, true
	case "/status":
		return Control{Kind: CtrlStatus}, true
	case "/context":
		return Control{Kind: CtrlContext}, true
	case "/mode":
		return Control{Kind: CtrlModeShow}, true
	case "/add":
		return Control{Kind: CtrlAdd, Arg: rest}, true
	}
	if m, ok := controlModeVerbs[first]; ok {
		return Control{Kind: CtrlMode, Mode: m, Arg: rest}, true
	}
	return Control{}, false
}
