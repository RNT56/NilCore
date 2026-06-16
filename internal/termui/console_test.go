package termui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"nilcore/internal/verb"
)

// A bytes.Buffer is not a terminal, so the console must render plain content with
// no ANSI escapes — the SSH / piped / CI degradation guarantee.
func TestPlainConsoleNoEscapes(t *testing.T) {
	var buf bytes.Buffer
	c := New(&buf)
	if c.Styled() {
		t.Fatal("a buffer must not be detected as a styled terminal")
	}
	c.Spin("", 1, verb.Chat, nil) // no label → nothing printed on a non-TTY
	c.Line("hello")
	c.StopSpin()
	got := buf.String()
	if strings.Contains(got, "\033[") || strings.Contains(got, "\r") {
		t.Errorf("plain output must contain no ANSI/CR escapes: %q", got)
	}
	if got != "hello\n" {
		t.Errorf("got %q, want \"hello\\n\"", got)
	}
}

// Blue/Magenta join the existing palette: they wrap with the right ANSI code when
// styling is on, and pass through unchanged when off (the I6 non-TTY discipline).
func TestBlueMagentaStyle(t *testing.T) {
	on := Style{on: true}
	if got := on.Blue("x"); got != "\033[34mx\033[0m" {
		t.Errorf("Blue on = %q", got)
	}
	if got := on.Magenta("x"); got != "\033[35mx\033[0m" {
		t.Errorf("Magenta on = %q", got)
	}
	off := Style{on: false}
	if off.Blue("x") != "x" || off.Magenta("x") != "x" {
		t.Error("colors must pass through unchanged when styling is off")
	}
}

// Streamed tokens flow inline; a following finalized line closes the stream with
// a newline so it never runs onto the token text.
func TestStreamThenLine(t *testing.T) {
	var buf bytes.Buffer
	c := New(&buf)
	c.Token("Hel")
	c.Token("lo")
	c.Line("done")
	if buf.String() != "Hello\ndone\n" {
		t.Errorf("got %q, want \"Hello\\ndone\\n\"", buf.String())
	}
}

// A labelled Spin on a non-TTY announces the activity once (no animation).
func TestSpinLabelPlain(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Spin("running go test", 1, verb.Native, nil)
	if buf.String() != "running go test\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestStyleOnOff(t *testing.T) {
	off := Style{on: false}
	if off.Bold("x") != "x" || off.Success("x") != "x" {
		t.Error("styling off must return the input unchanged")
	}
	on := Style{on: true}
	if !strings.Contains(on.Bold("x"), "\033[1m") || !strings.Contains(on.Success("x"), "\033[32m") {
		t.Errorf("styling on must wrap with ANSI: %q / %q", on.Bold("x"), on.Success("x"))
	}
	if on.Bold("") != "" {
		t.Error("empty string must never be wrapped")
	}
}

// renderSpinLocked builds the live line shape: a braille frame, a verb (or the
// label), the steer hint, and a token count when one is supplied.
func TestRenderSpinShape(t *testing.T) {
	c := &Console{st: Style{on: true}}

	c.spin = &spinState{start: time.Now().Add(-8 * time.Second), sp: verb.New(1, verb.Chat), styled: true}
	line := c.renderSpinLocked()
	if !strings.Contains(line, "! to steer") || !strings.Contains(line, "…") {
		t.Errorf("verb spinner line malformed: %q", line)
	}

	c.spin = &spinState{label: "go test ./...", start: time.Now(), sp: verb.New(1, verb.Native),
		tokens: func() int { return 3100 }, styled: true}
	line = c.renderSpinLocked()
	if !strings.Contains(line, "go test ./...") {
		t.Errorf("label not used in place of the verb: %q", line)
	}
	if !strings.Contains(line, "3.1k tok") {
		t.Errorf("token count not rendered: %q", line)
	}
}

func TestHumanHelpers(t *testing.T) {
	if humanDuration(8*time.Second) != "8s" || humanDuration(75*time.Second) != "1m15s" {
		t.Error("humanDuration")
	}
	if humanTokens(842) != "842" || humanTokens(3100) != "3.1k" {
		t.Error("humanTokens")
	}
}
