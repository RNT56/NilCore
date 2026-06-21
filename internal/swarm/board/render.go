package board

// render.go is the PURE scoreboard renderer: a Snapshot in, a string out, with all
// colour gated behind termui.Style so an off Style (a pipe, CI, a dumb terminal) emits
// ZERO ANSI — the same I6 SSH/CI guarantee every other NilCore renderer makes. It is a
// projection: it reads only the Snapshot's metadata counts and TRUSTED per-shard fields
// (Status/Detail/SourceURL — never a model-authored Value, I7). The clean-pass headline
// is shown ONLY when the snapshot's FinalCleanPass is set, so a green banner can never
// appear over an unverified run (I2).

import (
	"fmt"
	"strings"
	"time"

	"nilcore/internal/termui"
)

// RenderScoreboard renders the live scoreboard for snap to a string. It is pure (no
// I/O, no clock read beyond the snapshot's own captured timers) and deterministic for a
// given Snapshot. With an OFF Style it emits zero ANSI; with an ON Style the headline,
// the counts, and each shard row are tinted by verdict — but a green tint is only ever
// applied to a genuine pass (I2). Layout: a headline, a counts banner, a cost/time/token
// line, then the per-shard table.
func RenderScoreboard(snap Snapshot, st termui.Style) string {
	var b strings.Builder

	// Headline: the clean-pass banner ONLY when the snapshot says the swarm converged
	// green (FinalCleanPass). Otherwise an explicit "in progress / not clean" line — a
	// green headline never appears without the verified gate (I2/I5).
	if snap.FinalCleanPass {
		b.WriteString(st.Success("✔ swarm clean — every shard passed") + "\n")
	} else {
		b.WriteString(st.Warn(fmt.Sprintf("• swarm pass %d — %d/%d remaining", snap.Pass, snap.Remaining, snap.Total)) + "\n")
	}

	// Counts banner: the six headline tallies for the current pass. Passed is tinted
	// Success, a non-zero Failed Danger, RetryPass Info (a recovered shard is good news
	// but worth flagging) — all no-ops on an off Style.
	counts := []string{
		fmt.Sprintf("checked %d", snap.Checked),
		st.Success(fmt.Sprintf("passed %d", snap.Passed)),
		paintCount(st, snap.Failed, st.Danger, "failed"),
		paintCount(st, snap.RetryPass, st.Info, "retry-pass"),
		fmt.Sprintf("remaining %d", snap.Remaining),
	}
	b.WriteString(strings.Join(counts, "  ") + "\n")

	// Cost / time / token line: the live ledger cost, total wall-clock, and summed
	// tokens. The cost is the ledger's authority (Snapshot.Cost), never a Board-side
	// accumulation. Per-model token splits follow on the same line when present.
	meta := []string{
		fmt.Sprintf("cost $%.4f", snap.Cost),
		fmt.Sprintf("time %s", humanDuration(snap.RunElapsed)),
		fmt.Sprintf("tokens %d", snap.Tokens),
	}
	b.WriteString(st.Dim(strings.Join(meta, " · ")) + "\n")
	for _, m := range snap.Models {
		b.WriteString(st.Dim(fmt.Sprintf("    %s  in %d / out %d  ~$%.4f", m.Model, m.In, m.Out, m.Dollars)) + "\n")
	}

	// Per-shard table: one row per shard, in the snapshot's id order. The verdict glyph
	// + tint reflect the TRUSTED Status (a non-pass is never green); the SourceURL is the
	// key-free provenance (I3); the Detail is the verifier's bounded tail. No
	// model-authored Value is shown (I7) — the Snapshot does not carry one.
	if len(snap.Shards) > 0 {
		b.WriteString("\n")
		for _, r := range snap.Shards {
			b.WriteString(renderShardRow(r, st) + "\n")
		}
	}

	return b.String()
}

// renderShardRow renders one shard's line: a verdict glyph, the shard id, its pass, its
// per-shard elapsed, the verifier id, and the key-free SourceURL. A passing shard is
// tinted Success; a failing/exhausted one Danger — never a green tint over a non-pass
// (I2). All fields are trusted metadata; no model Value rides here (I7).
func renderShardRow(r ShardRow, st termui.Style) string {
	glyph, paint := "✘", st.Danger
	if r.Passed {
		glyph, paint = "✔", st.Success
	}
	tag := ""
	if r.Exhausted {
		tag = " (exhausted)"
	}
	head := paint(fmt.Sprintf("  %s %s%s", glyph, r.ID, tag))

	var parts []string
	parts = append(parts, fmt.Sprintf("pass %d", r.Pass))
	if r.Elapsed > 0 {
		parts = append(parts, humanDuration(r.Elapsed))
	}
	// Status/Verifier/SourceURL are TRUSTED (verifier-set + key-free by I3), but a
	// verifier Detail tail or a smuggled query param could still carry a key-shaped
	// substring or a control byte — so route each through the same redactor+sanitizer the
	// matrix renderer uses before it reaches the row (MINOR #12): secret-mask THEN
	// control/markup-escape, so neither a leaked key nor an ANSI repaint survives.
	if r.Status != "" {
		parts = append(parts, "status="+safeField(r.Status))
	}
	if r.Verifier != "" {
		parts = append(parts, "by="+safeField(r.Verifier))
	}
	if r.SourceURL != "" {
		parts = append(parts, "src="+safeSource(r.SourceURL))
	}
	return head + "  " + st.Dim(strings.Join(parts, " · "))
}

// paintCount renders "label N", tinting it with paint only when N is non-zero — a zero
// failed/retry count stays plain so the banner does not shout about nothing. On an off
// Style paint is a no-op, so this is still zero-ANSI.
func paintCount(st termui.Style, n int, paint func(string) string, label string) string {
	s := fmt.Sprintf("%s %d", label, n)
	if n == 0 {
		return s
	}
	return paint(s)
}

// humanDuration formats a duration compactly for the time line: sub-minute as "12s",
// longer as "3m04s". It mirrors termui's own humanDuration shape so the scoreboard reads
// consistently with the chat front door. Stdlib only.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
