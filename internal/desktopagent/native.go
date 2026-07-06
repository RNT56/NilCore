package desktopagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"nilcore/internal/desktopwire"
	"nilcore/internal/guard"
	"nilcore/internal/model"
	"nilcore/internal/tools"
)

// NativeComputerTool is Path A: it advertises Anthropic's NATIVE `computer` beta tool
// (so the model emits its RL-trained pixel/coordinate action vocabulary) and, when the
// model calls it, TRANSLATES that native action into a desktopwire.Act against the SAME
// in-sandbox driver Path B uses. Path B's governed body is unchanged — only the model
// interface + perception (raw pixels) differ. It is opt-in (NILCORE_COMPUTER_NATIVE);
// the generic Path-B ComputerTool is the default.
//
// It implements tools.BuiltinProvider so the registry advertises the typed builtin def
// (+ beta header). The driver runs in --native mode (raw screenshots, fixed display
// dims), so the model grounds from pixels exactly as in Anthropic's reference loop.
type NativeComputerTool struct {
	Sess        Session
	MaxSteps    int
	MaxStagnant int // consecutive no-op acts before the harness nudges (default defaultMaxStagnant)
	EventSink   EventSink
	Approver    Approver // human gate for irreversible actions; nil ⇒ fail closed on one (I2)

	mu       sync.Mutex
	steps    int
	stagnant int
	lastSig  string
}

func (*NativeComputerTool) Name() string { return "computer" }

func (*NativeComputerTool) Description() string {
	// The action vocabulary is baked into the model (builtin); the description is
	// advisory. The provider sends the typed builtin def, not this text/schema.
	return "Control the computer (Anthropic native computer tool)."
}

