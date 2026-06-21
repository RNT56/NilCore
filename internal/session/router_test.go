package session

import (
	"context"
	"errors"
	"testing"

	"nilcore/internal/model"
)

// scriptModel is a hermetic fake Provider: it returns a fixed reply (no network)
// and counts calls so a test can assert the classifier was (or was not) invoked.
type scriptModel struct {
	reply string
	err   error
	calls int
}

func (m *scriptModel) Model() string { return "script" }

func (m *scriptModel) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	m.calls++
	if m.err != nil {
		return model.Response{}, m.err
	}
	return model.Response{Content: []model.Block{{Type: "text", Text: m.reply}}}, nil
}

// alwaysSimple / alwaysComplex are the two ShouldSupervise heuristic stubs the
// router reconciles against.
func alwaysSimple(string) bool  { return false }
func alwaysComplex(string) bool { return true }

// TestRouteClassifierProposals checks that a parseable classifier proposal,
// reconciled with the ShouldSupervise heuristic, yields the expected Route for the
// trivial-fix, feature, project, and chat cases.
func TestRouteClassifierProposals(t *testing.T) {
	cases := []struct {
		name      string
		reply     string
		heuristic func(string) bool
		text      string
		want      Route
	}{
		{
			name:      "trivial fix routes native",
			reply:     `{"route":"native","reason":"a one-line typo fix"}`,
			heuristic: alwaysSimple,
			text:      "fix the typo in the error string",
			want:      RouteNative,
		},
		{
			name:      "feature routes supervise",
			reply:     `{"route":"supervise","reason":"multi-file feature"}`,
			heuristic: alwaysComplex,
			text:      "add pagination across the list endpoints",
			want:      RouteSupervise,
		},
		{
			name:      "whole project routes project",
			reply:     `{"route":"project","reason":"scaffold a service"}`,
			heuristic: alwaysComplex,
			text:      "build a URL shortener service with tests and a Dockerfile",
			want:      RouteProject,
		},
		{
			name:      "meta question routes chat",
			reply:     `{"route":"chat","reason":"a status question"}`,
			heuristic: alwaysComplex, // chat is honored regardless of the heuristic
			text:      "what are you working on right now?",
			want:      RouteChat,
		},
		{
			// Fix A: a model "native" on 40 words of trivial chatter is HONORED as
			// native — the heuristic no longer UPGRADES it to supervise. (Previously
			// this asserted an upgrade.)
			name:      "native proposal wins even when heuristic says complex",
			reply:     `{"route":"native","reason":"a one-line tweak despite the long ask"}`,
			heuristic: alwaysComplex,
			text:      "please could you go ahead and adjust the copy in the footer of the landing page so that the year reads two thousand and twenty six instead of the old value that is currently shown there right now today",
			want:      RouteNative,
		},
		{
			// Fix A: a model "supervise" on a SHORT, no-keyword goal is HONORED as
			// supervise — the heuristic no longer DOWNGRADES it to native.
			// "rewrite the auth subsystem" is <40 words and has no trigger keyword, so
			// the old string heuristic would have wrongly sized it simple.
			name:      "supervise proposal wins even when heuristic says simple",
			reply:     `{"route":"supervise","reason":"a cross-cutting rewrite"}`,
			heuristic: alwaysSimple,
			text:      "rewrite the auth subsystem",
			want:      RouteSupervise,
		},
		{
			// Fix A: a model "project" is HONORED — the heuristic no longer downgrades
			// a too-large estimate. The classifier's whole-project sizing wins.
			name:      "project proposal wins even when heuristic says simple",
			reply:     `{"route":"project","reason":"scaffold a whole service"}`,
			heuristic: alwaysSimple,
			text:      "stand up a new billing service",
			want:      RouteProject,
		},
		{
			name:      "chatty wrapper around the json still parses",
			reply:     "Sure, here is my call:\n{\"route\":\"native\",\"reason\":\"small\"}\nHope that helps!",
			heuristic: alwaysSimple,
			text:      "fix a bug",
			want:      RouteNative,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := &scriptModel{reply: tc.reply}
			r := &SupervisorFirstRouter{Classifier: cls, ShouldSupervise: tc.heuristic}
			got, err := r.Route(context.Background(), tc.text, WorkState{})
			if err != nil {
				t.Fatalf("Route err = %v", err)
			}
			if got != tc.want {
				t.Errorf("route = %v, want %v", got, tc.want)
			}
			if cls.calls != 1 {
				t.Errorf("classifier calls = %d, want exactly 1", cls.calls)
			}
		})
	}
}

