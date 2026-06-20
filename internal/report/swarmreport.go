package report

// swarmreport.go is the SWARM-dimension projection (Phase 12, SW-T06). It is the
// ADDITIVE sibling of report.go's per-run ReportModel: SwarmReport wraps a Base
// *ReportModel (the verifier-evidence projection every renderer already consumes)
// and adds a Swarm dimension folded from the swarm-only event Kinds the scoreboard
// (SW-T14) and the multi-pass controller (SW-T13) emit. It reuses ReplayReport for
// Base so the chain check, the artifact fold, and the claim traces are computed
// exactly once by the authoritative path, and folds the swarm Kinds in a single
// extra pass over the same log.
//
// Trust + invariants. The Swarm dimension is METADATA ONLY: it carries pass/fail
// COUNTS and a clean-pass flag, never a model-authored Value/SourceURL (those live,
// fenced, on the Base claim traces and are the renderer's to escape — I7). The clean
// gate is the union of three trusted signals — the Base hash chain verified, a
// swarm_pass_clean event is present, and the final remaining count is zero — so a
// broken chain forces BOTH Base.FinalPass=false (ReplayReport's gate) AND
// Swarm.FinalCleanPass=false (I2/I5): a green scoreboard over a tampered log can
// never read as a clean swarm.
//
// Graceful degradation. The swarm Kinds are emitted by OTHER Phase-12 tasks that may
// not have run for a given log (e.g. a plain single-agent run). A log lacking them
// still yields a valid SwarmReport whose Swarm dimension is the zero value — the
// counts are simply 0 and FinalCleanPass is false. The decode is defensive: the
// on-wire Detail shape is read key-by-key, so a missing or wrong-typed field reads as
// absent rather than panicking or erroring.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// SwarmReport is the SwarmReport-dimension projection of one swarm run: the per-run
// verifier-evidence Base plus the swarm-only Swarm dimension. It is what the swarm
// renderers (RenderMatrix here, the scoreboard in SW-T14) and the `nilcore report`
// swarm path (SW-T16) consume. Base is a pointer so the swarm path and the plain
// report path share one model value.
type SwarmReport struct {
	Base  *ReportModel
	Swarm SwarmDimension
}

// SwarmDimension is the metadata-only swarm scoreboard projection. The integer
// counts mirror the live Board.Scoreboard (SW-T14) so the keystone live==replay test
// can compare field-by-field: Checked/Passed/Failed/RetryPass/Remaining are the
// FINAL pass's tallies, Pass is the final pass number. FinalCleanPass is the swarm
// green gate (chain verified AND a clean-pass event present AND zero remaining).
// PassRows is one row per scoreboard snapshot in pass order, for the per-pass history.
type SwarmDimension struct {
	Checked   int
	Passed    int
	Failed    int
	RetryPass int
	Remaining int
	Pass      int

	FinalCleanPass bool
	PassRows       []PassRow
}

// PassRow is one scoreboard snapshot — the tally at the end of a single swarm pass.
// It is metadata only (counts, no model field), so it is safe to render and marshal
// verbatim.
type PassRow struct {
	Pass      int
	Checked   int
	Passed    int
	Failed    int
	RetryPass int
	Remaining int
}

// swarm event Kinds. These are FREE STRINGS the scoreboard/controller emit (no
// schema change, I5/I6). report decodes their metadata-only Detail rather than
// importing internal/swarm* — keeping `report` a leaf that never reaches the
// orchestrator side. The set mirrors SW-T14's kinds.go; only the two the swarm
// dimension folds (the per-pass snapshot and the clean-pass signal) are named here.
const (
	// scoreboardSnapshotKind carries one pass's Scoreboard tally
	// ({pass, checked, passed, failed, retry_pass, remaining}). The LAST snapshot
	// is the swarm's final state; each one becomes a PassRow.
	scoreboardSnapshotKind = "scoreboard_snapshot"
	// swarmPassCleanKind is emitted exactly when a pass converged with an empty
	// worklist on a verified chain (the controller's MarkClean gate). Its PRESENCE
	// is the second leg of FinalCleanPass; its Detail is metadata only.
	swarmPassCleanKind = "swarm_pass_clean"
)

