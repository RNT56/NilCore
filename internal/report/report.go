// Package report is the read-only verification-report projection (Phase 11,
// Pillar 6, P11-T30). It replays the append-only event log ONCE into a typed
// ReportModel, folds the persisted artifacts each artifact_verify event names,
// and calls eventlog.Verify so a broken chain can never render GREEN. It is a
// LEAF: it imports only eventlog + artifact + worktreefs + stdlib, never the
// orchestrator, inspect, termui, or any backend — so the model can be built and
// tested without pulling the whole tree, and the renderers (P11-T32) stay pure.
//
// Trust + invariants. The model is a pure read (I5): ReplayReport NEVER appends,
// mutates, or deletes an event — it only decodes. FinalPass is derived from the
// LOGGED verifier verdicts and the chain check, never from a backend self-report
// (I2): a green-looking log over a broken chain yields FinalPass=false. Artifact
// claim rows carry the model-authored Value/SourceURL fields verbatim as DATA
// (I7) — escaping/redaction for display is the renderer's job (P11-T32); this
// package only projects.
//
// Graceful degradation (auditor blocker). The requeue kinds (claim_requeue /
// claim_resolved / requeue_exhausted) and the enriched subagent_report
// continue_from are emitted by OTHER Phase-11 tasks that may not have run for a
// given log. A log lacking them still produces a valid, complete ReportModel —
// retry history is simply empty. That is why this task sits in wave 4 and is not
// gated behind the requeue waves.
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
)

// ReportModel is the typed projection of one run's verification evidence. It is
// the single value the renderers (P11-T32) consume, and the only thing the
// `nilcore report` subcommand (P11-T33) produces from a log.
type ReportModel struct {
	Run           string         // run/log identifier (the log path's base, sans extension)
	GeneratedAt   time.Time      // when this projection was built (wall clock)
	ChainVerified bool           // eventlog.Verify passed — the precondition for any GREEN
	Checks        []CheckResult  // every verify-family event, in log order
	Artifacts     []ArtifactView // one per artifact_verify event whose file folds back
	Retries       []RetryAttempt // requeue/continue-from history, ordered by Seq
	FinalPass     bool           // ChainVerified AND every relevant check passed

	// ClaimTraces is the flattened source→claim→verdict projection (SW-T06): one
	// row per folded claim across every artifact, tying a claim's verifier verdict
	// to its key-free SourceRef. It is populated in the SAME single log pass as
	// Artifacts (no extra file read) and is additive — existing consumers ignore it.
	ClaimTraces []ClaimTrace
	// SchemaDefects is the flattened schema_verify finding list (SW-T06): one row
	// per structural Defect carried in a schema_verify event's metadata. It records
	// shape failures the SchemaVerifier (SW-T01) found BEFORE any per-claim check
	// ran, so a malformed artifact surfaces even when no ClaimTrace exists.
	SchemaDefects []SchemaDefectRow
}

// ClaimTrace ties one claim's VERDICT to its key-free SOURCE in a flat row — the
// source-claim-trace projection (SW-T06). Status/Detail/Verifier are the trusted,
// verifier-set fields (I2); Value and Source.Locator are model-authored UNTRUSTED
// data carried verbatim (I7) — the RENDERER (RenderMatrix) escapes/redacts them,
// this struct only projects. Attempt threads the requeue pass so a re-verified
// claim's trace can be read in attempt order; it is 0 when no retry touched it.
type ClaimTrace struct {
	ArtifactID string
	ClaimID    string
	Field      string
	Value      string          // UNTRUSTED model-authored (I7) — renderer escapes
	Source     SourceRef       // key-free provenance (I3) — renderer redacts the Locator
	Verifier   string          // trusted: which check produced the verdict
	Status     artifact.Status // trusted verifier verdict (I2)
	Detail     string          // trusted verifier detail
	Attempt    int             // requeue pass that last touched this claim (0 = none)
}

// SourceRef is the key-free provenance of a claim's evidence. Locator is the
// SourceURL (required key-free by I3, but still redacted on render as defence in
// depth); RetrievedAt is when it was fetched; Resolved reports whether the source
// reached a decisive verdict (Status not unverified/unverifiable) so the matrix can
// flag an un-anchored claim without trusting a model string.
type SourceRef struct {
	Locator     string
	RetrievedAt time.Time
	Resolved    bool
}

