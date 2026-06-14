package trigger

import (
	"context"
	"testing"
)

func TestReversibleSelfStarts(t *testing.T) {
	var startedGoal string
	tr := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false }, // would deny, but reversible shouldn't ask
		Start:   func(_ context.Context, goal string) error { startedGoal = goal; return nil },
	}
	started, err := tr.Handle(context.Background(), Signal{Source: "ci", Goal: "fix the failing test in math_test.go"})
	if err != nil || !started {
		t.Fatalf("reversible signal should self-start: %v %v", started, err)
	}
	if startedGoal == "" {
		t.Error("Start was not called")
	}
}

func TestIrreversibleGated(t *testing.T) {
	denied := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false },
		Start:   func(context.Context, string) error { t := false; _ = t; return nil },
	}
	started, _ := denied.Handle(context.Background(), Signal{Source: "ci", Goal: "git push origin main"})
	if started {
		t.Error("irreversible work must be gated; a denied gate must not start it")
	}

	var ran bool
	approved := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return true },
		Start:   func(context.Context, string) error { ran = true; return nil },
	}
	started, _ = approved.Handle(context.Background(), Signal{Goal: "deploy to staging"})
	if !started || !ran {
		t.Error("an approved gate should let irreversible work start")
	}
}

func TestDisabledDoesNothing(t *testing.T) {
	tr := &Trigger{Enabled: false, Start: func(context.Context, string) error { panic("must not start") }}
	if started, _ := tr.Handle(context.Background(), Signal{Goal: "anything"}); started {
		t.Error("disabled trigger must not start work")
	}
}
