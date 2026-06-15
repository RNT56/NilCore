//go:build tui

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"nilcore/internal/emit"
	"nilcore/internal/session"
)

// drive the model through Update and return the updated model (the Bubble Tea
// value-model pattern), failing if the type assertion ever breaks.
func drive(t *testing.T, m tuiModel, msg tea.Msg) tuiModel {
	t.Helper()
	out, _ := m.Update(msg)
	tm, ok := out.(tuiModel)
	if !ok {
		t.Fatalf("Update returned %T, want tuiModel", out)
	}
	return tm
}

// TestTUIFoldsEvents proves the emit→transcript folding: streamed tokens accumulate
// into one line, and a framed event commits that line and appends its own.
func TestTUIFoldsEvents(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := newTUIModel(context.Background(), sess, "anthropic:claude-x", newTUIEmitter(), make(chan gateReq))

	m = drive(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if !m.ready {
		t.Fatal("model not ready after a window size")
	}

	m = drive(t, m, emitMsg(emit.Event{Kind: emit.KindToken, Text: "Hello "}))
	m = drive(t, m, emitMsg(emit.Event{Kind: emit.KindToken, Text: "world"}))
	m = drive(t, m, emitMsg(emit.Event{Kind: emit.KindTool, Text: "about to run: go test"}))

	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "Hello world") {
		t.Errorf("streamed reasoning not committed:\n%s", joined)
	}
	if !strings.Contains(joined, "about to run: go test") {
		t.Errorf("tool line missing:\n%s", joined)
	}
	if m.View() == "" {
		t.Error("View rendered empty")
	}
}

// TestTUIGateModal proves an approval gate shows as a modal and routes the answer
// back to the blocked Approve call.
func TestTUIGateModal(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := newTUIModel(context.Background(), sess, "m", newTUIEmitter(), make(chan gateReq))
	m = drive(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	reply := make(chan bool, 1)
	m = drive(t, m, gateMsg(gateReq{action: "git push origin main", reply: reply}))
	if m.gate == nil {
		t.Fatal("gate modal not shown")
	}
	if !strings.Contains(m.View(), "git push origin main") {
		t.Error("gate action not rendered in the modal")
	}

	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if m.gate != nil {
		t.Error("gate not cleared after an answer")
	}
	select {
	case ans := <-reply:
		if !ans {
			t.Error("'y' must approve")
		}
	default:
		t.Error("no reply was sent to the blocked Approve call")
	}
}

// tuiEmitter never blocks the loop, even far past the buffer, and coalescing under
// backpressure drops only TOKENS — never a framed event (which would lose a turn
// boundary and merge two turns in the transcript).
func TestTUIEmitterKeepsFramesNonBlocking(t *testing.T) {
	e := newTUIEmitter()
	e.Emit(emit.Event{Kind: emit.KindTool, Text: "frame-A"}) // a frame, first
	for i := 0; i < tuiEmitBuffer*3; i++ {                   // flood far past the buffer — must not block
		e.Emit(emit.Event{Kind: emit.KindToken, Text: "x"})
	}
	e.Emit(emit.Event{Kind: emit.KindSteerAck, Text: "frame-B"}) // a frame, last

	got := e.drain()
	if len(got) > tuiEmitBuffer+1 {
		t.Errorf("queue not bounded: %d events", len(got))
	}
	var frames []string
	for _, ev := range got {
		if ev.Kind != emit.KindToken {
			frames = append(frames, ev.Text)
		}
	}
	if len(frames) != 2 || frames[0] != "frame-A" || frames[1] != "frame-B" {
		t.Errorf("frames must survive coalescing in order, got %v", frames)
	}
}

// TestTUIBatchFoldsAndSplitsTurns proves a drained batch folds in order AND that a
// token carrying a new step commits the prior turn even with no framed boundary
// (the defensive split that stops two turns merging into one transcript line).
func TestTUIBatchFoldsAndSplitsTurns(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := newTUIModel(context.Background(), sess, "m", newTUIEmitter(), make(chan gateReq))
	m = drive(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m = drive(t, m, emitBatchMsg([]emit.Event{
		{Kind: emit.KindToken, Step: 1, Text: "alpha"},
		{Kind: emit.KindToken, Step: 2, Text: "beta"}, // new step, no frame between
	}))
	m = drive(t, m, emitMsg(emit.Event{Kind: emit.KindTool, Step: 2, Text: "go test"}))

	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") {
		t.Errorf("both turns must be present:\n%s", joined)
	}
	// They must be on SEPARATE committed lines, not merged into "alphabeta".
	if strings.Contains(joined, "alphabeta") {
		t.Errorf("turns merged into one line:\n%s", joined)
	}
}

// TestTUIApproverUnblocksOnCancel proves a quit/shutdown can never wedge the drive
// goroutine in a pending gate: a cancelled ctx releases Approve with a default-deny,
// even though no UI receiver ever takes the gateReq. Without the ctx select this
// hangs forever (and hangs sess.Wait() at shutdown).
func TestTUIApproverUnblocksOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ap := &tuiApprover{ctx: ctx, gates: make(chan gateReq)} // unbuffered, no receiver

	done := make(chan bool, 1)
	go func() { done <- ap.Approve("git push origin main") }()

	cancel() // tear down with the gate still pending
	select {
	case got := <-done:
		if got {
			t.Error("a cancelled gate must default-deny")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not return after ctx cancel — the shutdown hang is not fixed")
	}
}
