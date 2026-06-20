package render

// matrix.go is the SWARM cross-shard MATRIX renderer + the JSON deliverable helper
// (Phase 12, SW-T06). RenderMatrix pivots a SwarmReport's per-claim traces into a
// grid — rows are artifacts, columns are the sorted union of claim fields, each cell
// is the claim's verifier STATUS plus its (redacted, escaped) value and a numbered
// footnote pointing at the (redacted) source. MarshalRedacted is the `json` format's
// sink: it emits a REDACTED projection of the model, never the raw ReportModel, so a
// key smuggled into a SourceURL can never ride out in the JSON deliverable.
//
// Trust + invariants. The matrix is the display boundary for UNTRUSTED, model-authored
// data (I7): every claim Value and Source locator is run through redact (I3) THEN
// escapeCell (neutralizes the markup/control bytes a crafted value could use to break
// a terminal cell or smuggle a <script>), so a payload renders as inert text. GREEN is
// a verifier projection, never a citations claim (I2): a cell is only painted Success
// when its trusted Status is exactly "pass" — a non-pass status NEVER gets a green
// cell, even if the surrounding artifact looks green. Column order is the
// deterministic sorted field union, so the same model always renders byte-identically.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"nilcore/internal/report"
	"nilcore/internal/termui"
)

// RenderMatrix pivots the swarm report's claim traces into a cross-shard grid. Rows
// are artifacts (in first-seen order — the order ReplayReport folded them, which is
// log order), columns are the deterministic sorted union of every claim field seen,
// and each cell carries the claim's STATUS + redacted/escaped value with a numbered
// footnote citing the redacted source. With an off Style it emits ZERO ANSI (the I6
// pipe/CI guarantee); with an on Style a pass cell is Success-tinted and every other
// status Danger-tinted — never a green cell over a non-pass status (I2).
func RenderMatrix(sr *report.SwarmReport, st termui.Style) string {
	var b strings.Builder
	if sr == nil || sr.Base == nil {
		return ""
	}
	m := sr.Base

	fmt.Fprintf(&b, "%s\n", st.Bold("Claim matrix: "+escapeCell(m.Run)))

	// The chain gate comes first: a broken chain means the evidence may be tampered,
	// so the matrix is shown with a loud RED banner and the swarm clean-pass headline
	// is suppressed (I2/I5).
	if !m.ChainVerified {
		b.WriteString(st.Danger("⚠ "+brokenChainBanner) + "\n")
	}
	if sr.Swarm.FinalCleanPass {
		b.WriteString(st.Success("✔ swarm clean — every shard passed") + "\n")
	} else {
		b.WriteString(st.Danger("✘ swarm not clean") + "\n")
	}
	b.WriteString("\n")

	if len(m.ClaimTraces) == 0 {
		b.WriteString("(no claim traces)\n")
		return b.String()
	}

	// Build the deterministic column set (sorted field union) and the row set
	// (artifact ids in first-seen order). The cell index is keyed by (artifact,field).
	cols := sortedFieldUnion(m.ClaimTraces)
	rows, byCell := pivot(m.ClaimTraces)

	// Footnotes accumulate one numbered entry per cell that carries a source, so the
	// grid stays narrow and the (redacted) locators are listed once below it.
	var notes []string

	// Header row: a leading "artifact" gutter then each field column.
	header := []string{padCell("artifact", artifactColWidth)}
	for _, c := range cols {
		header = append(header, padCell(escapeCell(c), fieldColWidth))
	}
	b.WriteString(st.Bold(strings.Join(header, " | ")) + "\n")

	for _, rowID := range rows {
		cells := []string{padCell(escapeCell(rowID), artifactColWidth)}
		for _, field := range cols {
			tr, ok := byCell[cellKey{rowID, field}]
			if !ok {
				cells = append(cells, padCell("·", fieldColWidth))
				continue
			}
			text := cellText(tr, &notes)
			// Paint ONLY a pass cell green; every other status (fail/stale/
			// unverifiable/unverified/empty) is Danger — never a green cell over a
			// non-pass status (I2).
			paint := st.Danger
			if string(tr.Status) == statusPass {
				paint = st.Success
			}
			cells = append(cells, paint(padCell(text, fieldColWidth)))
		}
		b.WriteString(strings.Join(cells, " | ") + "\n")
	}

	if len(notes) > 0 {
		b.WriteString("\n" + st.Bold("Sources") + "\n")
		for _, n := range notes {
			b.WriteString("  " + n + "\n")
		}
	}
	return b.String()
}

// column widths for the fixed-width grid. They bound each cell so a long value cannot
// blow out the row; the full (redacted) value rides in the footnote.
const (
	artifactColWidth = 16
	fieldColWidth    = 22
)

