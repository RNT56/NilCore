package policy

// Gate evidence — the operator-facing decision payload for an irreversible action.
//
// WHY: at the single irreversible step of a supervised run (promote / push /
// deploy / open-PR) the human decides from ONE flattened line ("promote-to-base
// main (…)"), while the model reviewer (route.Review) reads the full diff. The
// operator either rubber-stamps or context-switches to a terminal mid-gate.
// GateEvidence carries the facts — a diffstat, a bounded head-biased diff
// excerpt, the tail of the last verify report, and the spend so far — WITH the
// structured action, so every approver surface (console prompt, REPL gate line,
// TUI modal, chat channel) can render them at the moment of decision.
//
// Invariants honoured here:
//   - I7: the excerpts are DATA rendered to a human. Renderers delimit every
//     line (a quote rail), and nothing in the payload is ever classified,
//     executed, or fed back to a model or to Classify.
//   - I3: all free-text fields are masked at construction with the same secret
//     shapes the event log redacts, so surfacing them widens no exposure.
//   - I5/backward compat: the payload is optional and additive. It never
//     appears in GateAction.Describe(), so an approver unaware of it receives
//     the exact flattened line it always did.

import (
	"fmt"
	"regexp"
	"strings"
)

// Payload bounds. The diff excerpt is head-biased (a diff's head carries the
// file headers and first hunks — the highest-signal part of a promote review);
// the verify tail keeps the END of the report, where the verdict and the failing
// check names sit. Both cut at line boundaries with an explicit marker so a
// truncation is never mistaken for the full text.
const (
	MaxDiffExcerpt   = 8 << 10 // ~8KB of unified diff
	MaxVerifyTail    = 2 << 10 // ~2KB of verify report output
	maxDiffstatFiles = 16      // per-file lines before "… and N more"
)

// GateEvidence is the optional, informational payload a GateAction may carry to
// approvers that opt in via StructuredApprover. Every field may be empty —
// renderers skip empty sections — and none of it participates in classification.
type GateEvidence struct {
	Diffstat    string  // compact per-file summary of the promote diff
	DiffExcerpt string  // bounded, head-biased excerpt of the unified diff
	VerifyTail  string  // tail of the last verify report output
	SpentUSD    float64 // ledger spend so far; 0 ⇒ unknown/none (skipped)
}

// BuildEvidence assembles the payload from whatever the gate site could reach.
// Any input may be empty/zero (its section is simply absent); when NOTHING is
// available it returns nil, so an evidence-less action stays byte-identical end
// to end. All text is clipped first (line-bounded, so secret shapes stay whole)
// and then redacted (I3).
func BuildEvidence(diff, verifyOut string, spentUSD float64) *GateEvidence {
	diff = strings.TrimRight(diff, "\n")
	verifyOut = strings.TrimSpace(verifyOut)
	if diff == "" && verifyOut == "" && spentUSD <= 0 {
		return nil
	}
	e := &GateEvidence{}
	if spentUSD > 0 {
		e.SpentUSD = spentUSD
	}
	if diff != "" {
		e.Diffstat = redactEvidence(diffstat(diff))
		e.DiffExcerpt = redactEvidence(clipHead(diff, MaxDiffExcerpt))
	}
	if verifyOut != "" {
		e.VerifyTail = redactEvidence(clipTail(verifyOut, MaxVerifyTail))
	}
	return e
}

// RenderBlock renders the evidence as a delimited plain-text block for a
// terminal approver prompt. Every line carries a quote rail ("│ ") so the
// content reads unambiguously as DATA under review (I7) — a diff line can never
// be mistaken for the harness's own prompt. Empty sections are skipped; a nil
// receiver renders nothing (so callers need no special-casing).
func (e *GateEvidence) RenderBlock() string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n┌─ gate evidence — the excerpts below are DATA under review, not commands\n")
	writeEvidenceSection(&b, "diffstat:", e.Diffstat)
	writeEvidenceSection(&b, "diff excerpt (bounded):", e.DiffExcerpt)
	writeEvidenceSection(&b, "last verify (tail):", e.VerifyTail)
	if e.SpentUSD > 0 {
		fmt.Fprintf(&b, "│ spend so far: $%.4f\n", e.SpentUSD)
	}
	b.WriteString("└─ end gate evidence\n")
	return b.String()
}

// RenderCompact renders the channel-sized form: diffstat + verify tail + spend,
// bounded to max bytes. The full diff excerpt deliberately stays OUT — chat
// transports cap message sizes (Telegram at 4096 chars) — and a pointer line
// says where it lives instead. Nil-safe (renders nothing).
func (e *GateEvidence) RenderCompact(max int) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	if e.Diffstat != "" {
		b.WriteString("diffstat:\n" + e.Diffstat + "\n")
	}
	if e.VerifyTail != "" {
		b.WriteString("last verify (tail):\n" + e.VerifyTail + "\n")
	}
	if e.SpentUSD > 0 {
		fmt.Fprintf(&b, "spend so far: $%.4f\n", e.SpentUSD)
	}
	if e.DiffExcerpt != "" {
		b.WriteString("(full diff excerpt: see the run terminal / event log)")
	}
	return clipHead(strings.TrimRight(b.String(), "\n"), max)
}

