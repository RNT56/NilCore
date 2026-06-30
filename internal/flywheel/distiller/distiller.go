// Package distiller is the self-improvement flywheel's failure-pattern miner
// (Phase 16, docs/ROADMAP-CLOSED-LOOP.md Pillar 4, SIF-T03). It replays the
// append-only event log read-only and clusters RECURRING verifier-failure
// patterns into candidate IMPROVEMENT TARGETS the flywheel can later aim a
// prompt/skill fix at. It is a sibling of internal/memory/lessons — that leaf
// writes deduped memory RECORDS for the next same-class task; this leaf yields
// the flywheel's targeting signal — but both obey the same two disciplines.
//
// Two invariants shape every line:
//
//   - I2: the verifier is the sole authority on "done". A pattern is folded ONLY
//     from a verifier verdict — a verify-family event whose Detail["passed"] is
//     the JSON bool false. No backend self-report (Result.SelfClaimed) ever
//     contributes, and nothing here marks work done, ships an edit, or skips a
//     verify: a Pattern is an INPUT to "what should we try to improve", never to
//     the ship/no-ship decision.
//   - I5: the append-only event log is the sole source of truth. This package
//     never mutates it; it only REPLAYS it read-only, and runs eventlog.Verify
//     LAST, failing closed on a broken chain (tampered, reordered, dropped, or
//     corrupt) by returning the verifier's error and no patterns — so no
//     improvement target is ever distilled from forged evidence, exactly like
//     trust.Replay (internal/trust/replay.go:78).
//
// I7 — untrusted input is data, never instructions: a verifier-failure event's
// Detail can carry attacker-influenced text (a failing command's stdout, a model
// turn). A Pattern therefore templates STRUCTURAL fields ONLY (the verifier id,
// a coarse failure class, a backend label, counts, timestamps) and NEVER copies
// raw model/tool output. The flywheel can target a fix from the structure; it
// must read the raw scar (if ever) through a separate, explicitly-untrusted path.
//
// The package is a stdlib + eventlog leaf and imports no orchestrator
// (deps_test.go enforces this).
package distiller

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"nilcore/internal/eventlog"
)

// Kind is the improvement-target kind carried by every Pattern this miner emits.
// It is a fixed, structural label (never derived from log content), so a Pattern
// is self-describing without quoting any untrusted text.
const Kind = "verifier-failure"

// DefaultThreshold is the minimum cluster Count for a Pattern to be reported.
// One-off failures are noise — a flaky run, a transient outage — and are not an
// improvement target; the flywheel earns a target only from a RECURRING scar.
// Distill uses this when called with threshold <= 0.
const DefaultThreshold = 2

// failClassUnknown labels a verifier failure whose enrichment fields are absent
// (the log predates LRN-T01's verifier_id/fail_class enrichment). Such failures
// still cluster — by their structural (verifier, backend) coordinates — so the
// miner is useful today and only sharpens as the log gains richer structure.
const failClassUnknown = "unknown"

// Pattern is one clustered, recurring verifier-failure — a candidate improvement
// target for the self-improvement flywheel. Every field is STRUCTURAL (I7): it
// carries enough to later aim a prompt/skill fix (which verifier, which coarse
// failure class, which backend, how often, how recently) and DELIBERATELY no raw
// model/tool output, no failing command text, and no attacker-influenced string.
type Pattern struct {
	// Kind is the improvement-target kind. Always the package Kind constant.
	Kind string
	// VerifierID identifies the verifier-of-record whose check is failing — the
	// target a prompt/skill fix should make pass. It is taken from the log's
	// structural enrichment (Detail["verifier_id"], else a verify-family event
	// kind / backend coordinate), never from free-text output.
	VerifierID string
	// FailClass is a coarse, structural failure bucket (e.g. an enrichment
	// Detail["fail_class"] such as "build" / "test" / "lint"), or
	// failClassUnknown when the log carries no class. It is matched as data.
	FailClass string
	// Backend is the backend label that produced the failing attempt (the native
	// loop, codex, claude-code), or "" if the event carried none. Structural.
	Backend string
	// Count is how many verifier FAILURES clustered into this pattern — the
	// recurrence strength that cleared the threshold.
	Count int
	// Sample is how many verifier VERDICTS (pass or fail) were observed for this
	// same (VerifierID, FailClass, Backend) coordinate. Count/Sample is the
	// failure rate; a high Sample with a low Count is a flaky check, a low Sample
	// with a high Count is a consistently-broken one — the flywheel can weigh
	// targets by this without ever reading raw output.
	Sample int
	// FirstSeen and LastSeen bound the recurrence in time, so the flywheel can
	// prefer a still-active scar over a long-dormant one. Zero if untimed.
	FirstSeen time.Time
	LastSeen  time.Time
}

