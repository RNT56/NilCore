// Package trace reconstructs a causal, navigable "why did the agent do that"
// view of a single run from the append-only event log (Phase 13). It is a
// READ-ONLY leaf: it replays the JSONL log the eventlog package wrote, never
// mutates it, and depends only on eventlog (for the on-disk shape and the chain
// authority), termui (for rendering), and the standard library.
//
// Two invariants shape this package and must never be relaxed:
//
//   - I5 (trustworthy "why" needs an intact chain): a tampered log has no
//     trustworthy story. Build runs eventlog.Verify and, on a broken chain,
//     still assembles the STRUCTURAL trace so an operator can debug — but marks
//     every node UNTRUSTED and sets a loud "CHAIN BROKEN" Verdict. We fail
//     closed on trustworthiness; we never silently render a tampered log as if
//     it were clean.
//
//   - I7 (metadata only): the event Detail the harness writes is metadata — ids,
//     counts, kinds, lengths, exit codes, pass/fail flags — never raw model or
//     tool bodies. The trace projects ONLY known-safe fields into Step.Detail
//     and the rendered "Why" annotations are harness-authored (see why.go). We
//     never echo untrusted free text into the causal story, and the renderer
//     fences any value it does surface so a body that leaked into a Detail field
//     upstream still cannot break out of its cell.
//
// The shape: Build groups the raw event stream into a tree of Steps. Consecutive
// model_call -> tool_exec(s) collapse into one "step" node; verify, gate,
// advisor, race, and integrate events attach as their own nodes, each annotated
// with a causal Why derived from the surrounding context (why.go). Render
// (render.go) prints that tree; render_tui.go (//go:build tui) explores it
// interactively, linking ZERO Charm into the default binary.
package trace

import "time"

// Step is one node in the causal tree. It is a projection of one or more raw
// events into a human-legible, metadata-only shape.
//
//   - Title is a harness-derived label for WHAT happened ("ran tool: edit",
//     "verify FAILED", "escalated to advisor", "integrated branch task/x").
//   - Why is a harness-derived causal annotation for WHY it happened ("after 2
//     consecutive verify failures", "because the build was red", "to recover the
//     failed task"). It is empty when no causal link is known — we never invent
//     a story we cannot ground in the surrounding events.
//   - Detail carries only known-safe metadata fields (step, phase, counts, ids,
//     exit codes, pass/fail). It is NEVER a raw model or tool body (I7).
//   - Untrusted is set on every node when the chain failed to verify, so the
//     renderer can flag the whole tree as not-to-be-trusted (I5).
//   - Children are nested steps (tool execs under a model call, sub-outcomes
//     under a race cluster).
type Step struct {
	Seq       uint64
	Time      time.Time
	Kind      string // the originating event kind (or a synthetic group kind)
	Backend   string
	Title     string
	Why       string
	Detail    map[string]string
	Untrusted bool
	Children  []Step
}

// Trace is the causal view of one task's run.
//
//   - Verdict is a one-line headline of how the run ended. It is a CLEAN
//     headline only when ChainVerified; on a broken chain it is the loud
//     "CHAIN BROKEN" string (see brokenChainVerdict), regardless of structure.
//   - ChainVerified records eventlog.Verify's verdict on the whole log. It
//     gates every trustworthiness signal in render.go.
//   - Counts is the per-kind event tally for this task, for the footer.
type Trace struct {
	Task          string
	Goal          string
	Steps         []Step
	Verdict       string
	ChainVerified bool
	Counts        map[string]int
}

// brokenChainVerdict is the single, loud verdict shown whenever the hash chain
// did not verify. A trace over a tampered log is structurally useful for
// debugging but carries no trustworthy "why", so we never dress it up as a clean
// outcome (I5).
const brokenChainVerdict = "CHAIN BROKEN — this trace is not trustworthy (the event log failed hash-chain verification; structure shown for debugging only)"
