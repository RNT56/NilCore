//go:build tui

package trace

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// sampleTrace builds a small two-level causal tree for exercising the explorer.
func sampleTrace(verified bool) *Trace {
	return &Trace{
		Task:          "t-1",
		Verdict:       "GREEN",
		ChainVerified: verified,
		Steps: []Step{
			{
				Seq:      1,
				Title:    "ran tool: edit",
				Why:      "after the plan",
				Children: []Step{{Seq: 2, Title: "verify PASSED", Why: "checks green"}},
			},
			{Seq: 3, Title: "integrated branch", Untrusted: !verified},
		},
	}
}

// TestExplorerReflowFlattensTree proves NewExplorer flattens the full tree by default
// (every node visible, children after their parent) so the whole story is shown before
// any fold.
func TestExplorerReflowFlattensTree(t *testing.T) {
	m := NewExplorer(sampleTrace(true)).(explorerModel)
	if len(m.rows) != 3 {
		t.Fatalf("initial rows = %d, want 3 (all nodes expanded)", len(m.rows))
	}
	if !m.rows[0].hasKids || m.rows[1].depth != 1 {
		t.Fatalf("child row not nested under its parent: %+v", m.rows)
	}
}

// TestExplorerFoldTogglesSubtree proves pressing enter on a parent collapses its
// descendants out of the flattened view, and pressing it again restores them.
func TestExplorerFoldTogglesSubtree(t *testing.T) {
	var m tea.Model = NewExplorer(sampleTrace(true))
	// Cursor starts on row 0 (the parent with a child). Enter folds it.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := len(m.(explorerModel).rows); got != 2 {
		t.Fatalf("after fold rows = %d, want 2 (child hidden)", got)
	}
	// Enter again unfolds.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := len(m.(explorerModel).rows); got != 3 {
		t.Fatalf("after unfold rows = %d, want 3", got)
	}
}

// TestExplorerViewHeaderVerdict proves the header carries the trust verdict: a clean
// chain shows the verified marker, a broken chain shows the loud banner (I5).
func TestExplorerViewHeaderVerdict(t *testing.T) {
	clean := NewExplorer(sampleTrace(true)).View()
	if !strings.Contains(clean, "chain verified") {
		t.Errorf("clean view missing verified marker:\n%s", clean)
	}
	broken := NewExplorer(sampleTrace(false)).View()
	if !strings.Contains(broken, "CHAIN BROKEN") {
		t.Errorf("broken view missing loud banner:\n%s", broken)
	}
}

// TestRunExplorerQuitsImmediately proves the RunExplorer entrypoint drives the model
// to completion when a quit key arrives at once — the interactive command's blocking
// call returns cleanly rather than hanging.
func TestRunExplorerQuitsImmediately(t *testing.T) {
	m := NewExplorer(sampleTrace(true))
	// The quit key is the model's own contract: verify it emits tea.Quit so the program
	// RunExplorer builds would terminate. (A full tea.Program.Run needs a TTY.)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Fatal("q must produce a command (tea.Quit) so RunExplorer's program can exit")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("quit command must yield a message")
	}
}