// TestRouteContinueReferencesGoal asserts the persistence requirement: a follow-up
// that references the active goal yields RouteContinue WITHOUT a classifier call.
func TestRouteContinueReferencesGoal(t *testing.T) {
	cls := &scriptModel{reply: `{"route":"native"}`}
	r := &SupervisorFirstRouter{Classifier: cls, ShouldSupervise: alwaysSimple}
	st := WorkState{}
	st.Summary.Goal = "build a URL shortener service"

	cases := []struct {
		name string
		text string
	}{
		{"shares a distinctive word", "the shortener should also rate-limit"},
		{"explicit continue verb", "continue with that"},
		{"keep going phrase", "keep going please"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Route(context.Background(), tc.text, st)
			if err != nil {
				t.Fatalf("Route err = %v", err)
			}
			if got != RouteContinue {
				t.Errorf("route = %v, want RouteContinue", got)
			}
		})
	}
	if cls.calls != 0 {
		t.Errorf("classifier calls = %d, want 0 (continue is local, no model)", cls.calls)
	}
}

// TestRouteNoContinueForUnrelatedGoal asserts that an unrelated follow-up does NOT
// spuriously continue: it falls through to the classifier.
func TestRouteNoContinueForUnrelatedGoal(t *testing.T) {
	cls := &scriptModel{reply: `{"route":"native","reason":"small"}`}
	r := &SupervisorFirstRouter{Classifier: cls, ShouldSupervise: alwaysSimple}
	st := WorkState{}
	st.Summary.Goal = "build a URL shortener service"

	got, err := r.Route(context.Background(), "fix a typo in the login page", st)
	if err != nil {
		t.Fatalf("Route err = %v", err)
	}
	if got != RouteNative {
		t.Errorf("route = %v, want RouteNative (unrelated goal should not continue)", got)
	}
	if cls.calls != 1 {
		t.Errorf("classifier calls = %d, want 1 (unrelated ⇒ classify a fresh drive)", cls.calls)
	}
}

// TestRouteUnparseableFallsBackToHeuristic is the core safety acceptance: garbage
// classifier output must fall back to the pure ShouldSupervise function (no crash,
// no second model call, not silently RouteNative when the heuristic says complex).
func TestRouteUnparseableFallsBackToHeuristic(t *testing.T) {
	cases := []struct {
		name      string
		reply     string
		heuristic func(string) bool
		want      Route
	}{
		{"garbage ⇒ heuristic complex ⇒ supervise", "i cannot help with that", alwaysComplex, RouteSupervise},
		{"garbage ⇒ heuristic simple ⇒ native", "i cannot help with that", alwaysSimple, RouteNative},
		{"empty reply ⇒ heuristic complex ⇒ supervise", "", alwaysComplex, RouteSupervise},
		{"valid json but unknown route ⇒ heuristic complex ⇒ supervise", `{"route":"frobnicate"}`, alwaysComplex, RouteSupervise},
		{"valid json but unknown route ⇒ heuristic simple ⇒ native", `{"route":"frobnicate"}`, alwaysSimple, RouteNative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := &scriptModel{reply: tc.reply}
			r := &SupervisorFirstRouter{Classifier: cls, ShouldSupervise: tc.heuristic}
			got, err := r.Route(context.Background(), "do something", WorkState{})
			if err != nil {
				t.Fatalf("Route err = %v", err)
			}
			if got != tc.want {
				t.Errorf("route = %v, want %v (fallback to heuristic)", got, tc.want)
			}
			// Exactly one model call (the failed classify); the fallback uses no model.
			if cls.calls != 1 {
				t.Errorf("classifier calls = %d, want exactly 1 (fallback is pure)", cls.calls)
			}
		})
	}
}

