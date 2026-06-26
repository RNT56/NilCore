// Package termui is the conversational front door's terminal renderer: a single
// "live line" at the bottom (an animated spinner, or the model's streaming text)
// with finalized lines scrolling above it — the "log above a live status line"
// idiom, done with plain ANSI and no raw mode, so it works over SSH and degrades
// to clean plain lines when stdout is not a terminal.
//
// Everything is gated on an isatty + NO_COLOR + TERM≠dumb check: on a real
// terminal you get colour, a braille spinner, and in-place redraws; off one
// (a pipe, a CI log, a dumb terminal) you get the same content as plain,
// newline-terminated lines with no escape sequences. Stdlib only (invariant I6).
package termui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"nilcore/internal/verb"
)

// Console renders the chat surface to w. It is safe for concurrent use: the agent
// goroutine emits lines/tokens while the spinner ticker redraws the live line.
type Console struct {
	mu        sync.Mutex
	w         io.Writer
	st        Style
	streaming bool // tokens are flowing inline at the bottom (no live spinner)
	spin      *spinState
}

// New returns a Console rendering to w, auto-detecting whether w is a styled
// terminal (colour + animation) or a plain sink (newline lines, no escapes).
func New(w io.Writer) *Console {
	return &Console{w: w, st: detectStyle(w)}
}

// Styled reports whether colour/animation is on (a real terminal that opted in).
func (c *Console) Styled() bool { return c.st.on }

// Style exposes the console's styling helper so callers can colour their own
// content consistently (glyphs, prompts).
func (c *Console) Style() Style { return c.st }

// Line writes a finalized line into the scrollback, above the live region. If
// tokens were streaming it first closes their line with a newline; if a spinner
// is live it is cleared, the line printed, then the spinner redrawn below.
func (c *Console) Line(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeStreamLocked()
	live := c.spin != nil
	if live {
		c.clearLiveLocked()
	}
	_, _ = io.WriteString(c.w, s+"\n")
	if live {
		c.drawLiveLocked()
	}
}

// Token writes one streamed token inline at the bottom. The first token of a run
// stops any live spinner and opens the stream; subsequent tokens flow as raw
// text. On a non-TTY this is just the text (the stream reads as one growing line).
func (c *Console) Token(s string) {
	if s == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spin != nil {
		c.stopSpinLocked()
	}
	c.streaming = true
	_, _ = io.WriteString(c.w, s)
}

// Spin starts (or relabels) the animated live line: a braille frame + a cycling
// verb + elapsed + an optional running token count + the steer hint. tokens may
// be nil. On a non-TTY it prints the label once and does not animate.
func (c *Console) Spin(label string, seed uint64, cat verb.Category, tokens func() int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeStreamLocked()
	c.stopSpinLocked()
	c.spin = &spinState{
		label:  label,
		start:  time.Now(),
		sp:     verb.New(seed, cat),
		tokens: tokens,
		styled: c.st.on,
	}
	if !c.st.on {
		// Non-TTY: announce the activity once, no ticker, no redraws.
		if label != "" {
			_, _ = io.WriteString(c.w, label+"\n")
		}
		return
	}
	c.drawLiveLocked()
	c.spin.stop = make(chan struct{})
	c.spin.done = make(chan struct{})
	go c.animate(c.spin)
}

// StopSpin clears the live spinner line, if any.
func (c *Console) StopSpin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopSpinLocked()
}

// Prompt writes the input prompt at the bottom (no trailing newline). Any live
// region is closed first so the prompt sits cleanly on its own line.
func (c *Console) Prompt(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeStreamLocked()
	c.stopSpinLocked()
	_, _ = io.WriteString(c.w, s)
}

// Gauge renders a context-usage indicator for pct (0–100, clamped). On a styled
// terminal it is a clockwise-filling ring (◔◑◕● at the 25/50/75/100 buckets, ○ for
// near-empty) tinted by pressure (green <60, amber 60–85, red >85) followed by the
// percentage. Off a terminal it degrades to a plain "context NN%" with no escapes
// (the I6 SSH/CI/pipe guarantee). The ring is deliberately bucketed, not smooth —
// it is a pressure signal ("time to /clear?"), not a precise meter.
func (c *Console) Gauge(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	if !c.st.on {
		return fmt.Sprintf("context %d%%", pct)
	}
	var ring string
	switch {
	case pct >= 100:
		ring = "●"
	case pct >= 75:
		ring = "◕"
	case pct >= 50:
		ring = "◑"
	case pct >= 25:
		ring = "◔"
	default:
		ring = "○"
	}
	paint := c.st.Success
	switch {
	case pct > 85:
		paint = c.st.Danger
	case pct >= 60:
		paint = c.st.Warn
	}
	return paint(fmt.Sprintf("%s %d%%", ring, pct))
}