// Schema is unused for a builtin (the provider sends the typed def), but the Tool
// interface requires it; return an empty object.
func (*NativeComputerTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// BuiltinDef advertises the native computer tool at the FIXED display dims (matching
// the driver's native-mode capture and the Xvfb geometry — 1:1 coordinates).
func (*NativeComputerTool) BuiltinDef() *model.BuiltinTool {
	return model.NewComputerTool(desktopwire.NativeDisplayW, desktopwire.NativeDisplayH).Builtin
}

func (n *NativeComputerTool) Run(ctx context.Context, workdir string, input json.RawMessage) (string, error) {
	out, _, err := n.RunWithImage(ctx, workdir, input)
	return out, err
}

func (n *NativeComputerTool) RunWithImage(ctx context.Context, _ string, input json.RawMessage) (string, *tools.Image, error) {
	if n.Sess == nil {
		return "", nil, fmt.Errorf("computer: no session")
	}
	act, err := translateNative(input)
	if err != nil {
		return "", nil, err
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	max := n.MaxSteps
	if max <= 0 {
		max = defaultMaxSteps
	}
	if n.steps >= max {
		return guard.Wrap("computer budget", fmt.Sprintf("step budget of %d reached — finish or report blocked.", max)), nil, nil
	}
	n.steps++

	// Irreversible-action gate, symmetric with Path B (I2). In pixel mode a click is
	// coordinate-based (no accessible target), so this mainly catches an Enter/Return
	// submit on a window that names a consequential action; a nil Approver fails CLOSED.
	// A blocked action consumes a step so a retrying model still terminates.
	if sig := irreversibleTarget(act, n.Sess.Latest()); sig != "" {
		if n.Approver == nil || !n.Approver.Approve("computer "+act.Op+" on irreversible target ("+sig+")") {
			body := fmt.Sprintf("the %s on %q was BLOCKED by the irreversible-action gate (matched %q) — not performed. A human must approve it; report this and finish if you cannot proceed.", act.Op, sig, sig)
			return guard.Wrap("computer gate", body), nil, nil
		}
	}

	obs, actErr := n.Sess.Act(ctx, act)

	// Never-retry stagnation detection (the discipline the package doc demands and that
	// must live in code, B7-cu.6): Path A previously had only the step budget, so a
	// model looping on an ineffective coordinate click got no nudge until the budget was
	// exhausted. We fingerprint the post-act observation (pixel-mode: window + ref count
	// + rung + version) and flag a run of no-ops, mirroring Path-B's ComputerTool.
	sig := nativeSignature(obs)
	stagnant := n.isStagnant(act.Op, sig, actErr != nil)
	n.lastSig = sig

	if n.EventSink != nil {
		n.EventSink(Step{N: n.steps, Op: act.Op, Window: obs.FocusedWindow, Rung: obs.Rung, Refs: len(obs.Refs), Version: obs.Version, Stagnant: stagnant, Errored: actErr != nil})
	}

	// Path A is pixel-mode: the screenshot IS the observation. Keep any text minimal
	// and fenced; the model grounds from the image.
	body := "screenshot updated"
	if actErr != nil {
		body = "ACTION ERROR: " + actErr.Error()
	} else if t := strings.TrimSpace(obs.FocusedWindow); t != "" {
		body = "focused window: " + t
	}
	if stagnant {
		maxStag := n.MaxStagnant
		if maxStag <= 0 {
			maxStag = defaultMaxStagnant
		}
		if n.stagnant >= maxStag {
			body += fmt.Sprintf("\n\n[harness] the last %d actions changed nothing — try a FUNDAMENTALLY different approach (a different coordinate/element, keyboard navigation, or finish if blocked).", n.stagnant)
		}
	}
	out := guard.Wrap("computer observation", body)
	if obs.ScreenshotB64 != "" {
		return out, &tools.Image{MediaType: "image/png", Base64: obs.ScreenshotB64}, nil
	}
	return out, nil, nil
}

// nativeSignature fingerprints a Path-A (pixel-mode) observation for stagnation. The
// native driver does not richly populate Refs/Title, so the stable signal is the
// focused window + the rung + the ref count. Version is deliberately EXCLUDED: the
// driver bumps it on every observe, so it would make two states never compare equal
// and stagnation would never fire.
func nativeSignature(o desktopwire.Observation) string {
	return fmt.Sprintf("%s|%d|%d", o.FocusedWindow, o.Rung, len(o.Refs))
}

// isStagnant updates and reports the consecutive-no-op counter (Path A). An observe is
// never counted (it is expected not to change the screen); an errored act that left the
// same signature also counts. Mirrors ComputerTool.isStagnant.
func (n *NativeComputerTool) isStagnant(op, sig string, errored bool) bool {
	if op == desktopwire.OpObserve {
		n.stagnant = 0
		return false
	}
	if sig != "" && sig == n.lastSig {
		n.stagnant++
		return true
	}
	if errored {
		n.stagnant++
		return true
	}
	n.stagnant = 0
	return false
}

// translateNative maps an Anthropic native `computer` action input to a
// desktopwire.Act. The native action vocabulary is the model's; we cover the common
// set FAITHFULLY — each click variant carries its real button/count, and drag/press/
// release map to their own ops — so a double/right/middle/drag action can no longer be
// silently demoted to a single left click (the mis-click bug). Read-only/positional
// actions (screenshot/cursor_position/mouse_move) degrade to a safe observe, and a
// genuinely UNKNOWN future action also degrades to observe (never a wrong mutation).
func translateNative(input json.RawMessage) (desktopwire.Act, error) {
	var in struct {
		Action          string `json:"action"`
		Coordinate      []int  `json:"coordinate"`
		StartCoordinate []int  `json:"start_coordinate"`
		Text            string `json:"text"`
		ScrollDirection string `json:"scroll_direction"`
		ScrollAmount    int    `json:"scroll_amount"`
		Duration        int    `json:"duration"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return desktopwire.Act{}, fmt.Errorf("bad computer input: %w", err)
	}
	switch in.Action {
	case "screenshot", "cursor_position", "mouse_move", "":
		return desktopwire.Act{Op: desktopwire.OpObserve}, nil
	case "left_click":
		return desktopwire.Act{Op: desktopwire.OpClick, Coordinate: in.Coordinate, Button: desktopwire.ButtonLeft, Count: 1}, nil
	case "double_click":
		return desktopwire.Act{Op: desktopwire.OpClick, Coordinate: in.Coordinate, Button: desktopwire.ButtonLeft, Count: 2}, nil
	case "triple_click":
		return desktopwire.Act{Op: desktopwire.OpClick, Coordinate: in.Coordinate, Button: desktopwire.ButtonLeft, Count: 3}, nil
	case "right_click":
		return desktopwire.Act{Op: desktopwire.OpClick, Coordinate: in.Coordinate, Button: desktopwire.ButtonRight, Count: 1}, nil
	case "middle_click":
		return desktopwire.Act{Op: desktopwire.OpClick, Coordinate: in.Coordinate, Button: desktopwire.ButtonMiddle, Count: 1}, nil
	case "left_click_drag":
		// Anthropic's drag carries the origin in start_coordinate and the destination in
		// coordinate. When start_coordinate is absent, the drag begins at the current
		// pointer (Coordinate empty ⇒ driver drags from where the cursor is).
		return desktopwire.Act{Op: desktopwire.OpDrag, Coordinate: in.StartCoordinate, To: in.Coordinate, Button: desktopwire.ButtonLeft}, nil
	case "left_mouse_down":
		return desktopwire.Act{Op: desktopwire.OpMouseDown, Coordinate: in.Coordinate, Button: desktopwire.ButtonLeft}, nil
	case "left_mouse_up":
		return desktopwire.Act{Op: desktopwire.OpMouseUp, Coordinate: in.Coordinate, Button: desktopwire.ButtonLeft}, nil
	case "type":
		return desktopwire.Act{Op: desktopwire.OpType, Text: in.Text}, nil
	case "key", "hold_key":
		return desktopwire.Act{Op: desktopwire.OpKey, Key: in.Text}, nil
	case "scroll":
		return desktopwire.Act{Op: desktopwire.OpScroll, Dir: in.ScrollDirection, Amount: in.ScrollAmount}, nil
	case "wait":
		ms := in.Duration * 1000
		if ms <= 0 {
			ms = 1000
		}
		return desktopwire.Act{Op: desktopwire.OpWait, MS: ms}, nil
	default:
		return desktopwire.Act{Op: desktopwire.OpObserve}, nil
	}
}
