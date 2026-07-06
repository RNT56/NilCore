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