// SchemaDefectRow is one structural finding from a schema_verify event — the
// metadata-only projection of a schema.Defect (SW-T01). Every field is
// HARNESS-authored (Code from the closed enum, Field/ClaimID are trusted location
// identifiers, Reason is the bounded harness-written explanation) — NO model field
// rides in it (I7), so it is safe to render and marshal verbatim.
type SchemaDefectRow struct {
	ArtifactID string
	ClaimID    string
	Field      string
	Code       string
	Reason     string
}

// CheckResult is one verify-family event projected to a pass/fail row. Family is
// the event Kind (verify, final_verify, …); Passed is decoded from that family's
// Detail shape; Output is a bounded human tail for the renderers.
type CheckResult struct {
	Family string
	Name   string
	Task   string
	Passed bool
	Stale  bool
	Output string
	Seq    uint64
	At     time.Time
}

// ArtifactView folds one persisted artifact (named by an artifact_verify event)
// into a display row with its per-claim breakdown. Green mirrors
// artifact.Green() exactly so the report agrees with the authoritative verdict.
type ArtifactView struct {
	ID     string
	Kind   artifact.Kind
	Title  string
	Green  bool
	Claims []ClaimRow
}

// ClaimRow is one claim of a folded artifact. Value/SourceURL are model-authored
// UNTRUSTED data carried verbatim (I7) — the renderer escapes/redacts them.
type ClaimRow struct {
	ClaimID     string
	Field       string
	Value       string
	SourceURL   string
	RetrievedAt time.Time
	Verifier    string
	Status      artifact.Status
	Detail      string
}

// RetryAttempt is one entry of requeue history, sourced primarily from the GRA
// claim_* kinds (attempt + claim_id) and secondarily from an enriched
// subagent_report continue_from. Ordered by Seq.
type RetryAttempt struct {
	Task         string
	ContinueFrom string
	BaseBranch   string
	Passed       bool
	Seq          uint64
	At           time.Time
}

// logEvent is this package's OWN decode struct (P11-T30 Note: do NOT widen or
// import inspect.Summary — its struct is depended on by inspect_test.go + the
// health probe). It mirrors only the eventlog.Event fields the projection reads.
type logEvent struct {
	Time    time.Time      `json:"time"`
	Seq     uint64         `json:"seq"`
	Task    string         `json:"task"`
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// verifyFamilies is the fixed set of event Kinds that project into a CheckResult.
// Each maps to a passed-predicate over its Detail (see passedFor). A Kind not in
// this set is ignored by the check projection (it may still feed retries).
var verifyFamilies = map[string]bool{
	"verify":               true,
	"final_verify":         true,
	"project_verify":       true,
	"project_acceptance":   true,
	"integration_verify":   true,
	"integration_rollback": true,
	"integration_conflict": true,
	"artifact_verify":      true,
}

// ReplayReport scans the append-only log at logPath ONCE, builds the typed model,
// folds every artifact an artifact_verify event names from worktreeRoot, and runs
// eventlog.Verify so the chain governs whether any GREEN is shown. A broken chain
// is NOT an error that hides the model: it returns a populated model with
// ChainVerified=false and FinalPass=false, so the renderer can show the RED
// banner. A genuinely unreadable log (missing file, unparseable line) IS an error.
func ReplayReport(logPath, worktreeRoot string) (*ReportModel, error) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, fmt.Errorf("report: read log %q: %w", logPath, err)
	}

	m := &ReportModel{
		Run:         runName(logPath),
		GeneratedAt: time.Now().UTC(),
	}

	// Decode the log line-by-line with our own struct. An empty log is valid
	// (no checks); a malformed line is a hard error — the projection must not
	// silently drop evidence.
	var events []logEvent
	for i, line := range nonEmptyLines(data) {
		var e logEvent
		if jerr := json.Unmarshal([]byte(line), &e); jerr != nil {
			return nil, fmt.Errorf("report: decode log %q line %d: %w", logPath, i+1, jerr)
		}
		events = append(events, e)
	}

	// Project checks, fold artifacts, gather retry history, AND project schema
	// defects + claim traces in ONE pass over the already-decoded events (the file
	// was read exactly once above). The claim traces are derived from the SAME
	// folded artifact view, so no extra read or second walk is incurred.
	seenArtifact := map[string]bool{}
	// claimAttempt records the highest requeue attempt number seen for each claim id
	// (from the GRA claim_* kinds, which carry {attempt, claim_id}). It is filled in
	// THIS pass and used to backfill ClaimTrace.Attempt below, so a re-verified
	// claim's trace reads in attempt order without a second walk or file read.
	claimAttempt := map[string]int{}
	for _, e := range events {
		if verifyFamilies[e.Kind] {
			m.Checks = append(m.Checks, checkFromEvent(e))
		}
		if e.Kind == "artifact_verify" {
			if id := stringDetail(e.Detail, "id"); id != "" && !seenArtifact[id] {
				seenArtifact[id] = true
				if av, ok := foldArtifact(worktreeRoot, id); ok {
					m.Artifacts = append(m.Artifacts, av)
					m.ClaimTraces = append(m.ClaimTraces, tracesFromView(av)...)
				}
			}
		}
		if e.Kind == schemaVerifyKind {
			m.SchemaDefects = append(m.SchemaDefects, schemaDefectsFromEvent(e)...)
		}
		if cid, att, ok := claimAttemptFromEvent(e); ok && att > claimAttempt[cid] {
			claimAttempt[cid] = att
		}
		if ra, ok := retryFromEvent(e); ok {
			m.Retries = append(m.Retries, ra)
		}
	}

	// Backfill each claim trace's Attempt from the per-claim attempt high-water mark
	// gathered above. A claim never requeued keeps Attempt 0.
	for i := range m.ClaimTraces {
		if att, ok := claimAttempt[m.ClaimTraces[i].ClaimID]; ok {
			m.ClaimTraces[i].Attempt = att
		}
	}

	// Retry history is ordered by Seq (the log's monotonic anchor) so the chain
	// reads in attempt order regardless of which kind contributed each entry.
	sort.SliceStable(m.Retries, func(i, j int) bool { return m.Retries[i].Seq < m.Retries[j].Seq })

	// The chain check is the gate for GREEN (I5). A broken chain leaves the model
	// intact but forces ChainVerified/FinalPass false rather than erroring out, so
	// the renderer can show evidence-with-a-warning instead of nothing.
	m.ChainVerified = eventlog.Verify(logPath) == nil
	m.FinalPass = m.ChainVerified && allChecksPassed(m.Checks)
	return m, nil
}

