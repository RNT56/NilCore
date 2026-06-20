package render

import (
	"fmt"
	"strings"

	"nilcore/internal/report"
)

// RenderMarkdown renders the report as hand-rolled GitHub-flavoured markdown (no
// template, no markdown module — stdlib string building only, I6). The GREEN
// final-pass headline appears ONLY when ChainVerified && every status is pass
// (showGreen) — proving the markdown is a projection of the harness-computed
// verdict, NOT a citations-emitter (the Pillar-6 NON-GOAL guard). A broken chain
// prints the RED "report not trustworthy" banner and suppresses GREEN (I2/I5).
// Untrusted Value/SourceURL/Output/Detail are redacted (I3) and pipe-escaped so a
// crafted value cannot break out of a table cell.
func RenderMarkdown(m *report.ReportModel) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Verification report: %s\n\n", mdInline(m.Run))
	fmt.Fprintf(&b, "_generated %s_\n\n", m.GeneratedAt.Format("2006-01-02 15:04:05 MST"))

	if !m.ChainVerified {
		fmt.Fprintf(&b, "> ⚠ **%s**\n\n", brokenChainBanner)
	}
	if showGreen(m) {
		fmt.Fprintf(&b, "## ✔ %s\n\n", greenHeadline)
	} else {
		fmt.Fprintf(&b, "## ✘ %s\n\n", redHeadline)
	}

	// Checks.
	b.WriteString("## Checks\n\n")
	if len(m.Checks) == 0 {
		b.WriteString("_(none)_\n\n")
	} else {
		b.WriteString("| Status | Family | Task | Output |\n|---|---|---|---|\n")
		for i := range m.Checks {
			c := m.Checks[i]
			status := "❌ FAIL"
			if c.Passed {
				status = "✅ PASS"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
				status, mdCell(c.Family), mdCell(dash(c.Task)), mdCell(redact(c.Output)))
		}
		b.WriteString("\n")
	}

	// Artifact claim tables.
	for i := range m.Artifacts {
		a := m.Artifacts[i]
		green := "❌"
		if a.Green {
			green = "✅"
		}
		fmt.Fprintf(&b, "## %s Artifact `%s` [%s] %s\n\n", green, mdInline(a.ID), mdInline(string(a.Kind)), mdInline(dash(a.Title)))
		b.WriteString("| Status | Claim | Field | Value | Source | Verifier | Detail |\n|---|---|---|---|---|---|---|\n")
		for j := range a.Claims {
			r := a.Claims[j]
			status := "❌ " + strings.ToUpper(string(r.Status))
			if string(r.Status) == statusPass {
				status = "✅ PASS"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
				status, mdCell(dash(r.ClaimID)), mdCell(dash(r.Field)),
				mdCell(redact(r.Value)), mdCell(dash(redact(r.SourceURL))),
				mdCell(dash(r.Verifier)), mdCell(redact(r.Detail)))
		}
		b.WriteString("\n")
	}

	// Retry history.
	if len(m.Retries) > 0 {
		b.WriteString("## Retry history\n\n")
		b.WriteString("| Seq | Outcome | Task | From | Base |\n|---|---|---|---|---|\n")
		for i := range m.Retries {
			r := m.Retries[i]
			outcome := "❌ FAILED"
			if r.Passed {
				outcome = "✅ RESOLVED"
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n",
				r.Seq, outcome, mdCell(dash(r.Task)), mdCell(dash(r.ContinueFrom)), mdCell(dash(r.BaseBranch)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// mdCell makes an untrusted string safe inside a markdown table cell: a literal
// pipe would start a new column and a newline would break the row, so both are
// neutralized. (Redaction is applied by the caller before this.)
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// mdInline neutralizes a pipe/newline for inline (non-table) markdown contexts
// like a heading, where a raw newline would split the element.
func mdInline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}
