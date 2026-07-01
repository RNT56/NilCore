package desktopagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/desktopwire"
	"nilcore/internal/model"
	"nilcore/internal/tools"
)

func TestTranslateNative(t *testing.T) {
	cases := []struct {
		in   string
		op   string
		want func(desktopwire.Act) bool
	}{
		{`{"action":"screenshot"}`, desktopwire.OpObserve, nil},
		{`{"action":"left_click","coordinate":[100,200]}`, desktopwire.OpClick, func(a desktopwire.Act) bool { return len(a.Coordinate) == 2 && a.Coordinate[0] == 100 }},
		{`{"action":"type","text":"hi"}`, desktopwire.OpType, func(a desktopwire.Act) bool { return a.Text == "hi" }},
		{`{"action":"key","text":"ctrl+s"}`, desktopwire.OpKey, func(a desktopwire.Act) bool { return a.Key == "ctrl+s" }},
		{`{"action":"scroll","scroll_direction":"down","scroll_amount":3}`, desktopwire.OpScroll, func(a desktopwire.Act) bool { return a.Dir == "down" && a.Amount == 3 }},
		{`{"action":"wait","duration":2}`, desktopwire.OpWait, func(a desktopwire.Act) bool { return a.MS == 2000 }},
		{`{"action":"unknown_future_action"}`, desktopwire.OpObserve, nil}, // degrade safely
	}
	for _, c := range cases {
		a, err := translateNative(json.RawMessage(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if a.Op != c.op {
			t.Errorf("%s → op %q, want %q", c.in, a.Op, c.op)
		}
		if c.want != nil && !c.want(a) {
			t.Errorf("%s → unexpected act %+v", c.in, a)
		}
	}
}

func TestNativeBuiltinDef(t *testing.T) {
	nt := &NativeComputerTool{}
	// It is registered as a builtin provider, so the registry advertises the typed def.
	var _ tools.BuiltinProvider = nt
	def := nt.BuiltinDef()
	if def == nil || def.Type != model.ComputerToolV20251124 {
		t.Fatalf("builtin def wrong: %+v", def)
	}
	if def.DisplayWidthPx != desktopwire.NativeDisplayW || def.DisplayHeightPx != desktopwire.NativeDisplayH {
		t.Fatalf("display dims = %dx%d, want %dx%d", def.DisplayWidthPx, def.DisplayHeightPx, desktopwire.NativeDisplayW, desktopwire.NativeDisplayH)
	}
}

func TestNativeToolDispatch(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, Rung: desktopwire.RungCoordinate, ScreenshotB64: "QUJD"}, nil
	}}
	nt := &NativeComputerTool{Sess: fs}
	out, img, err := nt.RunWithImage(context.Background(), ".", json.RawMessage(`{"action":"left_click","coordinate":[10,20]}`))
	if err != nil {
		t.Fatal(err)
	}
	if img == nil {
		t.Fatal("native tool should return the screenshot image (pixel mode)")
	}
	if len(fs.got) != 1 || fs.got[0].Op != desktopwire.OpClick || fs.got[0].Coordinate[0] != 10 {
		t.Fatalf("native click not translated to the driver: %+v", fs.got)
	}
	_ = out
}

// TestNativeStagnationNudge proves B7-cu.6: Path A now flags a run of no-op acts and
// emits the same [harness] nudge Path B does, instead of spinning silently to the step
// budget. The fake driver returns the SAME pixel-mode signature (window/rung/refs) for
// every click, so repeated clicks are stagnant.
func TestNativeStagnationNudge(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, Rung: desktopwire.RungCoordinate, FocusedWindow: "App", ScreenshotB64: "QUJD"}, nil
	}}
	nt := &NativeComputerTool{Sess: fs, MaxStagnant: 2}
	var last string
	for i := 0; i < 4; i++ {
		out, _, err := nt.RunWithImage(context.Background(), ".", json.RawMessage(`{"action":"left_click","coordinate":[10,20]}`))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		last = out
	}
	if !strings.Contains(last, "changed nothing") {
		t.Fatalf("expected a stagnation nudge after repeated no-op clicks, got %q", last)
	}
}

// TestNativeStagnationResetsOnChange confirms a genuinely-changing screen never trips
// the nudge (no false positives).
func TestNativeStagnationResetsOnChange(t *testing.T) {
	step := 0
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		step++
		// A different focused window each step ⇒ the signature changes ⇒ never stagnant.
		return desktopwire.Observation{Version: uint64(step), Rung: desktopwire.RungCoordinate, FocusedWindow: "Win" + string(rune('A'+step)), ScreenshotB64: "QUJD"}, nil
	}}
	nt := &NativeComputerTool{Sess: fs, MaxStagnant: 2}
	for i := 0; i < 4; i++ {
		out, _, err := nt.RunWithImage(context.Background(), ".", json.RawMessage(`{"action":"left_click","coordinate":[1,1]}`))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if strings.Contains(out, "changed nothing") {
			t.Fatalf("a changing screen must not trip the stagnation nudge: %q", out)
		}
	}
}

// The registry advertises the native tool as a builtin (the loop-dispatch wire).
func TestRegistryAdvertisesBuiltin(t *testing.T) {
	reg := tools.NewRegistry(&NativeComputerTool{})
	defs := reg.Defs()
	if len(defs) != 1 || defs[0].Builtin == nil {
		t.Fatalf("registry did not advertise the native tool as a builtin: %+v", defs)
	}
	// And a normal tool stays non-builtin.
	if (&ComputerTool{}).Name() != "computer" {
		t.Fatal("sanity: generic tool name")
	}
}
