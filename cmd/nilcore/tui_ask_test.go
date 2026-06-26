//go:build tui

package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"nilcore/internal/emit"
	"nilcore/internal/session"
)

// TestAskModalLineGrammar pins askModal.line() to the resolveReply grammar (indices
// only, never label text): a bare index for single-select, "i,j" (+ "; note") for
// multi, the raw text for free-form, and "" when nothing is picked (→ you-decide).
func TestAskModalLineGrammar(t *testing.T) {
	ch := []emit.AskChoice{{Label: "A"}, {Label: "B"}, {Label: "C"}}
	cases := []struct {
		name string
		a    askModal
		want string
	}{
		{"single cursor", askModal{choices: ch, cursor: 1}, "2"},
		{"single typed overrides", askModal{choices: ch, cursor: 1, text: "use mongo"}, "use mongo"},
		{"multi picks", askModal{choices: ch, multi: true, picked: []bool{true, false, true}}, "1,3"},
		{"multi picks + note", askModal{choices: ch, multi: true, picked: []bool{true, false, true}, text: "staging"}, "1,3 ; staging"},
		{"multi none picked", askModal{choices: ch, multi: true, picked: []bool{false, false, false}}, ""},
		{"multi note only", askModal{choices: ch, multi: true, picked: []bool{false, false, false}, text: "other"}, "other"},
		{"free-form", askModal{text: "do X"}, "do X"},
	}
	for _, c := range cases {
		if got := c.a.line(); got != c.want {
			t.Errorf("%s: line() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAskModalOpensAndDelivers: a structured KindAsk opens the modal (and renders the
// question + choices); cursor-down + enter selects a single-choice and closes it.
func TestAskModalOpensAndDelivers(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := readyTUI(t, sess)
	m = drive(t, m, emitMsg(emit.Event{Kind: emit.KindAsk, Text: "which db?", Ask: &emit.AskPrompt{
		Index: 1, Total: 1, Question: "Which database?",
		Choices: []emit.AskChoice{{Label: "Postgres", Detail: "managed"}, {Label: "SQLite"}},
	}}))
	if m.ask == nil {
		t.Fatal("a structured KindAsk should open the modal")
	}
	view := m.View()
	for _, want := range []string{"Which database?", "Postgres", "managed", "SQLite", "enter select"} {
		if !strings.Contains(view, want) {
			t.Fatalf("modal view missing %q in:\n%s", want, view)
		}
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.ask != nil {
		t.Fatal("enter on a single-select should close the modal (answer delivered)")
	}
}

// TestAskModalMultiToggleAndType: space toggles a multi-select pick; 't' focuses the
// free-text field; esc on a choice modal declines (you-decide) and closes.
func TestAskModalMultiToggleAndType(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := readyTUI(t, sess)
	open := func() tuiModel {
		return drive(t, readyTUI(t, sess), emitMsg(emit.Event{Kind: emit.KindAsk, Ask: &emit.AskPrompt{
			Index: 1, Total: 1, Question: "pick", MultiSelect: true,
			Choices: []emit.AskChoice{{Label: "A"}, {Label: "B"}},
		}}))
	}
	m = open()
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")}) // space toggles choice 1
	if m.ask == nil || !m.ask.picked[0] {
		t.Fatal("space should toggle the cursor's choice")
	}
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}) // focus free text
	if m.ask == nil || !m.ask.typing {
		t.Fatal("'t' should focus the free-text field")
	}

	// esc on a fresh choice modal declines and closes.
	m = open()
	m = drive(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.ask != nil {
		t.Fatal("esc should decline (you-decide) and close the modal")
	}
}

// TestGateTakesPriorityOverAsk: when both a gate and an ask are somehow active, the
// gate captures keys and renders first (the serialization guarantee).
func TestGateTakesPriorityOverAsk(t *testing.T) {
	sess := session.New("t", "local", t.TempDir(), nil)
	m := readyTUI(t, sess)
	m = drive(t, m, emitMsg(emit.Event{Kind: emit.KindAsk, Ask: &emit.AskPrompt{Index: 1, Total: 1, Question: "Q"}}))
	g := gateReq{action: "push to main", reply: make(chan bool, 1)}
	m = drive(t, m, gateMsg(g))
	if m.gate == nil || m.ask == nil {
		t.Fatal("both modals should be active for this test")
	}
	if !strings.Contains(m.View(), "GATE") {
		t.Fatalf("gate must render with priority over the ask, got:\n%s", m.View())
	}
}
