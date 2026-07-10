package termui

import (
	"bytes"
	"io"
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

// Blue joins the existing palette: it wraps with the right ANSI code when styling is
// on, and passes through unchanged when off (the I6 non-TTY discipline).
func TestBlueStyle(t *testing.T) {
	on := Style{on: true}
	if got := on.Blue("x"); got != "\033[34mx\033[0m" {
		t.Errorf("Blue on = %q", got)
	}
	off := Style{on: false}
	if off.Blue("x") != "x" {
		t.Error("colors must pass through unchanged when styling is off")
	}
}

// The gauge degrades to plain "context NN%" with no escapes off a terminal (I6),
// clamps out-of-range input, and buckets the ring on a styled console.
func TestGauge(t *testing.T) {
	var buf bytes.Buffer
	plain := New(&buf) // a buffer is not a TTY
	for in, want := range map[int]string{0: "context 0%", 42: "context 42%", 100: "context 100%", -5: "context 0%", 150: "context 100%"} {
		if got := plain.Gauge(in); got != want {
			t.Errorf("plain Gauge(%d) = %q, want %q", in, got, want)
		}
		if strings.Contains(plain.Gauge(in), "\033[") {
			t.Errorf("plain gauge must contain no ANSI escapes: %q", plain.Gauge(in))
		}
	}
	// On a styled console the ring bucket reflects the band.
	styled := &Console{w: &buf, st: Style{on: true}}
	for in, ring := range map[int]string{10: "○", 30: "◔", 55: "◑", 80: "◕", 100: "●"} {
		if got := styled.Gauge(in); !strings.Contains(got, ring) {
			t.Errorf("styled Gauge(%d) = %q, want ring %q", in, got, ring)
		}
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

// Regression for a reproduced deadlock between stopSpinLocked and animate.
// stopSpinLocked closes s.stop and then joins the animator (<-s.done) while
// holding c.mu; animate's tick branch used to acquire c.mu with a blocking Lock.
// Because select picks uniformly among ready cases, a tick that fired in the same
// instant stop closed could win the race, block on Lock against the stopper's
// held mutex, and never reach the point where it observes stop and exits — so the
// stopper blocked on the join forever, permanently wedging the console mutex. A
// standalone repro deadlocked at ~iteration 665.
//
// This drives every stop path (StopSpin, Token, Prompt, and Spin-over-Spin) many
// times with a very fast tick to maximise the odds a tick is buffered exactly
// when stop closes — the wedge window. Under the fix (TryLock in the tick branch)
// the animator never blocks on the mutex, so every cycle completes; the watchdog
// fails the test only if a cycle wedges. Run under -race.
func TestSpinnerStopNeverDeadlocks(t *testing.T) {
	const cycles = 3000
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A styled console (so the animator actually runs) over a discard sink,
		// ticking far faster than the wedge window is wide. spinEvery is set here
		// at construction and never mutated, so the animator reads it race-free.
		c := &Console{w: io.Discard, st: Style{on: true}, spinEvery: 20 * time.Microsecond}
		for i := 0; i < cycles; i++ {
			c.Spin("work", uint64(i), verb.Chat, nil)
			switch i % 4 {
			case 0:
				c.StopSpin() // StopSpin → stopSpinLocked
			case 1:
				c.Token("x") // Token stops the spinner, then opens a stream
			case 2:
				c.Prompt("> ") // Prompt stops the spinner
			case 3:
				c.Spin("again", uint64(i), verb.Native, nil) // Spin over a live Spin
				c.StopSpin()
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("spinner stop deadlocked: animator wedged on c.mu while stopSpinLocked joined it")
	}
}

func TestHumanHelpers(t *testing.T) {
	if humanDuration(8*time.Second) != "8s" || humanDuration(75*time.Second) != "1m15s" {
		t.Error("humanDuration")
	}
	if HumanTokens(842) != "842" || HumanTokens(3100) != "3.1k" {
		t.Error("HumanTokens")
	}
}
