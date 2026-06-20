package render

import (
	"fmt"
	"strings"

	"nilcore/internal/report"
	"nilcore/internal/termui"
)

// RenderText renders the report as a terminal report. With an off Style (the zero
// value, or a non-TTY-detected console) it emits ZERO ANSI escapes — clean lines
// for a pipe/CI log (the I6 guarantee). With an on Style it tints the verdict and
// every row: passed rows Success (green), failed rows Danger (red), a broken chain
// Danger. Untrusted Value/SourceURL/Output/Detail are redacted (I3) before print;
// no GREEN headline appears over a broken chain (I2).
func RenderText(m *report.ReportModel, st termui.Style) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n", st.Bold("Verification report: "+m.Run))
	fmt.Fprintf(&b, "generated %s\n\n", m.GeneratedAt.Format("2006-01-02 15:04:05 MST"))

	// The chain gate comes first: if it is broken nothing below can be trusted, so
	// the banner is loud and the GREEN headline is suppressed.
	if !m.ChainVerified {
		b.WriteString(st.Danger("⚠ "+brokenChainBanner) + "\n\n")
	}

	// Final verdict headline — green only when the shared gate allows it.
	if showGreen(m) {
		b.WriteString(st.Success("✔ "+greenHeadline) + "\n\n")
	} else {
		b.WriteString(st.Danger("✘ "+redHeadline) + "\n\n")
	}

	// Checks section.
	b.WriteString(st.Bold("Checks") + "\n")
	if len(m.Checks) == 0 {
		b.WriteString("  (none)\n")
	}
	for i := range m.Checks {
		c := m.Checks[i]
		mark, paint := "FAIL", st.Danger
		if c.Passed {
			mark, paint = "PASS", st.Success
		}
		line := fmt.Sprintf("  [%s] %-20s %s", mark, c.Family, dash(c.Task))
		if out := redact(c.Output); out != "" {
			line += "  " + st.Dim(truncate(out, 120))
		}
		b.WriteString(paint(line) + "\n")
	}
	b.WriteString("\n")

	// Artifact claim tables — the {value, source_url, verifier, status} rows.
	for i := range m.Artifacts {
		a := m.Artifacts[i]
		head := fmt.Sprintf("Artifact %s [%s] %s", a.ID, a.Kind, dash(a.Title))
		paintHead := st.Danger
		if a.Green {
			paintHead = st.Success
		}
		b.WriteString(paintHead(st.Bold(head)) + "\n")
		for j := range a.Claims {
			r := a.Claims[j]
			paint := st.Danger
			if string(r.Status) == statusPass {
				paint = st.Success
			}
			row := fmt.Sprintf("  %-10s claim=%s field=%s value=%q source=%s verifier=%s",
				strings.ToUpper(string(r.Status)),
				dash(r.ClaimID), dash(r.Field),
				redact(r.Value), dash(redact(r.SourceURL)), dash(r.Verifier))
			if d := redact(r.Detail); d != "" {
				row += " detail=" + truncate(d, 80)
			}
			b.WriteString(paint(row) + "\n")
		}
		b.WriteString("\n")
	}

	// Retry history — the ordered requeue chain.
	if len(m.Retries) > 0 {
		b.WriteString(st.Bold("Retry history") + "\n")
		for i := range m.Retries {
			r := m.Retries[i]
			mark, paint := "FAILED", st.Danger
			if r.Passed {
				mark, paint = "RESOLVED", st.Success
			}
			line := fmt.Sprintf("  #%d %-9s task=%s from=%s", r.Seq, mark, dash(r.Task), dash(r.ContinueFrom))
			if r.BaseBranch != "" {
				line += " base=" + r.BaseBranch
			}
			b.WriteString(paint(line) + "\n")
		}
		b.WriteString("\n")
	}

	return b.String()
}

// truncate bounds a tail for display, appending an ellipsis when it cuts.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