// ReplaySwarmReport builds a SwarmReport from the log at logPath. It reuses
// ReplayReport for the Base model (the one authoritative read of the verifier
// evidence + the chain check + the claim traces) and folds the swarm-only event
// Kinds for the Swarm dimension in a single extra pass over the same log bytes. A
// broken chain is NOT an error that hides the model: ReplayReport returns a populated
// Base with FinalPass=false, and this function forces FinalCleanPass=false to match.
// A genuinely unreadable log IS an error (surfaced by either read).
func ReplaySwarmReport(logPath, worktreeRoot string) (*SwarmReport, error) {
	base, err := ReplayReport(logPath, worktreeRoot)
	if err != nil {
		return nil, err
	}

	// Read the log once more for the swarm fold. ReplayReport owns the Base read;
	// this pass decodes only the swarm Kinds, so the two concerns stay separable and
	// the Base path is untouched. A read error here cannot occur in practice (the
	// Base read already succeeded), but we surface it rather than swallow it.
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, fmt.Errorf("report: read swarm log %q: %w", logPath, err)
	}

	sr := &SwarmReport{Base: base}
	cleanSeen := false
	for i, line := range nonEmptyLines(data) {
		var e logEvent
		if jerr := json.Unmarshal([]byte(line), &e); jerr != nil {
			return nil, fmt.Errorf("report: decode swarm log %q line %d: %w", logPath, i+1, jerr)
		}
		switch e.Kind {
		case scoreboardSnapshotKind:
			sr.Swarm.PassRows = append(sr.Swarm.PassRows, passRowFromEvent(e))
		case swarmPassCleanKind:
			cleanSeen = true
		}
	}

	// PassRows are ordered by their pass number so the history reads in pass order
	// regardless of log interleaving; the LAST row is the final tally that drives the
	// dimension's headline counts.
	sort.SliceStable(sr.Swarm.PassRows, func(i, j int) bool {
		return sr.Swarm.PassRows[i].Pass < sr.Swarm.PassRows[j].Pass
	})
	if n := len(sr.Swarm.PassRows); n > 0 {
		last := sr.Swarm.PassRows[n-1]
		sr.Swarm.Pass = last.Pass
		sr.Swarm.Checked = last.Checked
		sr.Swarm.Passed = last.Passed
		sr.Swarm.Failed = last.Failed
		sr.Swarm.RetryPass = last.RetryPass
		sr.Swarm.Remaining = last.Remaining
	}

	// The swarm green gate (I2/I5): the chain must have verified, a clean-pass event
	// must be present, AND the final pass must leave zero remaining work. A broken
	// chain (Base.ChainVerified=false) forces this false, matching Base.FinalPass.
	sr.Swarm.FinalCleanPass = base.ChainVerified && cleanSeen && sr.Swarm.Remaining == 0
	return sr, nil
}

// passRowFromEvent decodes one scoreboard_snapshot event's metadata-only Detail into
// a PassRow. The on-wire shape is defined defensively here (SW-T14 owns the emit):
// {pass, checked, passed, failed, retry_pass, remaining}. An absent/wrong-typed key
// reads as 0 (intDetail's fail-closed default) — a snapshot the log did not record a
// count for contributes nothing rather than a guess.
func passRowFromEvent(e logEvent) PassRow {
	return PassRow{
		Pass:      intOrZero(e, "pass"),
		Checked:   intOrZero(e, "checked"),
		Passed:    intOrZero(e, "passed"),
		Failed:    intOrZero(e, "failed"),
		RetryPass: intOrZero(e, "retry_pass"),
		Remaining: intOrZero(e, "remaining"),
	}
}

// intOrZero reads an integer Detail value, returning 0 when it is absent or not a
// number. It wraps intDetail to keep passRowFromEvent terse — every count is
// fail-closed to 0, never an optimistic default.
func intOrZero(e logEvent, key string) int {
	n, _ := intDetail(e.Detail, key)
	return n
}