// cellKey indexes a trace by its (artifact, field) coordinate in the pivot grid.
type cellKey struct {
	artifact string
	field    string
}

// sortedFieldUnion returns the deterministic, de-duplicated, sorted set of every
// claim field across all traces — the matrix's column order. Sorting makes the grid
// byte-identical for the same model regardless of log order (the determinism the test
// asserts).
func sortedFieldUnion(traces []report.ClaimTrace) []string {
	seen := map[string]bool{}
	var cols []string
	for i := range traces {
		f := traces[i].Field
		if f == "" {
			f = "-"
		}
		if !seen[f] {
			seen[f] = true
			cols = append(cols, f)
		}
	}
	sort.Strings(cols)
	return cols
}

// pivot groups the traces by artifact (row, first-seen order) and indexes each by its
// (artifact, field) cell coordinate. When two claims share a field on one artifact the
// LAST one wins — a deterministic, documented collapse (the field union is the axis,
// not the claim id).
func pivot(traces []report.ClaimTrace) ([]string, map[cellKey]report.ClaimTrace) {
	var rows []string
	rowSeen := map[string]bool{}
	byCell := map[cellKey]report.ClaimTrace{}
	for i := range traces {
		tr := traces[i]
		if !rowSeen[tr.ArtifactID] {
			rowSeen[tr.ArtifactID] = true
			rows = append(rows, tr.ArtifactID)
		}
		field := tr.Field
		if field == "" {
			field = "-"
		}
		byCell[cellKey{tr.ArtifactID, field}] = tr
	}
	return rows, byCell
}

// cellText renders one cell's body: the uppercased STATUS, the redacted+escaped value
// in brackets, and (when the trace has a source) a numbered footnote marker whose
// entry is appended to notes. Value and the source locator are UNTRUSTED model data,
// so both pass through redact (I3) then escapeCell (I7) before they reach the cell or
// the footnote.
func cellText(tr report.ClaimTrace, notes *[]string) string {
	status := strings.ToUpper(string(tr.Status))
	if status == "" {
		status = "UNVERIFIED"
	}
	val := escapeCell(redact(tr.Value))
	body := status + "[" + truncate(val, fieldColWidth) + "]"
	if tr.Source.Locator != "" {
		n := len(*notes) + 1
		*notes = append(*notes, fmt.Sprintf("[%d] %s", n, escapeCell(redactSource(tr.Source.Locator))))
		body += fmt.Sprintf("(%d)", n)
	}
	return body
}

// padCell left-justifies s to width, truncating (with an ellipsis) when it is longer,
// so the fixed-width grid stays aligned regardless of cell content length.
func padCell(s string, width int) string {
	if len(s) > width {
		return truncate(s, width-1)
	}
	return s + strings.Repeat(" ", width-len(s))
}

// keyParamNames is the closed set of query-param names that carry a credential. A
// param whose (lower-cased) name is in this set is dropped from a SourceURL entirely,
// regardless of its value length — closing the gap left by the length-gated secretRe
// (which only fires on values >=8 chars). I3: the SourceURL is required key-free, but
// the deliverables redact defensively in case a worker smuggled a key into one.
var keyParamNames = map[string]bool{
	"api_key":      true,
	"apikey":       true,
	"key":          true,
	"token":        true,
	"access_token": true,
	"secret":       true,
	"password":     true,
	"sig":          true,
	"signature":    true,
}

// redactSource masks a claim's SourceURL for any rendered/persisted output (I3). It
// FIRST strips every key-looking query param by name (unconditional, any value length)
// and THEN runs the shared secretRe redactor over the remainder, so neither a short
// ?api_key=secret nor an embedded provider key shape can survive into the matrix or
// the JSON deliverable. A locator that does not parse as a URL still gets the secretRe
// pass, so a non-URL provenance string is not waved through.
func redactSource(loc string) string {
	if loc == "" {
		return loc
	}
	if u, err := url.Parse(loc); err == nil && u.RawQuery != "" {
		q := u.Query()
		stripped := false
		for name := range q {
			if keyParamNames[strings.ToLower(name)] {
				q.Del(name)
				stripped = true
			}
		}
		if stripped {
			u.RawQuery = q.Encode()
			loc = u.String()
		}
	}
	return redact(loc)
}

// escapeCell neutralizes UNTRUSTED model text for a TERMINAL cell (I7): it strips the
// ANSI/control bytes a crafted value could use to repaint the terminal or hide
// content, and HTML-escapes the markup metacharacters so a <script>/<img onerror=>
// payload renders as inert literal text (the same inert-text guarantee the HTML
// renderer gives, applied here so the matrix string is safe to embed anywhere). It is
// applied AFTER redact so a secret is masked first.
func escapeCell(s string) string {
	if s == "" {
		return s
	}
	// Drop ESC and other C0 control bytes (except none are kept — a cell is single-
	// line) so no ANSI sequence or carriage-return overwrite survives.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x7f || (r < 0x20) {
			b.WriteByte(' ') // collapse any control char (incl. ESC, \n, \r, \t) to a space
			continue
		}
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// --- JSON deliverable (the `json` format) ---

