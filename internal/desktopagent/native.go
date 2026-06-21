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
	Sess      Session
	MaxSteps  int
	EventSink EventSink

	mu    sync.Mutex
	steps int
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

	obs, actErr := n.Sess.Act(ctx, act)
	if n.EventSink != nil {
		n.EventSink(Step{N: n.steps, Op: act.Op, Window: obs.FocusedWindow, Rung: obs.Rung, Refs: len(obs.Refs), Version: obs.Version, Errored: actErr != nil})
	}

	// Path A is pixel-mode: the screenshot IS the observation. Keep any text minimal
	// and fenced; the model grounds from the image.
	body := "screenshot updated"
	if actErr != nil {
		body = "ACTION ERROR: " + actErr.Error()
	} else if t := strings.TrimSpace(obs.FocusedWindow); t != "" {
		body = "focused window: " + t
	}
	out := guard.Wrap("computer observation", body)
	if obs.ScreenshotB64 != "" {
		return out, &tools.Image{MediaType: "image/png", Base64: obs.ScreenshotB64}, nil
	}
	return out, nil, nil
}

// translateNative maps an Anthropic native `computer` action input to a
// desktopwire.Act. The native action vocabulary is the model's; we cover the common
// set and degrade unknown/compound actions to a safe observe (never a wrong mutation).
func translateNative(input json.RawMessage) (desktopwire.Act, error) {
	var in struct {
		Action          string `json:"action"`
		Coordinate      []int  `json:"coordinate"`
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
	case "left_click", "double_click", "triple_click", "right_click", "middle_click", "left_click_drag", "left_mouse_down", "left_mouse_up":
		return desktopwire.Act{Op: desktopwire.OpClick, Coordinate: in.Coordinate}, nil
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
