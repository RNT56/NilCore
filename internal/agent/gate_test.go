package agent_test

import (
	"testing"

	"nilcore/internal/agent"
)

type stubApprover struct {
	ok    bool
	asked bool
}

func (s *stubApprover) Approve(string) bool {
	s.asked = true
	return s.ok
}

func TestOrchestratorGate(t *testing.T) {
	cases := []struct {
		name      string
		action    string
		approver  *stubApprover
		want      bool
		wantAsked bool
	}{
		{"reversible auto-proceeds", "go test ./...", &stubApprover{}, true, false},
		{"irreversible approved", "git push origin main", &stubApprover{ok: true}, true, true},
		{"irreversible denied", "kubectl apply -f deploy.yaml", &stubApprover{ok: false}, false, true},
	}
	for _, c := range cases {
		o := &agent.Orchestrator{Approver: c.approver}
		if got := o.Gate(c.action); got != c.want {
			t.Errorf("%s: Gate(%q) = %v, want %v", c.name, c.action, got, c.want)
		}
		if c.approver.asked != c.wantAsked {
			t.Errorf("%s: approver asked = %v, want %v", c.name, c.approver.asked, c.wantAsked)
		}
	}

	// With no approver wired, irreversible is denied and reversible still runs.
	o := &agent.Orchestrator{}
	if o.Gate("git push origin main") {
		t.Error("nil approver must deny an irreversible action")
	}
	if !o.Gate("ls -la") {
		t.Error("a reversible action must auto-proceed even with no approver")
	}
}