// checkFromEvent projects one verify-family event into a CheckResult, decoding the
// pass/fail verdict from that family's Detail shape (passedFor) and a bounded human
// tail (outputFor). Name defaults to the family Kind — the granular check name is
// not on the wire today, so the family IS the name.
func checkFromEvent(e logEvent) CheckResult {
	return CheckResult{
		Family: e.Kind,
		Name:   e.Kind,
		Task:   e.Task,
		Passed: passedFor(e),
		Stale:  false,
		Output: outputFor(e),
		Seq:    e.Seq,
		At:     e.Time,
	}
}

// passedFor decodes the pass verdict for a verify-family event from its Detail.
// The families carry the verdict in different shapes (audited at the emit sites):
//   - verify / final_verify / integration_verify: Detail["passed"] bool.
//   - project_verify: passed iff Detail["unmet"] == 0 (no unmet criteria remain).
//   - project_acceptance: bookkeeping (proposed/added/dropped) — always Passed
//     (it records criteria evolution, not a red/green gate).
//   - integration_rollback / integration_conflict: a failure by definition (a
//     merge that had to be reverted or could not be applied) ⇒ Passed=false.
//   - artifact_verify: passed iff Detail["green"] is true (the ArtifactVerifier's
//     own green; a fail/stale/unverifiable claim makes it false).
//
// Fail-closed: an absent or wrong-typed key reads as NOT passed (false), never an
// optimistic default — a verdict the log does not record is not a green.
func passedFor(e logEvent) bool {
	switch e.Kind {
	case "verify", "final_verify", "integration_verify":
		return boolDetail(e.Detail, "passed")
	case "project_verify":
		n, ok := intDetail(e.Detail, "unmet")
		return ok && n == 0
	case "project_acceptance":
		return true
	case "integration_rollback", "integration_conflict":
		return false
	case "artifact_verify":
		return boolDetail(e.Detail, "green")
	default:
		return false
	}
}

// outputFor renders a compact, deterministic tail of a check's Detail for display.
// It is a HINT for the human, not a parsed contract — the renderer redacts it.
func outputFor(e logEvent) string {
	if len(e.Detail) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e.Detail))
	for k := range e.Detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, e.Detail[k]))
	}
	return strings.Join(parts, " ")
}

