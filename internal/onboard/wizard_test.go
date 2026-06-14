package onboard

import (
	"strings"
	"testing"
)

// TestEchoOffNoopOnNonTTY proves the secret echo-off (audit L8) degrades to a
// safe no-op when input is not a terminal (a pipe or test reader), so
// non-interactive provisioning is never broken by it.
func TestEchoOffNoopOnNonTTY(t *testing.T) {
	restore := echoOff(strings.NewReader("piped"))
	if restore == nil {
		t.Fatal("echoOff must always return a callable restore func")
	}
	restore() // must not panic on a non-terminal reader
}
