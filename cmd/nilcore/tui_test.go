//go:build tui

package main

import (
	"context"
	"os"
	"path/filepath"
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

// typeLine simulates the user typing a line and pressing Enter — the path every
// control verb and message flows through (onKey → submit).
func typeLine(t *testing.T, m tuiModel, line string) tuiModel {
	t.Helper()
	m.ta.SetValue(line)
	return drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

// readyTUI builds a sized, ready TUI model over a real (log-free) Session.
func readyTUI(t *testing.T, sess *session.Session) tuiModel {
	t.Helper()
	m := newTUIModel(context.Background(), sess, "m", newTUIEmitter(), make(chan gateReq))
	return drive(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
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

// TestTUIModeVerbs proves the TUI routes the shared mode verbs through
// session.ParseControl onto the SAME Session — so /ask (the /discuss alias), /plan,
// /execute, and /auto pin the mode in the TUI exactly as in the REPL.
func TestTUIModeVerbs(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := readyTUI(t, sess)

	for _, tc := range []struct {
		line string
		want session.Mode
	}{
		{"/ask", session.ModeDiscuss},
		{"/discuss", session.ModeDiscuss},
		{"/plan", session.ModePlan},
		{"/execute", session.ModeExecute},
		{"/auto", session.ModeAuto},
	} {
		m = typeLine(t, m, tc.line)
		if got := sess.CurrentMode(); got != tc.want {
			t.Errorf("%s ⇒ mode %v, want %v", tc.line, got, tc.want)
		}
	}
}

// TestTUISaveVerb proves /save works in the TUI (a local front door): nothing to
// save ⇒ no file; after an answer exists ⇒ the last answer is persisted verbatim
// (the same writeLastAnswer path as the REPL).
func TestTUISaveVerb(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(t.TempDir()) // cwd deliberately differs from the session repo
	sess := session.New("t", "local", repo, nil)
	m := readyTUI(t, sess)

	m = typeLine(t, m, "/save NOTES.md")
	if _, err := os.Stat(filepath.Join(repo, "NOTES.md")); err == nil {
		t.Fatal("/save wrote a file with nothing to save")
	}

	sess.State.LastOutcome = "# Plan\n\n- step one"
	m = typeLine(t, m, "/save NOTES.md")
	// /save resolves against the session repo (RepoDir), NOT the process cwd.
	got, err := os.ReadFile(filepath.Join(repo, "NOTES.md"))
	if err != nil {
		t.Fatalf("read saved file from repo: %v", err)
	}
	if string(got) != "# Plan\n\n- step one\n" {
		t.Errorf("saved content = %q", got)
	}
	if !strings.Contains(strings.Join(m.lines, "\n"), "saved the last answer") {
		t.Errorf("no save confirmation in the transcript:\n%s", strings.Join(m.lines, "\n"))
	}
}

// TestTUIMiscVerbs covers the remaining shared verbs and the unknown-command guard:
// /add registers a read-only root, /mode reports, /clear resets, and a bogus slash
// warns instead of being routed to the model as a turn.
func TestTUIMiscVerbs(t *testing.T) {
	dir := t.TempDir()
	sess := session.New("t", "local", dir, nil)
	m := readyTUI(t, sess)

	m = typeLine(t, m, "/add "+dir)
	if len(sess.ReadRootsNow()) != 1 {
		t.Errorf("/add did not register a read-only root: %v", sess.ReadRootsNow())
	}

	m = typeLine(t, m, "/mode")
	m = typeLine(t, m, "/bogus")
	m = typeLine(t, m, "/clear")
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "mode:") {
		t.Error("/mode did not report the current mode")
	}
	if !strings.Contains(joined, "unknown command") {
		t.Error("/bogus did not warn (it must not be routed to the model)")
	}
	if !strings.Contains(joined, "context cleared") {
		t.Error("/clear did not confirm")
	}
}
