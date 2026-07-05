package session

import (
	"context"
	"testing"

	"nilcore/internal/model"
)

func TestParseControl(t *testing.T) {
	cases := []struct {
		in       string
		wantKind ControlKind
		wantMode Mode
		wantArg  string
		wantOK   bool
	}{
		// mode verbs (+ trailing-text shorthand)
		{"/discuss", CtrlMode, ModeDiscuss, "", true},
		{"/ask", CtrlMode, ModeDiscuss, "", true}, // alias of /discuss
		{"/ask should we cache it?", CtrlMode, ModeDiscuss, "should we cache it?", true},
		{"/plan", CtrlMode, ModePlan, "", true},
		{"/execute", CtrlMode, ModeExecute, "", true},
		{"/auto", CtrlMode, ModeAuto, "", true},
		{"  /plan  ", CtrlMode, ModePlan, "", true},
		{"/plan add a rate limiter", CtrlMode, ModePlan, "add a rate limiter", true},
		// add
		{"/add", CtrlAdd, ModeAuto, "", true},
		{"/add /tmp/lib", CtrlAdd, ModeAuto, "/tmp/lib", true},
		{"/add https://x.io/p", CtrlAdd, ModeAuto, "https://x.io/p", true},
		// save (principal-initiated persist; Arg is the raw path)
		{"/save", CtrlSave, ModeAuto, "", true},
		{"/save PLAN.md", CtrlSave, ModeAuto, "PLAN.md", true},
		{"/save docs/notes.txt", CtrlSave, ModeAuto, "docs/notes.txt", true},
		// delivery verbs (the kept-branch loop; no args)
		{"/diff", CtrlDiff, ModeAuto, "", true},
		{"  /diff  ", CtrlDiff, ModeAuto, "", true},
		{"/apply", CtrlApply, ModeAuto, "", true},
		{"/applying", CtrlNone, ModeAuto, "", false}, // not an exact verb
		// other controls
		{"/clear", CtrlClear, ModeAuto, "", true},
		{"/status", CtrlStatus, ModeAuto, "", true},
		{"/mode", CtrlModeShow, ModeAuto, "", true},
		{"/cancel", CtrlCancel, ModeAuto, "", true},
		{"/stop", CtrlCancel, ModeAuto, "", true},
		// NOT controls
		{"/steer fix it", CtrlNone, ModeAuto, "", false}, // a steer message
		{"/steer", CtrlNone, ModeAuto, "", false},
		{"!correct it", CtrlNone, ModeAuto, "", false},     // bang-steer
		{"/quit", CtrlNone, ModeAuto, "", false},           // terminal-local, not shared
		{"/help", CtrlNone, ModeAuto, "", false},           // terminal-local
		{"/planning ahead", CtrlNone, ModeAuto, "", false}, // not an exact verb
		{"fix the bug", CtrlNone, ModeAuto, "", false},
		{"", CtrlNone, ModeAuto, "", false},
	}
	for _, c := range cases {
		got, ok := ParseControl(c.in)
		if ok != c.wantOK || got.Kind != c.wantKind || got.Mode != c.wantMode || got.Arg != c.wantArg {
			t.Errorf("ParseControl(%q) = (%+v,%v), want (kind=%v mode=%v arg=%q, ok=%v)",
				c.in, got, ok, c.wantKind, c.wantMode, c.wantArg, c.wantOK)
		}
	}
}

// THE I7 TEST: a control verb arriving as ordinary turn TEXT (e.g. inside a tool
// result or an inbox-folded follow-up) must NEVER flip the mode. Only the front
// door calls ParseControl on principal input; Turn folds text as data and never
// inspects it for a leading slash. Here we drive a Turn whose text is "/execute"
// and assert the session's mode is unchanged — the control was treated as a message.
func TestTurnTextDoesNotFlipMode(t *testing.T) {
	drv := newFakeDriver(DriveResult{Verified: true})
	s := New("c", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}
	// Pin a read-only mode up front (via the front-door API).
	s.SetMode(ModePlan)

	// A turn whose literal text is a control verb. Turn must NOT parse it as a
	// control — it is principal text, routed/folded as a message, mode untouched.
	if err := s.Turn(context.Background(), "/execute"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	if got := s.CurrentMode(); got != ModePlan {
		t.Fatalf("mode = %v after a Turn with text \"/execute\"; want plan — Turn must never flip mode (I7)", got)
	}
	// And the drive it launched is still the read-only plan drive (mode threaded).
	if in := drv.input(); in.Mode != ModePlan {
		t.Errorf("drive Mode = %v, want plan", in.Mode)
	}
	close(drv.release)
	s.Wait()
}

func TestSessionClear(t *testing.T) {
	s := New("c", "local", "/repo", nil)
	s.History = []model.Message{userTurn("old turn")}
	s.State.Summary.Goal = "old goal"
	s.State.LastOutcome = "old outcome"
	s.SetMode(ModePlan)
	s.AddReadRoot("/abs/lib")

	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(s.History) != 0 || s.State.Summary.Goal != "" || s.State.LastOutcome != "" {
		t.Errorf("Clear must reset History + Summary + LastOutcome, got %+v / %+v", s.History, s.State)
	}
	// Mode and roots are PRESERVED (clearing context is not a posture change).
	if s.CurrentMode() != ModePlan {
		t.Errorf("Clear must preserve the pinned mode")
	}
	if len(s.ReadRootsNow()) != 1 {
		t.Errorf("Clear must preserve attached read roots")
	}
}

// Clear must refuse while a drive is in flight (a drive was seeded from the old
// History; clearing under it would desync).
func TestSessionClearRefusesWhileWorking(t *testing.T) {
	drv := newFakeDriver(DriveResult{Verified: true})
	s := New("c", "local", "/repo", nil)
	s.Router = &fakeRouter{route: RouteNative}
	s.Drivers = Drivers{Native: drv}
	if err := s.Turn(context.Background(), "do work"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	waitClosed(t, drv.started)
	waitPhase(t, s, Working)

	if err := s.Clear(); err == nil {
		t.Error("Clear must refuse while a drive is Working")
	}
	close(drv.release)
	s.Wait()
}