// TestRouteTransportErrorPropagates asserts a model transport fault is returned to
// the caller (the Session returns to Idle), distinct from a parse failure.
func TestRouteTransportErrorPropagates(t *testing.T) {
	wantErr := errors.New("boom")
	cls := &scriptModel{err: wantErr}
	r := &SupervisorFirstRouter{Classifier: cls, ShouldSupervise: alwaysComplex}

	_, err := r.Route(context.Background(), "do something", WorkState{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the transport error propagated", err)
	}
}

// TestRouteNilClassifierDegradesToHeuristic asserts the router still works with no
// classifier wired — it degrades to the pure heuristic rather than crashing.
func TestRouteNilClassifierDegradesToHeuristic(t *testing.T) {
	r := &SupervisorFirstRouter{ShouldSupervise: alwaysComplex}
	got, err := r.Route(context.Background(), "do something big", WorkState{})
	if err != nil {
		t.Fatalf("Route err = %v", err)
	}
	if got != RouteSupervise {
		t.Errorf("route = %v, want RouteSupervise (heuristic-only path)", got)
	}
}

// TestRouteNilHeuristic asserts that with no heuristic wired (the clamp can't fire),
// a parseable proposal is honored as-is, and the unparseable FALLBACK path — which
// is the only place the (here nil) heuristic is consulted — defaults to RouteNative.
func TestRouteNilHeuristic(t *testing.T) {
	// Parseable supervise proposal, nil heuristic ⇒ honored as-is (Fix A: the
	// classifier proposal wins; the heuristic is never an overrule).
	cls := &scriptModel{reply: `{"route":"supervise"}`}
	r := &SupervisorFirstRouter{Classifier: cls}
	got, err := r.Route(context.Background(), "a feature", WorkState{})
	if err != nil {
		t.Fatalf("Route err = %v", err)
	}
	if got != RouteSupervise {
		t.Errorf("route = %v, want RouteSupervise (proposal honored as-is, nil heuristic does not overrule)", got)
	}

	// Unparseable, nil heuristic ⇒ fallback to RouteNative (not a crash).
	cls2 := &scriptModel{reply: "garbage"}
	r2 := &SupervisorFirstRouter{Classifier: cls2}
	got2, err := r2.Route(context.Background(), "anything", WorkState{})
	if err != nil {
		t.Fatalf("Route err = %v", err)
	}
	if got2 != RouteNative {
		t.Errorf("route = %v, want RouteNative (nil heuristic fallback)", got2)
	}
}

// TestRouteClampDownBackstop covers the OPTIONAL, default-off ClampDownToNative
// lever: it is INERT by default (proposal wins) and, when enabled, ONLY clamps a
// large proposal DOWN to native when the heuristic says simple — it never upgrades.
func TestRouteClampDownBackstop(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		clamp bool
		heur  func(string) bool
		want  Route
	}{
		// Default-off: the supervise proposal wins even though the heuristic says simple.
		{"default off ⇒ supervise wins", `{"route":"supervise"}`, false, alwaysSimple, RouteSupervise},
		// Enabled + heuristic simple ⇒ clamp supervise down to native.
		{"clamp on + simple ⇒ native", `{"route":"supervise"}`, true, alwaysSimple, RouteNative},
		// Enabled + heuristic simple ⇒ clamp project down to native too.
		{"clamp on + simple ⇒ project→native", `{"route":"project"}`, true, alwaysSimple, RouteNative},
		// Enabled but heuristic complex ⇒ no clamp (proposal kept).
		{"clamp on + complex ⇒ supervise kept", `{"route":"supervise"}`, true, alwaysComplex, RouteSupervise},
		// Clamp is one-directional: it never UPGRADES a native proposal.
		{"clamp on never upgrades native", `{"route":"native"}`, true, alwaysComplex, RouteNative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cls := &scriptModel{reply: tc.reply}
			r := &SupervisorFirstRouter{Classifier: cls, ShouldSupervise: tc.heur, ClampDownToNative: tc.clamp}
			got, err := r.Route(context.Background(), "do the thing", WorkState{})
			if err != nil {
				t.Fatalf("Route err = %v", err)
			}
			if got != tc.want {
				t.Errorf("route = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseRoute is a focused table test on the defensive parser.
func TestParseRoute(t *testing.T) {
	cases := []struct {
		in       string
		wantRt   Route
		wantOk   bool
		wantReas string
	}{
		{`{"route":"native","reason":"x"}`, RouteNative, true, "x"},
		{`{"route":"SUPERVISE"}`, RouteSupervise, true, ""},
		{`prefix {"route":"project","reason":"y"} suffix`, RouteProject, true, "y"},
		{`{"route":"chat"}`, RouteChat, true, ""},
		{`{"route":"feature"}`, RouteSupervise, true, ""}, // alias
		{`{"route":"unknown"}`, RouteContinue, false, ""},
		{`no json here`, RouteContinue, false, ""},
		{`{not json}`, RouteContinue, false, ""},
		{``, RouteContinue, false, ""},
	}
	for _, tc := range cases {
		gotRt, gotReas, gotOk := parseRoute(tc.in)
		if gotOk != tc.wantOk || gotRt != tc.wantRt || (gotOk && gotReas != tc.wantReas) {
			t.Errorf("parseRoute(%q) = (%v,%q,%v), want (%v,%q,%v)",
				tc.in, gotRt, gotReas, gotOk, tc.wantRt, tc.wantReas, tc.wantOk)
		}
	}
}