// redactedReport is the REDACTED JSON projection of a SwarmReport. It mirrors only the
// fields a consumer needs and routes every UNTRUSTED model string (claim Value, source
// Locator, verifier Detail) through redact (I3) so the JSON deliverable can never leak
// an api_key=/token= query param or a provider key shape. Trusted fields (Status,
// Verifier, the swarm counts) are carried as-is. It is a DISTINCT type from
// report.ReportModel on purpose: marshaling the raw model would emit the un-redacted
// SourceURL/Value verbatim, which is exactly the leak this guards against.
type redactedReport struct {
	Run            string           `json:"run"`
	ChainVerified  bool             `json:"chain_verified"`
	FinalPass      bool             `json:"final_pass"`
	FinalCleanPass bool             `json:"swarm_final_clean_pass"`
	Swarm          redactedSwarm    `json:"swarm"`
	Claims         []redactedClaim  `json:"claims"`
	SchemaDefects  []redactedDefect `json:"schema_defects,omitempty"`
}

// redactedSwarm is the metadata-only swarm dimension (counts only, no model field).
type redactedSwarm struct {
	Checked   int `json:"checked"`
	Passed    int `json:"passed"`
	Failed    int `json:"failed"`
	RetryPass int `json:"retry_pass"`
	Remaining int `json:"remaining"`
	Pass      int `json:"pass"`
}

// redactedClaim is one claim trace with its UNTRUSTED fields redacted. Value and
// Source are the model-authored fields the redactor scrubs; Status/Verifier/Field are
// trusted.
type redactedClaim struct {
	Artifact string `json:"artifact"`
	Claim    string `json:"claim"`
	Field    string `json:"field"`
	Value    string `json:"value"`
	Source   string `json:"source"`
	Resolved bool   `json:"source_resolved"`
	Verifier string `json:"verifier"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Attempt  int    `json:"attempt,omitempty"`
}

// redactedDefect is one schema defect row — already harness-authored metadata, carried
// through unchanged (no model field rides in a Defect, so nothing to redact).
type redactedDefect struct {
	Artifact string `json:"artifact"`
	Claim    string `json:"claim,omitempty"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Reason   string `json:"reason"`
}

// MarshalRedacted produces the `json` deliverable: an indented JSON document of the
// REDACTED projection (never the raw report.ReportModel). Every model-authored string
// is passed through the SAME redact the renderers use, so a SourceURL carrying
// ?api_key=… emits no secret (I3) and a <-bearing Value cannot inject markup into a
// consumer that renders the JSON unescaped (the value is redacted; escaping is the
// consumer's, but the redaction guarantee — no leaked key — is unconditional here).
func MarshalRedacted(sr *report.SwarmReport) ([]byte, error) {
	if sr == nil || sr.Base == nil {
		return nil, fmt.Errorf("render: nil swarm report")
	}
	m := sr.Base
	out := redactedReport{
		Run:            m.Run,
		ChainVerified:  m.ChainVerified,
		FinalPass:      m.FinalPass,
		FinalCleanPass: sr.Swarm.FinalCleanPass,
		Swarm: redactedSwarm{
			Checked:   sr.Swarm.Checked,
			Passed:    sr.Swarm.Passed,
			Failed:    sr.Swarm.Failed,
			RetryPass: sr.Swarm.RetryPass,
			Remaining: sr.Swarm.Remaining,
			Pass:      sr.Swarm.Pass,
		},
	}
	for i := range m.ClaimTraces {
		tr := m.ClaimTraces[i]
		out.Claims = append(out.Claims, redactedClaim{
			Artifact: tr.ArtifactID,
			Claim:    tr.ClaimID,
			Field:    tr.Field,
			Value:    redact(tr.Value),                // UNTRUSTED model value — redacted (I3)
			Source:   redactSource(tr.Source.Locator), // UNTRUSTED locator — key params stripped so no api_key leaks (I3)
			Resolved: tr.Source.Resolved,
			Verifier: tr.Verifier,
			Status:   string(tr.Status),
			Detail:   redact(tr.Detail),
			Attempt:  tr.Attempt,
		})
	}
	for i := range m.SchemaDefects {
		d := m.SchemaDefects[i]
		out.SchemaDefects = append(out.SchemaDefects, redactedDefect{
			Artifact: d.ArtifactID,
			Claim:    d.ClaimID,
			Field:    d.Field,
			Code:     d.Code,
			Reason:   d.Reason,
		})
	}
	return json.MarshalIndent(out, "", "  ")
}