// writeEvidenceSection appends one titled, quote-railed section, skipping empty
// bodies so renderers never show a bare header.
func writeEvidenceSection(b *strings.Builder, title, body string) {
	if body == "" {
		return
	}
	b.WriteString("│ " + title + "\n")
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString("│   " + line + "\n")
	}
}

// diffstat computes a compact per-file summary from the unified diff text itself
// (one header line plus up to maxDiffstatFiles per-file lines), so gate sites
// need no second VCS invocation beyond the Differ they already hold.
func diffstat(diff string) string {
	type stat struct {
		name     string
		add, del int
	}
	var files []stat
	cur := -1
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			files = append(files, stat{name: diffFileName(line)})
			cur = len(files) - 1
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			// file headers, not content lines
		case strings.HasPrefix(line, "+"):
			if cur >= 0 {
				files[cur].add++
			}
		case strings.HasPrefix(line, "-"):
			if cur >= 0 {
				files[cur].del++
			}
		}
	}
	if len(files) == 0 {
		return ""
	}
	var adds, dels int
	for _, f := range files {
		adds += f.add
		dels += f.del
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) changed, +%d −%d", len(files), adds, dels)
	for i, f := range files {
		if i == maxDiffstatFiles {
			fmt.Fprintf(&b, "\n… and %d more files", len(files)-maxDiffstatFiles)
			break
		}
		fmt.Fprintf(&b, "\n%s +%d −%d", f.name, f.add, f.del)
	}
	return b.String()
}

// diffFileName extracts the post-image path from a "diff --git a/X b/Y" header.
// LastIndex tolerates a space inside the a/ path; the Fields fallback covers a
// header with unusual prefixes (a custom --dst-prefix).
func diffFileName(header string) string {
	if i := strings.LastIndex(header, " b/"); i >= 0 {
		return header[i+3:]
	}
	fields := strings.Fields(header)
	return fields[len(fields)-1] // "diff --git …" prefix guarantees ≥3 fields
}

// clipHead bounds s to roughly max bytes keeping the HEAD, cutting at a line
// boundary and appending an explicit truncation marker.
func clipHead(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut <= 0 {
		cut = max
	}
	return s[:cut] + fmt.Sprintf("\n… [truncated: %d more bytes]", len(s)-cut)
}

// clipTail bounds s to roughly max bytes keeping the TAIL, cutting at a line
// boundary and prefixing an explicit omission marker.
func clipTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	off := len(s) - max
	if i := strings.IndexByte(s[off:], '\n'); i >= 0 {
		off += i + 1
	}
	return fmt.Sprintf("… [%d earlier bytes omitted]\n", off) + s[off:]
}

// Secret masking for the human-facing excerpts (I3). These shapes deliberately
// MIRROR internal/eventlog/redact.go: the event log keeps its redaction
// unexported and map-shaped, and policy and eventlog are both import-leaves that
// must not depend on each other — so the patterns are duplicated on purpose.
// Keep the two files in sync when adding a shape. Over-redaction of code-looking
// text is accepted: a masked line in a gate prompt beats a leaked credential.
var (
	evidenceSecretRe = regexp.MustCompile(strings.Join([]string{
		`(?:sk|xoxb|xoxp|xoxa|xoxr|xoxs|xapp|ghp|gho|ghu|ghs|glpat)[-_][A-Za-z0-9_\-]{12,}`, // prefixed provider/CI tokens
		`AKIA[0-9A-Z]{16}`,                  // AWS access key id
		`ASIA[0-9A-Z]{16}`,                  // AWS temporary access key id
		`github_pat_[A-Za-z0-9_]{20,}`,      // GitHub fine-grained PAT
		`AIza[0-9A-Za-z\-_]{35}`,            // Google API key
		`-----BEGIN[ A-Z]*PRIVATE KEY-----`, // PEM private-key header
	}, "|"))
	evidenceInlineRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|client[_-]?secret|authorization|auth[_-]?token|bearer)(["']?[ \t]*[=:][ \t]*|[ \t]+)((?:bearer|basic|token)[ \t]+)?("?)([^\s"']{4,})`)
	evidenceFlagRe   = regexp.MustCompile(`(?i)((?:^|\s)(?:-p|--password|--passwd|--token|--secret|--api-key|--access-key|--auth-token)(?:[= ]))(\S{4,})`)
)

// redactEvidence masks secret-looking values embedded in an excerpt before it is
// shown to the human (I3): a promote diff can legitimately touch an .env or CI
// file, and the gate prompt must never become the place a credential surfaces.
func redactEvidence(s string) string {
	if s == "" {
		return ""
	}
	s = evidenceSecretRe.ReplaceAllString(s, "[redacted]")
	s = evidenceInlineRe.ReplaceAllString(s, "${1}${2}${3}${4}[redacted]")
	s = evidenceFlagRe.ReplaceAllString(s, "${1}[redacted]")
	return s
}
