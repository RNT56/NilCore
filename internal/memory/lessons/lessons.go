// Package lessons is the A8 "learn from scars" distiller (Pillar 3, LRN-T02).
//
// WHY: a verifier failure that recurs is a scar — the same broken step the agent
// keeps walking into. The cheapest place to stop repeating it is the next
// same-class task, where a one-line reminder ("this verifier has failed before;
// here is its shape") nudges the model away from the rut. This package mines the
// append-only event log for RECURRING verifier-failure PATTERNS and distills them
// into deduped memory.Record values the caller may choose to Remember.
//
// It exists as a separate leaf, not inside internal/memory, for three reasons:
//
//   - It only ever READS the event log (I5: append-only, replay-only). It runs
//     eventlog.Verify and fails CLOSED on a broken chain — a log we cannot trust
//     yields NO lessons, because a lesson learned from forged evidence is worse
//     than none (mirrors internal/trust/replay.go and internal/inspect).
//
//   - A lesson is judged ONLY by the verifier's recorded verdict (I2). We fold a
//     "failure" only from a verify-family event whose Detail["passed"] is the
//     boolean false the verifier itself wrote — never from a backend self-report.
//
//   - The distilled Value templates STRUCTURAL fields ONLY (verifier id, a coarse
//     failure class, counts) — NEVER raw failing output or any model/tool body
//     (I7: tool output and file contents are data, never instructions). We read a
//     fixed allowlist of identity keys from Detail and ignore everything else, so
//     a free-text payload can never leak into a record that is later surfaced to
//     the model as context.
//
// Default-off: Distill never writes. It returns records; the caller (the
// NILCORE_LESSONS-gated command in LRN-T03) decides whether to memory.Remember
// them. When unwired, this package does nothing and the build is byte-identical.
package lessons

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/memory"
)

// MinRecurrence is the threshold that separates a one-off slip from a scar. A
// failure pattern must be observed at least this many times before it earns a
// lesson, so a single transient failure never pollutes memory. It is exported so
// the wiring layer can document the bar it ships with; callers needing a different
// floor use DistillN.
const MinRecurrence = 2

// verifyKinds is the set of event kinds whose Detail["passed"] carries a
// verifier's own pass/fail verdict (I2). Only these fold into a scar; a
// race_outcome is the router's signal, not a verifier failure to learn from, and
// a backend's self-report never appears here at all.
var verifyKinds = map[string]bool{
	"verify":       true,
	"final_verify": true,
}

// Structural-only Detail keys. These are the ONLY fields Distill ever reads from
// an event's Detail, and each is a short harness-written identity scalar — never a
// body. This allowlist is the I7 boundary in code: a free-text "output"/"stderr"/
// "log" field cannot reach a Record because we never look at it.
const (
	keyPassed    = "passed"      // bool: the verifier's verdict
	keyVerifier  = "verifier_id" // string: which verifier (LRN-T01 enrichment)
	keyFailClass = "fail_class"  // string: a coarse failure bucket (LRN-T01)
	keyKind      = "kind"        // string: fallback failure kind when no fail_class
)

// scarEvent mirrors only the structural fields of an on-disk eventlog.Event a
// lesson is built from: the event Kind (to select verify-family events) and the
// Detail map (from which we read ONLY the allowlisted identity keys above). Every
// other field — time, seq, hash, backend, and any free-text Detail payload — is
// ignored. Chain integrity is eventlog.Verify's job, checked after the fold.
type scarEvent struct {
	Kind   string         `json:"kind"`
	Detail map[string]any `json:"detail"`
}

// pattern is the deduped identity of a recurring scar: the verifier that failed
// and the coarse class of the failure. Two failures with the same pattern are the
// same scar, counted once with a tally — never two records.
type pattern struct {
	verifierID string
	failClass  string
}

// Distill replays the append-only event log at logPath READ-ONLY, mines recurring
// verifier-failure patterns, and returns one deduped memory.Record per pattern
// seen at least MinRecurrence times. See DistillN for the threshold rationale.
func Distill(logPath string) ([]memory.Record, error) {
	return DistillN(logPath, MinRecurrence)
}

