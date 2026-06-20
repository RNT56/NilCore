package render

import (
	"fmt"
	"html"
	"strings"

	"nilcore/internal/report"
)

// RenderHTML renders the report as ONE self-contained HTML document: inline CSS
// only, NO <script>, NO external asset URL — so it is safe to open from disk and
// carries no live code or remote fetch (I7). Every model-authored or verifier
// string (Value, SourceURL, Output, Detail, Title, Run) is redacted (I3) then
// html.EscapeString-escaped, so a `<script>`/`onerror=` payload in a claim Value
// renders as inert text. No GREEN headline appears over a broken chain (I2).
func RenderHTML(m *report.ReportModel) string {
	var b strings.Builder

	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">\n")
	b.WriteString("<title>Verification report: " + esc(m.Run) + "</title>\n")
	b.WriteString("<style>\n" + inlineCSS + "</style>\n</head>\n<body>\n")

	b.WriteString("<h1>Verification report: " + esc(m.Run) + "</h1>\n")
	b.WriteString("<p class=\"meta\">generated " + esc(m.GeneratedAt.Format("2006-01-02 15:04:05 MST")) + "</p>\n")

	if !m.ChainVerified {
		b.WriteString("<div class=\"banner fail\">⚠ " + esc(brokenChainBanner) + "</div>\n")
	}
	if showGreen(m) {
		b.WriteString("<div class=\"verdict pass\">✔ " + esc(greenHeadline) + "</div>\n")
	} else {
		b.WriteString("<div class=\"verdict fail\">✘ " + esc(redHeadline) + "</div>\n")
	}

	// Checks.
	b.WriteString("<h2>Checks</h2>\n")
	if len(m.Checks) == 0 {
		b.WriteString("<p class=\"empty\">(none)</p>\n")
	} else {
		b.WriteString("<table class=\"checks\"><thead><tr><th>Status</th><th>Family</th><th>Task</th><th>Output</th></tr></thead><tbody>\n")
		for i := range m.Checks {
			c := m.Checks[i]
			cls, label := "fail", "FAIL"
			if c.Passed {
				cls, label = "pass", "PASS"
			}
			fmt.Fprintf(&b, "<tr class=\"%s\"><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				cls, label, esc(c.Family), esc(dash(c.Task)), esc(redact(c.Output)))
		}
		b.WriteString("</tbody></table>\n")
	}

	// Artifact claim tables.
	for i := range m.Artifacts {
		a := m.Artifacts[i]
		cls := "fail"
		if a.Green {
			cls = "pass"
		}
		fmt.Fprintf(&b, "<h2 class=\"%s\">Artifact %s <span class=\"kind\">[%s]</span> %s</h2>\n",
			cls, esc(a.ID), esc(string(a.Kind)), esc(dash(a.Title)))
		b.WriteString("<table class=\"claims\"><thead><tr><th>Status</th><th>Claim</th><th>Field</th><th>Value</th><th>Source</th><th>Verifier</th><th>Detail</th></tr></thead><tbody>\n")
		for j := range a.Claims {
			r := a.Claims[j]
			rc := "fail"
			if string(r.Status) == statusPass {
				rc = "pass"
			}
			fmt.Fprintf(&b, "<tr class=\"%s\"><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				rc, esc(strings.ToUpper(string(r.Status))),
				esc(dash(r.ClaimID)), esc(dash(r.Field)),
				esc(redact(r.Value)), esc(dash(redact(r.SourceURL))),
				esc(dash(r.Verifier)), esc(redact(r.Detail)))
		}
		b.WriteString("</tbody></table>\n")
	}

	// Retry history.
	if len(m.Retries) > 0 {
		b.WriteString("<h2>Retry history</h2>\n")
		b.WriteString("<table class=\"retries\"><thead><tr><th>Seq</th><th>Outcome</th><th>Task</th><th>From</th><th>Base</th></tr></thead><tbody>\n")
		for i := range m.Retries {
			r := m.Retries[i]
			rc, label := "fail", "FAILED"
			if r.Passed {
				rc, label = "pass", "RESOLVED"
			}
			fmt.Fprintf(&b, "<tr class=\"%s\"><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				rc, r.Seq, label, esc(dash(r.Task)), esc(dash(r.ContinueFrom)), esc(dash(r.BaseBranch)))
		}
		b.WriteString("</tbody></table>\n")
	}

	b.WriteString("</body></html>\n")
	return b.String()
}

// esc redacts then HTML-escapes an untrusted string. Redaction runs FIRST so a
// secret is masked even if it would otherwise survive escaping unchanged.
func esc(s string) string {
	return html.EscapeString(redact(s))
}

// inlineCSS is the entire stylesheet, embedded so the document needs no network
// asset (I7). No url(), no @import, no remote font.
const inlineCSS = `
body{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;margin:2rem;color:#1b1b1b;background:#fff}
h1{font-size:1.4rem}
h2{font-size:1.1rem;margin-top:1.6rem}
.meta{color:#666}
.banner{padding:.6rem .8rem;border-radius:4px;font-weight:bold;margin:.8rem 0}
.verdict{padding:.6rem .8rem;border-radius:4px;font-weight:bold;margin:.8rem 0;font-size:1.1rem}
.pass{color:#0a6b2e}
.fail{color:#9b1c1c}
.banner.fail{background:#fde8e8}
.verdict.pass{background:#e6f4ea}
.verdict.fail{background:#fde8e8}
.empty{color:#888}
.kind{color:#666;font-weight:normal}
table{border-collapse:collapse;width:100%;margin:.5rem 0;font-size:.85rem}
th,td{border:1px solid #ddd;padding:.3rem .5rem;text-align:left;vertical-align:top;word-break:break-all}
th{background:#f3f3f3}
tr.pass td:first-child{color:#0a6b2e;font-weight:bold}
tr.fail td:first-child{color:#9b1c1c;font-weight:bold}
`
