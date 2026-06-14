package policy

import (
	"io"
	"strings"
	"testing"
)

type approveAll struct{}

func (approveAll) Approve(string) bool { return true }

type denyAll struct{}

func (denyAll) Approve(string) bool { return false }

func TestGate(t *testing.T) {
	cases := []struct {
		name   string
		action string
		ask    Approver
		want   bool
	}{
		{"reversible auto-proceeds (nil approver)", "go test ./...", nil, true},
		{"reversible auto-proceeds (deny approver, never asked)", "edit main.go", denyAll{}, true},
		{"irreversible without approver is denied", "git push origin main", nil, false},
		{"irreversible approved", "git push origin main", approveAll{}, true},
		{"irreversible denied", "kubectl apply -f deploy.yaml", denyAll{}, false},
	}
	for _, c := range cases {
		if got := Gate(c.action, c.ask); got != c.want {
			t.Errorf("%s: Gate(%q) = %v, want %v", c.name, c.action, got, c.want)
		}
	}
}

func TestConsoleApprover(t *testing.T) {
	cases := map[string]bool{
		"y\n":     true,
		"yes\n":   true,
		"Y\n":     true,
		"n\n":     false,
		"\n":      false,
		"nope\n":  false,
		"":        false, // EOF, no input
		" yes \n": true,
	}
	for input, want := range cases {
		a := NewConsoleApprover(strings.NewReader(input), io.Discard)
		if got := a.Approve("git push origin main"); got != want {
			t.Errorf("Approve(input=%q) = %v, want %v", input, got, want)
		}
	}
}
