//go:build tui

package main

import (
	"context"
	"strings"
	"testing"

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
	m := newTUIModel(context.Background(), sess, "anthropic:claude-x", make(chan emit.Event, 8), make(chan gateReq))

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
	m := newTUIModel(context.Background(), sess, "m", make(chan emit.Event, 1), make(chan gateReq))
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

// tuiEmitter never blocks the loop, even with a full buffer (drop-oldest).
func TestTUIEmitterNonBlocking(t *testing.T) {
	e := &tuiEmitter{events: make(chan emit.Event, 2)}
	for i := 0; i < 10; i++ { // far past the buffer — must not block
		e.Emit(emit.Event{Kind: emit.KindToken, Text: "x"})
	}
}
