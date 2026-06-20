//go:build tui

// board_tui.go is the OPTIONAL Charm dashboard over the SAME Board.Snapshot the pure
// renderer consumes — a Bubble Tea model that polls Snapshot on a ticker and paints a
// live grid with lipgloss. It is gated behind the `tui` build tag, so the DEFAULT
// nilcore binary links ZERO Charm (invariant I6: the sanctioned Charm exception is
// isolated to opt-in presentation files, exactly as cmd/nilcore/tui.go isolates it for
// the chat TUI). Building this file requires `-tags tui`; without the tag the package
// compiles Charm-free.
//
// Same data, richer skin. The dashboard reads the identical Snapshot value the
// off-Charm RenderScoreboard reads — so the two can never disagree, and the I7/I3 trust
// boundary holds here too: it shows only the TRUSTED counts and per-shard fields
// (Status/SourceURL), never a model-authored Value. The Board is the single source of
// truth; this is a view.
package board

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// dashTick is the Snapshot poll cadence — fast enough to feel live, slow enough not to
// busy-spin. The model re-reads the Board on each tick (Snapshot is a cheap O(shards)
// copy-out), so the dashboard always reflects the latest tally without the Board having
// to push.
const dashTick = 250 * time.Millisecond

// snapshotter is the one thing the dashboard needs from a Board: a Snapshot poll. It is
// an interface (satisfied by *Board) so the model can be unit-driven with a fake in a
// tui-tagged test without standing up a whole run.
type snapshotter interface {
	Snapshot() Snapshot
}

// DashboardModel is a Bubble Tea model that renders a Board's live Snapshot. Construct
// it with NewDashboard(board) and run it under tea.NewProgram. It owns no swarm state —
// it only polls and paints, so closing the program never touches the run.
type DashboardModel struct {
	src  snapshotter
	last Snapshot
	w, h int
	done bool
}

// NewDashboard returns a dashboard model polling src (a *Board). The model takes its
// first Snapshot lazily on the first tick, so constructing it is side-effect-free.
func NewDashboard(src snapshotter) DashboardModel {
	return DashboardModel{src: src}
}

// tickMsg fires on the poll ticker; each one triggers a fresh Snapshot read.
type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(dashTick, func(time.Time) tea.Msg { return tickMsg{} })
}

// Init starts the poll ticker.
func (m DashboardModel) Init() tea.Cmd { return tick() }

// Update handles ticks (re-poll the Snapshot), resize, and quit keys. It is a pure
// view: it never mutates the Board.
func (m DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.src != nil {
			m.last = m.src.Snapshot()
		}
		return m, tick()
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// View paints the current Snapshot. It mirrors RenderScoreboard's content — the same
// headline, counts, cost/time/token line, and per-shard table — under a lipgloss skin,
// so the Charm dashboard and the plain renderer never diverge in WHAT they show.
func (m DashboardModel) View() string {
	s := m.last
	var b strings.Builder

	head := dashWarn.Render(fmt.Sprintf("• swarm pass %d — %d/%d remaining", s.Pass, s.Remaining, s.Total))
	if s.FinalCleanPass {
		head = dashOK.Render("✔ swarm clean — every shard passed")
	}
	b.WriteString(head + "\n")

	b.WriteString(fmt.Sprintf(
		"checked %d  %s  %s  %s  remaining %d\n",
		s.Checked,
		dashOK.Render(fmt.Sprintf("passed %d", s.Passed)),
		dashErr.Render(fmt.Sprintf("failed %d", s.Failed)),
		dashInfo.Render(fmt.Sprintf("retry-pass %d", s.RetryPass)),
		s.Remaining,
	))

	b.WriteString(dashDim.Render(fmt.Sprintf(
		"cost $%.4f · time %s · tokens %d", s.Cost, humanDuration(s.RunElapsed), s.Tokens,
	)) + "\n")

	for _, r := range s.Shards {
		glyph, style := "✘", dashErr
		if r.Passed {
			glyph, style = "✔", dashOK
		}
		tag := ""
		if r.Exhausted {
			tag = " (exhausted)"
		}
		row := style.Render(fmt.Sprintf("%s %s%s", glyph, r.ID, tag))
		meta := fmt.Sprintf("pass %d · status=%s", r.Pass, r.Status)
		if r.SourceURL != "" {
			meta += " · src=" + r.SourceURL
		}
		b.WriteString("  " + row + "  " + dashDim.Render(meta) + "\n")
	}

	return dashBox.Render(b.String())
}

// lipgloss styles for the dashboard — the same palette family as the chat TUI's
// (cmd/nilcore/tui.go), kept local so this file is the only swarm-side Charm consumer.
var (
	dashBox  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	dashOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	dashErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	dashWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	dashInfo = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	dashDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)
