package trace

import (
	"fmt"
	"sort"
	"strings"

	"nilcore/internal/termui"
)

// render.go prints a Trace as an indented causal tree. Three properties are
// load-bearing and tested:
//
//   - Deterministic: the same Trace renders byte-for-byte the same string.
//     Detail fields are emitted in sorted key order; nothing depends on map
//     iteration order or wall-clock.
//
//   - Zero ANSI off-Style: every colour goes through termui.Style, which is a
//     no-op when the console did not opt in, so piped/redirected output is clean
//     text. A caller wanting plain output passes the zero Style.
//
//   - No raw-body leak (I7): only allowlisted metadata reaches Detail, and every
//     value is fenced (newlines/escapes neutralized, length-capped) before it is
//     printed, so even a value that smuggled a body upstream cannot break the
//     tree layout or inject control sequences.
//
// A CLEAN headline (the green ✓ "verified" line) appears ONLY when
// ChainVerified. On a broken chain the header carries a red ✗ and the loud
// CHAIN BROKEN verdict, and every node is tagged untrusted (I5).

// Render returns the full textual trace. st controls colour; the zero Style
// yields plain text.
func Render(tr *Trace, st termui.Style) string {
	if tr == nil {
		return ""
	}
	var b strings.Builder
	renderHeader(&b, tr, st)
	for _, s := range tr.Steps {
		renderStep(&b, s, 0, st)
	}
	renderFooter(&b, tr, st)
	return b.String()
}

// renderHeader prints the task, goal, and the trust headline. The trust glyph is
// the one place the chain verdict surfaces at the top: ✓ green only when
// verified, ✗ red plus the CHAIN BROKEN verdict otherwise.
func renderHeader(b *strings.Builder, tr *Trace, st termui.Style) {
	fmt.Fprintf(b, "%s %s\n", st.Bold("why:"), st.Bold(fence(tr.Task)))
	if tr.Goal != "" {
		fmt.Fprintf(b, "  %s %s\n", st.Dim("goal:"), fence(tr.Goal))
	}
	if tr.ChainVerified {
		fmt.Fprintf(b, "  %s chain verified — this trace is trustworthy\n", st.Success("✓"))
		fmt.Fprintf(b, "  %s %s\n", st.Dim("verdict:"), fence(tr.Verdict))
	} else {
		// Fail closed on trustworthiness: loud, red, unmistakable. Use the same
		// "CHAIN BROKEN" wording as the verdict const, the TUI header, and the
		// package doc-comments so the label does not diverge across surfaces.
		fmt.Fprintf(b, "  %s %s\n", st.Danger("✗"), st.Danger("CHAIN BROKEN"))
		fmt.Fprintf(b, "  %s %s\n", st.Danger("verdict:"), st.Danger(fence(tr.Verdict)))
	}
	b.WriteString("\n")
}

// renderStep prints one node and recurses into its children, indenting by depth.
// The line is: «Seq · kind · Title — Why», with the Why dimmed and an untrusted
// node prefixed by a red marker.
func renderStep(b *strings.Builder, s Step, depth int, st termui.Style) {
	indent := strings.Repeat("  ", depth+1)

	marker := stepGlyph(s, st)
	if s.Untrusted {
		// On a broken chain every node carries the doubt explicitly.
		marker = st.Danger("?")
	}

	line := fmt.Sprintf("%s%s %s · %s", indent, marker, st.Dim(fmt.Sprintf("#%d", s.Seq)), fence(s.Title))
	if s.Backend != "" {
		line += " " + st.Dim("["+fence(s.Backend)+"]")
	}
	if s.Why != "" {
		line += st.Dim(" — " + fence(s.Why))
	}
	b.WriteString(line)
	b.WriteString("\n")

	// Known-safe metadata, sorted for determinism, each value fenced.
	if len(s.Detail) > 0 {
		for _, k := range sortedKeys(s.Detail) {
			fmt.Fprintf(b, "%s    %s %s=%s\n", indent, st.Dim("·"), st.Dim(fence(k)), st.Dim(fence(s.Detail[k])))
		}
	}

	for _, child := range s.Children {
		renderStep(b, child, depth+1, st)
	}
}

// stepGlyph picks a glyph that mirrors the event's nature: green ✓ for a pass,
// red ✗ for a fail or block, amber for a gate, cyan ▸ otherwise. It reads only
// the harness-derived Title (metadata), never a raw body.
func stepGlyph(s Step, st termui.Style) string {
	t := strings.ToLower(s.Title)
	switch {
	case strings.Contains(t, "passed") || strings.Contains(t, "approved") ||
		strings.Contains(t, "integrated") || strings.Contains(t, "complete"):
		return st.Success("✓")
	case strings.Contains(t, "failed") || strings.Contains(t, "denied") ||
		strings.Contains(t, "blocked") || strings.Contains(t, "rollback") ||
		strings.Contains(t, "conflict"):
		return st.Danger("✗")
	case strings.Contains(t, "gate") || strings.Contains(t, "escalat") ||
		strings.Contains(t, "advisor"):
		return st.Warn("⚑")
	default:
		return st.Info("▸")
	}
}

// renderFooter prints the per-kind event tally in sorted order, so the reader
// can see the shape of the run at a glance and the output stays deterministic.
func renderFooter(b *strings.Builder, tr *Trace, st termui.Style) {
	if len(tr.Counts) == 0 {
		return
	}
	b.WriteString("\n")
	fmt.Fprintf(b, "%s\n", st.Dim("events:"))
	kinds := make([]string, 0, len(tr.Counts))
	for k := range tr.Counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(b, "  %s %s\n", st.Dim(fmt.Sprintf("%4d", tr.Counts[k])), st.Dim(fence(k)))
	}
}

// fence neutralizes a value before it is printed: it replaces every control
// character (so a smuggled newline cannot reshape the tree and an ESC cannot
// inject ANSI) with a space or a visible '?', then caps the length. This is the
// I7 safety net — even though only allowlisted metadata reaches Detail, anything
// that does reach the renderer is rendered as inert, single-line, bounded text.
func fence(s string) string {
	const max = 200
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			out.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Other control characters — including the ESC that starts an ANSI
			// sequence — become a visible '?', so they can neither inject colour
			// nor silently vanish.
			out.WriteByte('?')
		default:
			out.WriteRune(r)
		}
	}
	res := out.String()
	if rs := []rune(res); len(rs) > max {
		res = string(rs[:max]) + "…"
	}
	return res
}
