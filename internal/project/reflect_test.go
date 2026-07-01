package project

import (
	"context"
	"testing"

	"nilcore/internal/advisor"
)

// The SWITCH rung must persist the advisor's proposed approach into the SAME State
// the outer loop threads to the next Plan. reflect takes *State precisely so the
// appended "switch approach" decision is not written to a discarded copy — the bug
// this guards against silently degraded SWITCH into a plain NARROW.
func TestReflect_LadderRungs(t *testing.T) {
	const proposed = "rewrite the parser as a table-driven state machine"

	tests := []struct {
		name         string
		rung         int
		advisor      *advisor.Advisor
		channelYes   bool // only consulted on the STOP rung (rung >= 2)
		wantAction   ladderAction
		wantDecision string // "" ⇒ no new decision folded into st.Summary.Decisions
	}{
		{
			name:       "rung 0 narrows and leaves state untouched",
			rung:       0,
			advisor:    advisor.New(replyModel{reply: proposed}, 0),
			wantAction: ladderNarrow,
		},
		{
			name:         "rung 1 switches and folds the advisor approach into state",
			rung:         1,
			advisor:      advisor.New(replyModel{reply: proposed}, 0),
			wantAction:   ladderSwitch,
			wantDecision: "switch approach: " + proposed,
		},
		{
			name:       "rung 1 with a nil advisor still switches but folds nothing",
			rung:       1,
			advisor:    nil,
			wantAction: ladderSwitch,
		},
		{
			name:       "rung 2 stop-and-ask with no channel defaults to stop",
			rung:       2,
			advisor:    advisor.New(replyModel{reply: proposed}, 0),
			wantAction: ladderStop,
		},
		{
			name:       "rung 2 stop-and-ask honored 'keep going' drops back to switch",
			rung:       2,
			advisor:    advisor.New(replyModel{reply: proposed}, 0),
			channelYes: true,
			wantAction: ladderSwitch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &Loop{Goal: "g", Log: tmpLog(t), Advisor: tt.advisor}
			if tt.rung >= 2 {
				l.Channel = &askChannel{yes: tt.channelYes}
			}

			st := State{Goal: "g", Iteration: 3}
			act := l.reflect(context.Background(), &st, "slice failed: boom", tt.rung)

			if act != tt.wantAction {
				t.Fatalf("action = %v, want %v", act, tt.wantAction)
			}
			if got := len(st.Summary.Decisions); (got > 0) != (tt.wantDecision != "") {
				t.Fatalf("decisions = %v, want folded=%v", st.Summary.Decisions, tt.wantDecision != "")
			}
			if tt.wantDecision != "" && st.Summary.Decisions[0] != tt.wantDecision {
				t.Fatalf("decision = %q, want %q", st.Summary.Decisions[0], tt.wantDecision)
			}
		})
	}
}

// End-to-end proof of the fix: the State a rung-1 reflect mutates is the SAME value
// the next Plan receives. We thread st through reflect (by pointer, as the loop does)
// and then hand it to a captured Plan seam; the planner must see the switch decision.
func TestReflect_SwitchReachesNextPlan(t *testing.T) {
	const proposed = "swap the greedy pass for a two-phase merge"

	l := &Loop{Goal: "g", Log: tmpLog(t), Advisor: advisor.New(replyModel{reply: proposed}, 0)}

	var planSaw []string
	l.Plan = func(_ context.Context, _ string, st State) (Slice, error) {
		planSaw = append([]string(nil), st.Summary.Decisions...)
		return Slice{}, nil
	}

	st := State{Goal: "g"}
	if act := l.reflect(context.Background(), &st, "planning failed: boom", 1); act != ladderSwitch {
		t.Fatalf("action = %v, want switch", act)
	}
	// The loop threads the SAME st (not a copy) into the next Plan.
	if _, err := l.Plan(context.Background(), st.Goal, st); err != nil {
		t.Fatalf("plan: %v", err)
	}

	want := "switch approach: " + proposed
	if len(planSaw) != 1 || planSaw[0] != want {
		t.Fatalf("Plan saw decisions %v, want [%q]", planSaw, want)
	}
}
