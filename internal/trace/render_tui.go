//go:build tui

// render_tui.go is the interactive collapsible explorer over a Trace — the same
// causal tree Render prints, but navigable: arrow keys move the cursor, enter
// folds/unfolds a subtree, and the header carries the trust verdict. It is the
// ONLY file in this package that imports the Charm stack, behind the `tui` build
// tag, so the default `nilcore` binary links ZERO Charm (invariant I6 — same
// isolation the cmd/nilcore TUI uses).
//
// It is a pure VIEW over the already-built Trace: it never re-reads the log, never
// mutates anything, and surfaces only the harness-derived, metadata-only fields
// the builder already vetted (I7). On a broken chain every row is flagged
// untrusted and the header is loud-red, exactly as the plain renderer (I5).
package trace

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// flatRow is one displayable line in the flattened, navigable view. The tree is
// flattened once per (un)fold so cursor movement is a simple index walk; depth
// and the collapsed flag drive indentation and the fold glyph.
type flatRow struct {
	step      Step
	depth     int
	hasKids   bool
	collapsed bool
}

// explorerModel is the Bubble Tea model. It owns the Trace, the per-node
// collapsed set (keyed by Seq, stable across reflows), the cursor, and the
// viewport offset. It is value-typed and copied by Bubble Tea like the rest of
// NilCore's TUI models.
type explorerModel struct {
	tr        *Trace
	collapsed map[uint64]bool
	rows      []flatRow
	cursor    int
	top       int // first visible row (simple scroll window)
	height    int
	width     int
}

// NewExplorer returns a Bubble Tea model exploring tr. Callers in the (tui-built)
// command layer wrap it in tea.NewProgram. Subtrees start expanded so the whole
// causal story is visible; the operator folds what they do not need.
func NewExplorer(tr *Trace) tea.Model {
	m := explorerModel{tr: tr, collapsed: map[uint64]bool{}, height: 24, width: 80}
	m.reflow()
	return m
}

func (m explorerModel) Init() tea.Cmd { return nil }

// reflow rebuilds the flat row list from the tree, honouring the collapsed set:
// a collapsed node's descendants are omitted. Called after every fold toggle and
// resize so the view always matches the fold state.
func (m *explorerModel) reflow() {
	m.rows = m.rows[:0]
	var walk func(steps []Step, depth int)
	walk = func(steps []Step, depth int) {
		for _, s := range steps {
			collapsed := m.collapsed[s.Seq]
			m.rows = append(m.rows, flatRow{
				step:      s,
				depth:     depth,
				hasKids:   len(s.Children) > 0,
				collapsed: collapsed,
			})
			if !collapsed {
				walk(s.Children, depth+1)
			}
		}
	}
	walk(m.tr.Steps, 0)
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m explorerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "enter", " ", "right", "left":
			// Toggle the fold on the node under the cursor (if it has children).
			if m.cursor < len(m.rows) && m.rows[m.cursor].hasKids {
				seq := m.rows[m.cursor].step.Seq
				m.collapsed[seq] = !m.collapsed[seq]
				m.reflow()
			}
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(m.rows) - 1
		}
		m.scrollIntoView()
	}
	return m, nil
}

// scrollIntoView keeps the cursor within the visible window above the 3-line
// chrome (header trust line + blank + footer hint).
func (m *explorerModel) scrollIntoView() {
	body := m.bodyHeight()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+body {
		m.top = m.cursor - body + 1
	}
	if m.top < 0 {
		m.top = 0
	}
}

func (m explorerModel) bodyHeight() int {
	h := m.height - 3 // header + blank + hint
	if h < 1 {
		return 1
	}
	return h
}

func (m explorerModel) View() string {
	var b strings.Builder

	// Header: the trust verdict, loud-red on a broken chain (I5).
	if m.tr.ChainVerified {
		b.WriteString(lipgloss.NewStyle().Bold(true).Render("why: " + fence(m.tr.Task)))
		b.WriteString("  " + lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓ chain verified"))
	} else {
		b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1")).
			Render("why: " + fence(m.tr.Task) + "  ✗ CHAIN BROKEN — not trustworthy"))
	}
	b.WriteString("\n\n")

	// Body: the visible window of flattened rows.
	body := m.bodyHeight()
	for i := m.top; i < len(m.rows) && i < m.top+body; i++ {
		b.WriteString(m.renderRow(m.rows[i], i == m.cursor))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Faint(true).
		Render("↑/↓ move · enter fold/unfold · q quit"))
	return b.String()
}

// renderRow renders one flat row: a cursor marker, a fold glyph for nodes with
// children, the indented «#Seq Title — Why», and an untrusted flag. Everything
// is fenced (I7) and a broken-chain row is dimmed-red.
func (m explorerModel) renderRow(r flatRow, selected bool) string {
	indent := strings.Repeat("  ", r.depth)

	fold := "  "
	if r.hasKids {
		if r.collapsed {
			fold = "▸ "
		} else {
			fold = "▾ "
		}
	}

	cursor := "  "
	if selected {
		cursor = "❯ "
	}

	line := fmt.Sprintf("%s%s%s#%d %s", cursor, indent, fold, r.step.Seq, fence(r.step.Title))
	if r.step.Why != "" {
		line += " — " + fence(r.step.Why)
	}

	style := lipgloss.NewStyle()
	switch {
	case r.step.Untrusted:
		style = style.Foreground(lipgloss.Color("1")) // red: do not trust
	case selected:
		style = style.Bold(true).Foreground(lipgloss.Color("6")) // cyan cursor row
	}
	return style.Render(line)
}
