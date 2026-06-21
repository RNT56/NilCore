package desktopagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/desktopwire"
)

type fakeSession struct {
	got    []desktopwire.Act
	respFn func(a desktopwire.Act) (desktopwire.Observation, error)
	latest desktopwire.Observation
}

func (f *fakeSession) Act(_ context.Context, a desktopwire.Act) (desktopwire.Observation, error) {
	f.got = append(f.got, a)
	if f.respFn != nil {
		o, err := f.respFn(a)
		f.latest = o
		return o, err
	}
	f.latest = desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI}
	return f.latest, nil
}
func (f *fakeSession) Latest() desktopwire.Observation { return f.latest }

func run(t *testing.T, c *ComputerTool, m map[string]any) (string, bool) {
	t.Helper()
	in, _ := json.Marshal(m)
	out, img, err := c.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage(%v): %v", m, err)
	}
	return out, img != nil
}

func TestRendersRefsFenced(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, Rung: desktopwire.RungATSPI, FocusedWindow: "Settings",
			Refs: []desktopwire.Ref{{ID: 0, Role: "push button", Name: "Save"}}}, nil
	}}
	c := &ComputerTool{Sess: fs}
	out, _ := run(t, c, map[string]any{"op": "observe"})
	for _, want := range []string{"Settings", "Save", "[0]", "accessibility refs"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestScreenshotImageWhenNoRefs(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, Rung: desktopwire.RungCoordinate, ScreenshotB64: "QUJD"}, nil
	}}
	c := &ComputerTool{Sess: fs}
	out, hasImg := run(t, c, map[string]any{"op": "observe"})
	if !hasImg {
		t.Fatal("a screenshot observation must return an image")
	}
	if !strings.Contains(out, "coordinate") {
		t.Fatalf("rung-3 render should mention coordinate: %s", out)
	}
}

func TestBudgetEnforced(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, FocusedWindow: a.Key + a.Op}, nil
	}}
	c := &ComputerTool{Sess: fs, MaxSteps: 2}
	for i := 0; i < 2; i++ {
		run(t, c, map[string]any{"op": "key", "key": string(rune('a' + i))})
	}
	before := len(fs.got)
	out, _ := run(t, c, map[string]any{"op": "observe"})
	if !strings.Contains(out, "budget") {
		t.Fatalf("expected budget message, got %s", out)
	}
	if len(fs.got) != before {
		t.Fatal("over-budget act reached the session")
	}
}

func TestStagnation(t *testing.T) {
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 1, FocusedWindow: "same", Title: "same"}, nil
	}}
	c := &ComputerTool{Sess: fs, MaxStagnant: 2}
	var last string
	for i := 0; i < 4; i++ {
		last, _ = run(t, c, map[string]any{"op": "click", "coordinate": []int{1, 1}})
	}
	if !strings.Contains(last, "different approach") {
		t.Fatalf("expected stagnation nudge, got %s", last)
	}
}

func TestEventSink(t *testing.T) {
	var steps []Step
	fs := &fakeSession{respFn: func(a desktopwire.Act) (desktopwire.Observation, error) {
		return desktopwire.Observation{Version: 2, Rung: desktopwire.RungATSPI, FocusedWindow: "calc",
			Refs: []desktopwire.Ref{{ID: 0}}}, nil
	}}
	c := &ComputerTool{Sess: fs, EventSink: func(s Step) { steps = append(steps, s) }}
	run(t, c, map[string]any{"op": "click", "ref": 0})
	if len(steps) != 1 || steps[0].Op != "click" || steps[0].Window != "calc" || steps[0].Rung != desktopwire.RungATSPI {
		t.Fatalf("trajectory step wrong: %+v", steps)
	}
}