// retryFromEvent extracts a retry-history entry from an event, if it is one. The
// PRIMARY source is the GRA claim_* kinds (claim_requeue / claim_resolved /
// requeue_exhausted), which carry attempt + claim_id. The SECONDARY source is an
// enriched subagent_report (P11-T17a) that additively adds continue_from — only
// THEN does it count as a retry. A plain subagent_report (no continue_from) is NOT
// a retry, so a log without the enrichment degrades to an empty retry list.
func retryFromEvent(e logEvent) (RetryAttempt, bool) {
	switch e.Kind {
	case "claim_requeue", "claim_resolved", "requeue_exhausted":
		ra := RetryAttempt{
			Task:         retryTask(e),
			ContinueFrom: stringDetail(e.Detail, "claim_id"),
			Passed:       e.Kind == "claim_resolved",
			Seq:          e.Seq,
			At:           e.Time,
		}
		return ra, true
	case "subagent_report":
		cf := stringDetail(e.Detail, "continue_from")
		if cf == "" {
			return RetryAttempt{}, false // un-enriched report: not a retry (graceful degradation)
		}
		return RetryAttempt{
			Task:         e.Task,
			ContinueFrom: cf,
			BaseBranch:   stringDetail(e.Detail, "base"),
			Passed:       boolDetail(e.Detail, "passed"),
			Seq:          e.Seq,
			At:           e.Time,
		}, true
	default:
		return RetryAttempt{}, false
	}
}

// retryTask names the task for a claim_* retry entry, preferring the event's Task
// and falling back to the claim_id so the row is never anonymous.
func retryTask(e logEvent) string {
	if e.Task != "" {
		return e.Task
	}
	return stringDetail(e.Detail, "claim_id")
}

// foldArtifact reads the artifact persisted for id from the worktree and projects
// it into an ArtifactView with one ClaimRow per claim. A missing or corrupt file
// is NOT fatal — the artifact_verify event is recorded but the file may not have
// been written or the worktree may be gone; the view is simply omitted (ok=false)
// so the rest of the model stands.
func foldArtifact(worktreeRoot, id string) (ArtifactView, bool) {
	if worktreeRoot == "" {
		return ArtifactView{}, false
	}
	a, err := artifact.Read(worktreeRoot, id)
	if err != nil || a == nil {
		return ArtifactView{}, false
	}
	rows := make([]ClaimRow, 0, len(a.Claims))
	for i := range a.Claims {
		c := a.Claims[i]
		rows = append(rows, ClaimRow{
			ClaimID:     c.ID,
			Field:       c.Field,
			Value:       c.Evidence.Value,
			SourceURL:   c.Evidence.SourceURL,
			RetrievedAt: c.Evidence.RetrievedAt,
			Verifier:    c.Evidence.Verifier,
			Status:      c.Evidence.Status,
			Detail:      c.Evidence.Detail,
		})
	}
	return ArtifactView{
		ID:     a.ID,
		Kind:   a.Kind,
		Title:  a.Title,
		Green:  a.Green(), // mirror the authoritative pure projection exactly
		Claims: rows,
	}, true
}

// schemaVerifyKind is the event Kind the SchemaVerifier (SW-T01) emits when the
// structural shape check runs. It MUST equal schema.EventKind on the wire; this
// package decodes the Detail rather than importing the struct so `report` stays a
// leaf whose only artifact-subtree dependency is the artifact package itself.
const schemaVerifyKind = "schema_verify"

// tracesFromView flattens one folded artifact view into per-claim ClaimTrace rows.
// It reuses the SAME ClaimRow data already projected by foldArtifact, so no second
// artifact read or walk happens. Value/Locator are carried verbatim (UNTRUSTED, I7)
// — the renderer escapes/redacts; this only projects. Resolved is derived from the
// trusted Status (a claim with a decisive verdict resolved its source), never from
// any model-authored string.
func tracesFromView(av ArtifactView) []ClaimTrace {
	out := make([]ClaimTrace, 0, len(av.Claims))
	for i := range av.Claims {
		c := av.Claims[i]
		out = append(out, ClaimTrace{
			ArtifactID: av.ID,
			ClaimID:    c.ClaimID,
			Field:      c.Field,
			Value:      c.Value,
			Source: SourceRef{
				Locator:     c.SourceURL,
				RetrievedAt: c.RetrievedAt,
				Resolved:    statusResolved(c.Status),
			},
			Verifier: c.Verifier,
			Status:   c.Status,
			Detail:   c.Detail,
		})
	}
	return out
}