// DistillN is Distill with an explicit recurrence floor. A pattern observed fewer
// than minRecurrence times is NOT a lesson (a one-off failure is noise, not a
// scar); minRecurrence < 1 is clamped to 1.
//
// The order of operations is the fail-closed discipline (I5): we fold the whole
// log first, THEN run eventlog.Verify, and on any chain error we discard every
// record we just built and return the error with NO records. A log we cannot
// trust yields no lessons — exactly as inspect.Replay and trust.Replay fail
// closed. A MISSING log is not a failure: a fresh install has no scars yet, so it
// returns no records and no error.
func DistillN(logPath string, minRecurrence int) ([]memory.Record, error) {
	if minRecurrence < 1 {
		minRecurrence = 1
	}

	f, err := os.Open(logPath)
	if err != nil {
		// No log yet ⇒ no history ⇒ no scars. Any other open error (permissions, a
		// directory, an I/O fault) is a real failure to surface.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	counts := make(map[pattern]int)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e scarEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("event %d: parsing line: %w", n, err)
		}
		if !verifyKinds[e.Kind] {
			continue // only verify-family events carry a verifier verdict
		}
		// Detail["passed"] is the verifier's verdict, a JSON bool. A missing or
		// non-bool value reads as "not a recorded failure": absent evidence never
		// becomes a scar (fail-safe — we only learn from an explicit false).
		passed, ok := e.Detail[keyPassed].(bool)
		if !ok || passed {
			continue
		}
		counts[failPattern(e.Detail)]++
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading event log: %w", err)
	}

	// Chain integrity is eventlog's authority (I5). A parseable log whose hashes do
	// not link is untrustworthy; surface it AFTER discarding the tally we built, so
	// a tampered chain yields no lesson rather than a forged one.
	if err := eventlog.Verify(logPath); err != nil {
		return nil, fmt.Errorf("verifying chain: %w", err)
	}

	// Promote only patterns at or above the recurrence floor, in deterministic
	// order so the output (and any downstream dedupe) is stable.
	pats := make([]pattern, 0, len(counts))
	for p, c := range counts {
		if c >= minRecurrence {
			pats = append(pats, p)
		}
	}
	sort.Slice(pats, func(i, j int) bool {
		if pats[i].verifierID != pats[j].verifierID {
			return pats[i].verifierID < pats[j].verifierID
		}
		return pats[i].failClass < pats[j].failClass
	})

	out := make([]memory.Record, 0, len(pats))
	for _, p := range pats {
		out = append(out, lessonRecord(p, counts[p]))
	}
	return out, nil
}

// failPattern extracts a scar's structural identity from a verify event's Detail,
// reading ONLY the allowlisted identity keys (I7). A missing verifier id buckets
// as "unknown"; the failure class falls back from fail_class to a generic kind to
// a constant, so every failure lands in a well-defined, body-free bucket.
func failPattern(d map[string]any) pattern {
	return pattern{
		verifierID: structuralField(d, keyVerifier, "unknown"),
		failClass:  failClass(d),
	}
}

// failClass derives the coarse, structural failure class, preferring an explicit
// fail_class, then a kind hint, then a constant. It NEVER falls back to free text.
func failClass(d map[string]any) string {
	if fc := structuralField(d, keyFailClass, ""); fc != "" {
		return fc
	}
	if k := structuralField(d, keyKind, ""); k != "" {
		return k
	}
	return "verify_failed"
}

// structuralField reads one allowlisted Detail key as a SHORT identity token and
// sanitizes it so nothing body-like survives. A non-string value, an empty value,
// or one longer than maxTokenLen (a clear sign it is not an id but a payload) is
// rejected to def. The result is restricted to id-shaped characters; anything else
// is dropped. This is belt-and-suspenders on top of the key allowlist: even if an
// emitter ever stuffed text into an identity key, it cannot leak through.
func structuralField(d map[string]any, key, def string) string {
	v, ok := d[key].(string)
	if !ok {
		return def
	}
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxTokenLen {
		return def
	}
	if t := sanitizeToken(v); t != "" {
		return t
	}
	return def
}

// maxTokenLen bounds a structural identity token. Verifier ids and failure classes
// are short, stable handles (e.g. "go-test", "make-verify", "lint"); anything past
// this is presumed to be a payload, not an identity, and is rejected.
const maxTokenLen = 64

// sanitizeToken keeps only id-shaped runes (letters, digits, and the separators
// commonly used in verifier ids), dropping everything else. This guarantees a
// distilled Value can never carry whitespace-laden free text — a lesson is a
// structural handle, not a sentence.
func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lessonRecord templates a memory.Record from a structural pattern and its tally.
// The Key is the pattern's stable identity (so memory.Remember dedupes a re-run
// against an already-stored lesson) and the Value is a fixed-format sentence built
// ONLY from the structural fields and the count — no event body ever touches it
// (I7). The record is global scope: a flaky verifier is a fact about the toolchain,
// not one project.
func lessonRecord(p pattern, count int) memory.Record {
	key := "lesson:verify-fail:" + p.verifierID + ":" + p.failClass
	value := fmt.Sprintf(
		"Recurring verifier failure: verifier %q has failed with class %q %d times. "+
			"Anticipate this failure on the next same-class task and address it before verifying.",
		p.verifierID, p.failClass, count,
	)
	return memory.Record{
		Scope: memory.ScopeGlobal,
		Key:   key,
		Value: value,
	}
}
