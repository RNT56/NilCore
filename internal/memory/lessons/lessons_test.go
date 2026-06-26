package lessons_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/memory/lessons"
)

// secretBody is a free-text payload we deliberately stuff into a verify event's
// Detail under a NON-allowlisted key. No distilled Record may ever contain it
// (I7: failing output is data, never instructions, and never surfaced as a
// lesson). It carries a recognizable marker we assert against.
const secretBody = "panic: SECRET_FAILING_OUTPUT goroutine 1 [running] do-not-leak-me"

// writeLog builds a fresh hash-chained event log at a temp path from the given
// events, appended in order via the real eventlog API, and returns its path.
func writeLog(t *testing.T, events []eventlog.Event) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	for _, e := range events {
		log.Append(e)
	}
	if err := log.Err(); err != nil {
		t.Fatalf("log write error: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	// Sanity: a freshly written chain must verify, or the test fixture is wrong.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("fixture chain does not verify: %v", err)
	}
	return path
}

// failEvent is a verify-family failure carrying structural identity AND a
// free-text body under a non-allowlisted key, so tests can assert the body is
// dropped while the identity is mined.
func failEvent(kind, verifierID, failClass string) eventlog.Event {
	return eventlog.Event{
		Task: "T",
		Kind: kind,
		Detail: map[string]any{
			"passed":      false,
			"verifier_id": verifierID,
			"fail_class":  failClass,
			"output":      secretBody, // non-allowlisted body — must never leak
			"stderr":      secretBody,
		},
	}
}

func TestRecurringFailureBecomesOneDedupedLesson(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "T", Kind: "task_start"},
		failEvent("verify", "go-test", "compile"),
		failEvent("verify", "go-test", "compile"),
		failEvent("final_verify", "go-test", "compile"), // same pattern, 3rd hit
	})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected exactly one deduped lesson, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.Scope != "global" {
		t.Errorf("lesson scope = %q, want global", r.Scope)
	}
	if r.Key == "" || r.Value == "" {
		t.Errorf("lesson must have a non-empty key/value: %+v", r)
	}
	// The structural identity must be present in the templated value.
	for _, want := range []string{"go-test", "compile"} {
		if !strings.Contains(r.Value, want) {
			t.Errorf("lesson value missing structural field %q: %q", want, r.Value)
		}
	}
	// The count (3) must be reflected.
	if !strings.Contains(r.Value, "3") {
		t.Errorf("lesson value missing recurrence count: %q", r.Value)
	}
}

func TestSingleFailureIsNotALesson(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "T", Kind: "task_start"},
		failEvent("verify", "go-test", "compile"), // one-off ⇒ below threshold
		{Task: "T", Kind: "verify", Detail: map[string]any{"passed": true}},
	})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("a one-off failure must not become a lesson, got %d: %+v", len(recs), recs)
	}
}

func TestPassingAndNonVerifyEventsAreIgnored(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		{Task: "T", Kind: "task_start"},
		// Passing verifies never fold, no matter how many.
		{Task: "T", Kind: "verify", Detail: map[string]any{"passed": true, "verifier_id": "lint"}},
		{Task: "T", Kind: "final_verify", Detail: map[string]any{"passed": true, "verifier_id": "lint"}},
		// A race_outcome failure is the router's signal, not a verifier failure.
		{Task: "T", Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": false}},
		{Task: "T", Kind: "race_outcome", Backend: "native", Detail: map[string]any{"passed": false}},
	})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("only verify-family failures fold; got %d lessons: %+v", len(recs), recs)
	}
}

func TestRawFailingOutputNeverAppearsInValue(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		failEvent("verify", "go-test", "compile"),
		failEvent("verify", "go-test", "compile"),
	})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one lesson, got %d", len(recs))
	}
	for _, r := range recs {
		if strings.Contains(r.Value, secretBody) || strings.Contains(r.Key, secretBody) {
			t.Fatalf("I7 violation: raw failing output leaked into a record: key=%q value=%q", r.Key, r.Value)
		}
		if strings.Contains(r.Value, "SECRET_FAILING_OUTPUT") {
			t.Fatalf("I7 violation: failing-output marker leaked into value: %q", r.Value)
		}
	}
}