// FailRate reports Count/Sample (0 when Sample is 0), the share of observed
// verdicts for this coordinate that the verifier failed.
func (p Pattern) FailRate() float64 {
	if p.Sample <= 0 {
		return 0
	}
	return float64(p.Count) / float64(p.Sample)
}

// verifyEvent mirrors only the fields of an on-disk eventlog.Event a
// verifier-failure pattern is built from. Chain integrity is eventlog.Verify's
// job, not this decoder's, so seq/prev/hash are intentionally ignored, and the
// raw output channels of Detail (Detail["output"], a model turn) are NEVER read
// here — only structural keys, per I7.
type verifyEvent struct {
	Time    time.Time      `json:"time"`
	Task    string         `json:"task"`
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// isVerifyKind reports whether an event kind is a verifier verdict this miner
// folds. These are the verify-family events the orchestrator, native loop, and
// project loop emit (orchestrator.go:343 "final_verify", native.go:926 "verify",
// project.go:242 "project_verify") plus the race verdict (route.go:61). Each
// carries the verifier's Detail["passed"] bool — the I2 source of truth — so a
// failure is unambiguously a VERIFIER failure, never a backend's self-claim.
func isVerifyKind(kind string) bool {
	switch kind {
	case "verify", "final_verify", "project_verify", "race_outcome":
		return true
	default:
		return false
	}
}

// cluster accumulates the verdicts for one structural coordinate as the log is
// scanned. Only structural fields are retained.
type cluster struct {
	verifierID string
	failClass  string
	backend    string
	fails      int
	sample     int
	first      time.Time
	last       time.Time
}

// clusterKey is the structural identity a verdict folds into: (verifier, failure
// class, backend). It is built ONLY from structural enrichment, never from raw
// output, so two failures of the same check coalesce into one improvement target.
type clusterKey struct {
	verifierID string
	failClass  string
	backend    string
}

// keyFor derives the structural cluster coordinate from an event. It prefers the
// LRN-T01 enrichment keys (Detail["verifier_id"], Detail["fail_class"]) when
// present and falls back to the event's own structural coordinates (kind as the
// verifier id, "unknown" failure class) for a log that predates enrichment. It
// reads only strings that name a check — never a free-text output channel (I7).
func keyFor(e verifyEvent) clusterKey {
	vid := stringDetail(e.Detail, "verifier_id")
	if vid == "" {
		// No enrichment: the event kind is the coarsest structural verifier
		// identity available (e.g. "build" failed its "project_verify").
		vid = e.Kind
	}
	fc := stringDetail(e.Detail, "fail_class")
	if fc == "" {
		fc = failClassUnknown
	}
	return clusterKey{verifierID: vid, failClass: fc, backend: e.Backend}
}

// stringDetail reads a structural string field from an untyped Detail map. A
// missing or non-string value reads as absent. This is the ONLY shape of Detail
// value the miner ever lifts into a Pattern: a short structural label, never a
// raw output blob (I7).
func stringDetail(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	s, _ := d[key].(string)
	return s
}

// Distill replays the append-only event log at logPath READ-ONLY, clusters every
// recurring verifier FAILURE into structural improvement-target Patterns, then —
// and only then — runs eventlog.Verify on the same file. It returns patterns
// sorted by descending Count (then VerifierID for a stable order).
//
// threshold is the minimum Count for a cluster to surface; threshold <= 0 uses
// DefaultThreshold. A cluster below the threshold is a one-off and is dropped:
// the flywheel earns an improvement target only from a RECURRING scar.
//
// Fail-closed (I5): a broken hash chain returns eventlog.Verify's error and NIL
// patterns — no target is ever distilled from a tampered log, so tampering can
// only ERASE a scar (by failing the whole replay), never forge one. A MISSING
// log is a clean empty result (nil error): a fresh install has no scars yet.
// Only an EXISTING but unreadable/broken log errors.
//
// I2: only a verify-family event whose Detail["passed"] is the JSON bool false
// folds as a failure; a missing or non-bool "passed" reads as a non-failure
// (absent evidence is never a scar). No Result.SelfClaimed is consulted.
func Distill(logPath string, threshold int) ([]Pattern, error) {
	return DistillAcross(threshold, logPath)
}

// DistillAcross is Distill over MORE THAN ONE log generation: it clusters the
// recurring verifier failures across every given path into one set of Patterns,
// so a scar that straddles a log rotation (e.g. the live `events.jsonl` plus the
// rotated `events.jsonl.1`) still clears the recurrence threshold instead of
// resetting its Count at the rotation boundary (the B5-autonomy.8 fix —
// maint.RotateLog renames the bulky live log to path+".1" and starts a fresh
// genesis chain, which the single-generation Distill could not see).
//
// Each generation is an INDEPENDENT hash chain (rotation creates a fresh genesis,
// so seq/prev restart at 0/""). So each path is chain-verified ON ITS OWN and
// FAILS CLOSED per file: if ANY existing generation's chain does not link,
// DistillAcross returns that file's error and NIL patterns — a tampered or corrupt
// generation can only ERASE the whole result, never forge a scar (I5), exactly
// like the single-file Distill. A MISSING generation is skipped cleanly (a not-yet
// rotated host has no ".1"); only an EXISTING but unreadable/broken one errors.
//
// Paths are folded in the order given and clusters MERGE across them, so the
// caller passes newest-first or oldest-first freely — the Pattern's FirstSeen /
// LastSeen still bound the true recurrence window because they min/max over every
// folded event regardless of file order. Passing a single path is byte-identical
// to the old Distill.
func DistillAcross(threshold int, logPaths ...string) ([]Pattern, error) {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}

	clusters := map[clusterKey]*cluster{}
	for _, logPath := range logPaths {
		if logPath == "" {
			continue
		}
		if err := scanGeneration(logPath, clusters); err != nil {
			// Fail-closed per generation: a missing file is skipped inside
			// scanGeneration (returns nil); any real error (parse, I/O, broken
			// chain) drops EVERYTHING and surfaces, so no target is distilled from
			// a tampered or unreadable log.
			return nil, err
		}
	}

	// A nil (not allocated-empty) result when nothing clears the threshold keeps the
	// single-path Distill contract: a missing/empty/all-one-off log yields nil, never
	// a non-nil zero-length slice.
	var patterns []Pattern
	for _, c := range clusters {
		if c.fails < threshold {
			continue // a one-off (sub-threshold) scar is not an improvement target
		}
		patterns = append(patterns, Pattern{
			Kind:       Kind,
			VerifierID: c.verifierID,
			FailClass:  c.failClass,
			Backend:    c.backend,
			Count:      c.fails,
			Sample:     c.sample,
			FirstSeen:  c.first,
			LastSeen:   c.last,
		})
	}
	// Deterministic order: strongest recurrence first, then a stable tiebreak on
	// the structural coordinate so callers and golden tests see a fixed sequence.
	sort.Slice(patterns, func(i, j int) bool {
		if patterns[i].Count != patterns[j].Count {
			return patterns[i].Count > patterns[j].Count
		}
		if patterns[i].VerifierID != patterns[j].VerifierID {
			return patterns[i].VerifierID < patterns[j].VerifierID
		}
		if patterns[i].FailClass != patterns[j].FailClass {
			return patterns[i].FailClass < patterns[j].FailClass
		}
		return patterns[i].Backend < patterns[j].Backend
	})
	return patterns, nil
}

