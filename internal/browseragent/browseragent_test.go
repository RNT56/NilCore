package browseragent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/browserwire"
)

// fakeSession is a scriptable Session for hermetic tool tests (no daemon, no Chrome).
type fakeSession struct {
	got    []browserwire.Act
	respFn func(a browserwire.Act) (browserwire.Observation, error)
	latest browserwire.Observation
}

func (f *fakeSession) Act(_ context.Context, a browserwire.Act) (browserwire.Observation, error) {
	f.got = append(f.got, a)
	if f.respFn != nil {
		obs, err := f.respFn(a)
		f.latest = obs
		return obs, err
	}
	f.latest = browserwire.Observation{Version: 1, URL: "http://x.test/"}
	return f.latest, nil
}
func (f *fakeSession) Latest() browserwire.Observation { return f.latest }

func runOp(t *testing.T, b *BrowseTool, op string) (string, bool) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"op": op})
	out, img, err := b.RunWithImage(context.Background(), ".", in)
	if err != nil {
		t.Fatalf("RunWithImage(%s): %v", op, err)
	}
	return out, img != nil
}

func TestRendersObservationFenced(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{
			Version: 1, URL: "http://x.test/", Title: "Hello",
			Refs: []browserwire.Ref{{ID: 0, Role: "button", Name: "Submit"}},
			Text: "hello world",
		}, nil
	}}
	b := &BrowseTool{Sess: fs}
	out, _ := runOp(t, b, "observe")
	for _, want := range []string{"http://x.test/", "Submit", "[0]", "hello world"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered observation missing %q: %s", want, out)
		}
	}
}

func TestScreenshotPassedAsImageWhenNoRefs(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 1, ScreenshotB64: "QUJD"}, nil
	}}
	b := &BrowseTool{Sess: fs}
	_, hasImg := runOp(t, b, "observe")
	if !hasImg {
		t.Fatal("a screenshot-bearing observation must return an image")
	}
}

func TestBudgetIsEnforced(t *testing.T) {
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		// Each act lands on a NEW url so stagnation never trips before the budget.
		return browserwire.Observation{Version: 1, URL: "http://x.test/" + a.Op + a.URL}, nil
	}}
	b := &BrowseTool{Sess: fs, MaxSteps: 3}
	for i := 0; i < 3; i++ {
		in, _ := json.Marshal(map[string]any{"op": "navigate", "url": string(rune('a' + i))})
		if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	// The 4th call exceeds the budget and must return the budget message without
	// dispatching to the session.
	before := len(fs.got)
	out, _ := runOp(t, b, "observe")
	if !strings.Contains(out, "budget") {
		t.Fatalf("expected a budget message after the cap, got %q", out)
	}
	if len(fs.got) != before {
		t.Fatal("an over-budget act must not reach the session")
	}
}

func TestStagnationDetected(t *testing.T) {
	// The session always returns the SAME signature for a mutating act → stagnant.
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 1, URL: "http://same/", Title: "same"}, nil
	}}
	b := &BrowseTool{Sess: fs, MaxStagnant: 2}
	var last string
	for i := 0; i < 4; i++ {
		in, _ := json.Marshal(map[string]any{"op": "click", "ref": 1})
		// ref 1 isn't validated here (fake session ignores it); we exercise the tool.
		out, _, err := b.RunWithImage(context.Background(), ".", in)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		last = out
	}
	if !strings.Contains(last, "different approach") {
		t.Fatalf("expected a stagnation nudge after repeated no-ops, got %q", last)
	}
}

func TestForwardsActFields(t *testing.T) {
	fs := &fakeSession{}
	b := &BrowseTool{Sess: fs}
	in, _ := json.Marshal(map[string]any{"op": "type", "ref": 5, "text": "{{secret:pw}}"})
	if _, _, err := b.RunWithImage(context.Background(), ".", in); err != nil {
		t.Fatal(err)
	}
	if len(fs.got) != 1 || fs.got[0].Op != "type" || fs.got[0].Ref != 5 || fs.got[0].Text != "{{secret:pw}}" {
		t.Fatalf("act not forwarded verbatim (secret substitution is the session's job): %+v", fs.got)
	}
}

func TestEventSinkEmitsTrajectoryStep(t *testing.T) {
	var steps []Step
	fs := &fakeSession{respFn: func(a browserwire.Act) (browserwire.Observation, error) {
		return browserwire.Observation{Version: 2, URL: "http://x.test/" + a.Op, Refs: []browserwire.Ref{{ID: 0}}}, nil
	}}
	b := &BrowseTool{Sess: fs, EventSink: func(s Step) { steps = append(steps, s) }}
	if _, _, err := b.RunWithImage(context.Background(), ".", mustInput("navigate", "http://x.test/")); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 trajectory step, got %d", len(steps))
	}
	s := steps[0]
	if s.Op != "navigate" || s.Refs != 1 || s.Version != 2 || s.N != 1 {
		t.Fatalf("step metadata wrong: %+v", s)
	}
}

func mustInput(op, url string) []byte {
	b, _ := json.Marshal(map[string]any{"op": op, "url": url})
	return b
}