// A body smuggled into an allowlisted identity KEY must still be rejected: the
// per-field sanitizer drops whitespace/oversized payloads, so even a hostile
// emitter cannot turn an identity field into a free-text channel.
func TestBodySmuggledIntoIdentityKeyIsSanitized(t *testing.T) {
	ev := func() eventlog.Event {
		return eventlog.Event{
			Task: "T",
			Kind: "verify",
			Detail: map[string]any{
				"passed":      false,
				"verifier_id": secretBody, // hostile: a body in an id key
				"fail_class":  "compile",
			},
		}
	}
	path := writeLog(t, []eventlog.Event{ev(), ev()})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one lesson, got %d", len(recs))
	}
	v := recs[0].Value
	if strings.Contains(v, "SECRET_FAILING_OUTPUT") || strings.Contains(v, "goroutine") || strings.Contains(v, "do-not-leak-me") {
		t.Fatalf("I7 violation: smuggled body survived into value: %q", v)
	}
	if strings.Contains(recs[0].Key, "SECRET_FAILING_OUTPUT") || strings.Contains(recs[0].Key, "goroutine") {
		t.Fatalf("I7 violation: smuggled body survived into key: %q", recs[0].Key)
	}
}

func TestTamperedChainFailsClosed(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		failEvent("verify", "go-test", "compile"),
		failEvent("verify", "go-test", "compile"),
	})

	// Tamper: append a forged line whose hash does not chain.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	forged := string(data) +
		`{"seq":99,"kind":"verify","detail":{"passed":false,"verifier_id":"x"},"prev":"deadbeef","hash":"forged"}` + "\n"
	if err := os.WriteFile(path, []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}

	recs, err := lessons.Distill(path)
	if err == nil {
		t.Fatalf("expected fail-closed error on a broken chain, got nil (and %d records)", len(recs))
	}
	if recs != nil {
		t.Fatalf("fail-closed must return NO records, got %d: %+v", len(recs), recs)
	}
}

func TestMissingLogIsNoLessonsNoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("missing log must not error, got %v", err)
	}
	if recs != nil {
		t.Fatalf("missing log must yield no lessons, got %+v", recs)
	}
}

func TestDistinctPatternsYieldDistinctLessons(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		failEvent("verify", "go-test", "compile"),
		failEvent("verify", "go-test", "compile"),
		failEvent("verify", "lint", "format"),
		failEvent("verify", "lint", "format"),
	})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected two distinct lessons, got %d: %+v", len(recs), recs)
	}
	// Deterministic order: lint < go-test? sorted by verifierID then failClass.
	if recs[0].Key == recs[1].Key {
		t.Fatalf("distinct patterns must have distinct keys: %q", recs[0].Key)
	}
}

func TestFailClassFallsBackToKindThenConstant(t *testing.T) {
	// No fail_class, but a "kind" hint ⇒ class derives from kind.
	kindOnly := func() eventlog.Event {
		return eventlog.Event{Task: "T", Kind: "verify", Detail: map[string]any{
			"passed": false, "verifier_id": "vetter", "kind": "vet",
		}}
	}
	// Neither fail_class nor kind ⇒ the constant bucket, still a valid lesson.
	bare := func() eventlog.Event {
		return eventlog.Event{Task: "T", Kind: "verify", Detail: map[string]any{
			"passed": false, "verifier_id": "bare",
		}}
	}
	path := writeLog(t, []eventlog.Event{kindOnly(), kindOnly(), bare(), bare()})

	recs, err := lessons.Distill(path)
	if err != nil {
		t.Fatalf("Distill: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected two lessons, got %d: %+v", len(recs), recs)
	}
	joined := recs[0].Value + "\n" + recs[1].Value
	if !strings.Contains(joined, "vet") {
		t.Errorf("expected a lesson classed by the kind fallback %q: %q", "vet", joined)
	}
	if !strings.Contains(joined, "verify_failed") {
		t.Errorf("expected a lesson with the constant fallback class: %q", joined)
	}
}

func TestDistillNThreshold(t *testing.T) {
	path := writeLog(t, []eventlog.Event{
		failEvent("verify", "go-test", "compile"),
		failEvent("verify", "go-test", "compile"),
	})

	// Floor of 3 ⇒ a pattern seen twice is below the bar ⇒ no lesson.
	recs, err := lessons.DistillN(path, 3)
	if err != nil {
		t.Fatalf("DistillN: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("threshold 3 must drop a 2x pattern, got %d", len(recs))
	}
	// Floor clamps to 1 ⇒ even a single failure surfaces.
	recs, err = lessons.DistillN(path, 0)
	if err != nil {
		t.Fatalf("DistillN: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("clamped threshold 1 must surface the pattern, got %d", len(recs))
	}
}