// scanGeneration replays ONE log generation read-only, folding its verifier
// verdicts into the shared clusters map, then runs eventlog.Verify on that same
// file LAST and fails closed on a broken chain (dropping nothing into the map is
// the caller's concern — on a chain error the whole DistillAcross returns nil
// patterns, so a partially-folded map is never returned). A MISSING file is a
// clean no-op (a fresh install or a host with no rotated generation yet).
func scanGeneration(logPath string, clusters map[clusterKey]*cluster) error {
	f, err := os.Open(logPath)
	if err != nil {
		// No such generation ⇒ nothing to fold (a fresh install, or no ".1" yet).
		// Any other open error (permissions, a directory, an I/O fault) is real.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opening event log %q: %w", logPath, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e verifyEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("event %d in %q: parsing line: %w", n, logPath, err)
		}
		if !isVerifyKind(e.Kind) {
			continue // only verifier verdicts carry failure-pattern signal
		}
		// Detail["passed"] is the verifier's verdict, written as a JSON bool. A
		// missing or non-bool value reads as a non-pass but ALSO as a non-fail:
		// we only fold an EXPLICIT false, so absent evidence is never a scar.
		passed, ok := e.Detail["passed"].(bool)
		if !ok {
			continue
		}
		key := keyFor(e)
		c := clusters[key]
		if c == nil {
			c = &cluster{
				verifierID: key.verifierID,
				failClass:  key.failClass,
				backend:    key.backend,
			}
			clusters[key] = c
		}
		c.sample++
		if c.first.IsZero() || (!e.Time.IsZero() && e.Time.Before(c.first)) {
			c.first = e.Time
		}
		if e.Time.After(c.last) {
			c.last = e.Time
		}
		if !passed {
			c.fails++
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading event log %q: %w", logPath, err)
	}

	// Chain integrity is eventlog's authority: drop EVERYTHING we just folded if
	// this generation's chain does not link. A target distilled from a tampered log
	// would let an attacker steer the flywheel, so we fail closed (no patterns over
	// a broken chain), exactly like trust.Replay (internal/trust/replay.go:78).
	if err := eventlog.Verify(logPath); err != nil {
		return fmt.Errorf("verifying chain %q: %w", logPath, err)
	}
	return nil
}
