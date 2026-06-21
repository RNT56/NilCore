package trust

import (
	"fmt"
	"strings"

	"nilcore/internal/termui"
)

// Render formats a Snapshot as a human-readable scoreboard: one row per backend
// (name · races · wins · pass-rate, already sorted best-first by the snapshot),
// followed by the eval-config rows when any are present. Styling is delegated to
// termui.Style, so on a non-terminal (off Style) the output is plain text with
// ZERO ANSI escapes (the I6 SSH/CI/pipe guarantee); the layout and ordering are
// identical either way, so the rendering is deterministic.
//
// The header carries the I2 reminder in plain words — "the ledger ranks, the
// verifier still decides" — so no reader mistakes a scoreboard for a verdict. The
// ledger biases which backend is tried first; the verifier alone decides "done".
func Render(snap Snapshot, st termui.Style) string {
	var b strings.Builder

	b.WriteString(st.Bold("Trust Ledger — strength routing"))
	b.WriteByte('\n')
	b.WriteString(st.Dim("verifier-judged outcomes — the ledger ranks, the verifier still decides."))
	b.WriteByte('\n')

	if len(snap.Backends) == 0 {
		b.WriteString(st.Dim("no earned outcomes yet — routing defers to the configured default."))
		b.WriteByte('\n')
	} else {
		b.WriteByte('\n')
		// Column widths size to the widest backend name so the table stays aligned
		// whether one backend or ten are present.
		nameW := len("backend")
		for _, s := range snap.Backends {
			if len(s.Backend) > nameW {
				nameW = len(s.Backend)
			}
		}
		header := fmt.Sprintf("%-*s  %6s  %6s  %9s", nameW, "backend", "races", "wins", "pass-rate")
		b.WriteString(st.Dim(header))
		b.WriteByte('\n')
		for _, s := range snap.Backends {
			row := fmt.Sprintf("%-*s  %6d  %6d  %8.1f%%",
				nameW, s.Backend, s.Races, s.Wins, s.PassRate*100)
			b.WriteString(row)
			b.WriteByte('\n')
		}
	}

	// Eval-config rows, only when present: a config aggregates a model+backend pair
	// over a measured suite, so it reads as separate evidence below the per-backend
	// scoreboard rather than mixed into it.
	if len(snap.Configs) > 0 {
		b.WriteByte('\n')
		b.WriteString(st.Bold("eval configs"))
		b.WriteByte('\n')
		nameW := len("config")
		for _, c := range snap.Configs {
			if len(c.Config) > nameW {
				nameW = len(c.Config)
			}
		}
		header := fmt.Sprintf("%-*s  %9s  %5s  %10s", nameW, "config", "pass-rate", "cases", "total-cost")
		b.WriteString(st.Dim(header))
		b.WriteByte('\n')
		for _, c := range snap.Configs {
			row := fmt.Sprintf("%-*s  %8.1f%%  %5d  %10.4f",
				nameW, c.Config, c.PassRate*100, c.Cases, c.TotalCost)
			b.WriteString(row)
			b.WriteByte('\n')
		}
	}

	return b.String()
}