// --- internals (all callers hold c.mu) ---

type spinState struct {
	label  string
	start  time.Time
	sp     verb.Spinner
	tokens func() int
	styled bool
	stop   chan struct{}
	done   chan struct{}
}

// animate redraws the live line on a ticker until stopped. It takes the console
// mutex for each redraw so it never interleaves with Line/Token writes.
func (c *Console) animate(s *spinState) {
	defer close(s.done)
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			c.mu.Lock()
			if c.spin == s { // still the active spinner
				c.drawLiveLocked()
			}
			c.mu.Unlock()
		}
	}
}

func (c *Console) drawLiveLocked() {
	if c.spin == nil || !c.st.on {
		return
	}
	_, _ = io.WriteString(c.w, "\r\033[K"+c.renderSpinLocked())
}

func (c *Console) clearLiveLocked() {
	if c.st.on {
		_, _ = io.WriteString(c.w, "\r\033[K")
	}
}

func (c *Console) stopSpinLocked() {
	if c.spin == nil {
		return
	}
	s := c.spin
	c.spin = nil
	if s.stop != nil {
		close(s.stop)
		<-s.done // join the ticker so no redraw races a later write
	}
	c.clearLiveLocked()
}

func (c *Console) closeStreamLocked() {
	if c.streaming {
		_, _ = io.WriteString(c.w, "\n")
		c.streaming = false
	}
}

// renderSpinLocked builds the live line: "⠹ Cogitating…  12s · 3.1k tok · ! to steer".
// A label (e.g. a running tool) replaces the verb.
func (c *Console) renderSpinLocked() string {
	s := c.spin
	elapsed := time.Since(s.start)
	head := s.label
	if head == "" {
		head = s.sp.Verb(elapsed) + "…"
	}
	var meta []string
	meta = append(meta, humanDuration(elapsed))
	if s.tokens != nil {
		if n := s.tokens(); n > 0 {
			meta = append(meta, HumanTokens(n)+" tok")
		}
	}
	frame := c.st.Warn(s.sp.Frame(elapsed))
	line := frame + " " + head + "  " + c.st.Dim(strings.Join(meta, " · ")+" · ") + c.st.Warn("! to steer")
	return line
}

func humanDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// HumanTokens renders a token count compactly: the raw integer below 1000, else a
// one-decimal "k" (e.g. 1500 → "1.5k"). Exported so both front doors — the REPL live
// line and the TUI activity line — render the estimate identically.
func HumanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// Style applies ANSI styling only on a terminal that opted in (honouring NO_COLOR
// and TERM=dumb). Off a terminal every method returns its input unchanged, so
// piped output stays clean. Stdlib only — plain escape constants.
type Style struct{ on bool }

func detectStyle(w io.Writer) Style {
	f, ok := w.(*os.File)
	if !ok {
		return Style{}
	}
	fi, err := f.Stat()
	on := err == nil && fi.Mode()&os.ModeCharDevice != 0 &&
		os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	return Style{on: on}
}

func (s Style) wrap(code, t string) string {
	if !s.on || t == "" {
		return t
	}
	return "\033[" + code + "m" + t + "\033[0m"
}

func (s Style) Bold(t string) string    { return s.wrap("1", t) }
func (s Style) Dim(t string) string     { return s.wrap("2", t) }
func (s Style) Info(t string) string    { return s.wrap("36", t) } // cyan
func (s Style) Success(t string) string { return s.wrap("32", t) } // green
func (s Style) Danger(t string) string  { return s.wrap("31", t) } // red
func (s Style) Warn(t string) string    { return s.wrap("33", t) } // amber
func (s Style) Blue(t string) string    { return s.wrap("34", t) } // blue
func (s Style) Magenta(t string) string { return s.wrap("35", t) } // magenta