// statusResolved reports whether a claim's verifier verdict is decisive — i.e. the
// source was actually reached and judged. StatusPass/StatusFail/StatusStale are
// decisive (the source resolved, even if it failed a freshness/equality check);
// StatusUnverified (never checked) and StatusUnverifiable (no decisive verdict) are
// NOT resolved. This is a pure function of the trusted Status (I2), never a model
// string.
func statusResolved(s artifact.Status) bool {
	switch s {
	case artifact.StatusPass, artifact.StatusFail, artifact.StatusStale:
		return true
	default:
		return false
	}
}

// schemaDefectsFromEvent decodes the metadata-only schema_verify event Detail into
// SchemaDefectRow rows. The on-wire shape is defined defensively here (the cmd-layer
// sink that serializes schema.SchemaVerifyEvent is owned elsewhere): the artifact id
// at Detail["id"], and a "defects" list whose entries carry {code, field, claim_id,
// reason}. Every decoded field is harness-authored (Code from the closed enum, a
// location identifier, a bounded harness Reason) — no model field rides in it (I7).
// A passing schema check (no defects) yields zero rows. An absent/malformed defects
// list yields zero rows rather than an error: a log without the SW-T01 enrichment
// degrades gracefully to an empty SchemaDefects slice.
func schemaDefectsFromEvent(e logEvent) []SchemaDefectRow {
	id := stringDetail(e.Detail, "id")
	raw, ok := e.Detail["defects"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make([]SchemaDefectRow, 0, len(raw))
	for _, item := range raw {
		d, ok := item.(map[string]any)
		if !ok {
			continue // a non-object entry is dropped, not fatal (defensive decode)
		}
		out = append(out, SchemaDefectRow{
			ArtifactID: id,
			ClaimID:    stringDetail(d, "claim_id"),
			Field:      stringDetail(d, "field"),
			Code:       stringDetail(d, "code"),
			Reason:     stringDetail(d, "reason"),
		})
	}
	return out
}

// claimAttemptFromEvent extracts a (claim_id, attempt) pair from a GRA claim_* event
// so ReplayReport can stamp each ClaimTrace with the requeue pass that last touched
// it. Only the claim_* kinds carry attempt + claim_id; any other event returns
// ok=false. An absent/wrong-typed attempt reads as ok=false (the trace keeps
// Attempt 0), never an optimistic default.
func claimAttemptFromEvent(e logEvent) (string, int, bool) {
	switch e.Kind {
	case "claim_requeue", "claim_resolved", "requeue_exhausted":
		cid := stringDetail(e.Detail, "claim_id")
		att, ok := intDetail(e.Detail, "attempt")
		if cid == "" || !ok {
			return "", 0, false
		}
		return cid, att, true
	default:
		return "", 0, false
	}
}

// allChecksPassed reports whether every projected check passed. An empty check set
// is NOT a pass (fail-closed): a run that recorded no verifier verdict has not
// earned a green. This is the second half of the FinalPass gate (the first is
// ChainVerified).
func allChecksPassed(checks []CheckResult) bool {
	if len(checks) == 0 {
		return false
	}
	for i := range checks {
		if !checks[i].Passed {
			return false
		}
	}
	return true
}

// --- Detail decode helpers (defensive: a wrong-typed value is treated as absent) ---

// boolDetail reads a bool Detail value, tolerating the JSON-number / string forms
// a round-tripped log might carry. Absent or non-truthy ⇒ false (fail-closed).
func boolDetail(d map[string]any, key string) bool {
	switch v := d[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}

// intDetail reads an integer Detail value. JSON decodes numbers as float64, so we
// accept that plus the native int forms. ok=false when the key is absent or not a
// number, so the caller can distinguish "0" from "missing".
func intDetail(d map[string]any, key string) (int, bool) {
	switch v := d[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

// stringDetail reads a string Detail value; absent or non-string ⇒ "".
func stringDetail(d map[string]any, key string) string {
	if s, ok := d[key].(string); ok {
		return s
	}
	return ""
}

// nonEmptyLines splits the log file into its JSONL records, dropping the trailing
// newline and any blank lines so an empty/whitespace log yields no events.
func nonEmptyLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	raw := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := raw[:0]
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// runName derives the run identifier from the log path's base name, stripping a
// trailing extension (e.g. "/tmp/run-7.jsonl" ⇒ "run-7"). It is display metadata
// only — never a path used for I/O.
func runName(logPath string) string {
	base := logPath
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return base
}
