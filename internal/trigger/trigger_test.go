package trigger

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/eventlog"
)

func TestReversibleSelfStarts(t *testing.T) {
	var startedGoal string
	tr := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false }, // would deny, but reversible shouldn't ask
		Start:   func(_ context.Context, goal string) error { startedGoal = goal; return nil },
	}
	started, err := tr.Handle(context.Background(), Signal{Source: "ci", Goal: "fix the failing test in math_test.go"})
	if err != nil || !started {
		t.Fatalf("reversible signal should self-start: %v %v", started, err)
	}
	if startedGoal == "" {
		t.Error("Start was not called")
	}
}

func TestIrreversibleGated(t *testing.T) {
	denied := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false },
		Start:   func(context.Context, string) error { t := false; _ = t; return nil },
	}
	started, _ := denied.Handle(context.Background(), Signal{Source: "ci", Goal: "git push origin main"})
	if started {
		t.Error("irreversible work must be gated; a denied gate must not start it")
	}

	var ran bool
	approved := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return true },
		Start:   func(context.Context, string) error { ran = true; return nil },
	}
	started, _ = approved.Handle(context.Background(), Signal{Goal: "deploy to staging"})
	if !started || !ran {
		t.Error("an approved gate should let irreversible work start")
	}
}

func TestDisabledDoesNothing(t *testing.T) {
	tr := &Trigger{Enabled: false, Start: func(context.Context, string) error { panic("must not start") }}
	if started, _ := tr.Handle(context.Background(), Signal{Goal: "anything"}); started {
		t.Error("disabled trigger must not start work")
	}
}

// TestNilStartDoesNotClaimStarted proves that when no runnable Start is wired,
// Handle reports started=false (nothing ran) AND emits no trigger_start event,
// so neither the return value nor the audit trail claims work that never began.
func TestNilStartDoesNotClaimStarted(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	tr := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return true }, // gate would pass; Start is nil
		Start:   nil,
		Log:     log,
	}
	// A reversible goal (no gate involved) with a nil Start must not claim a start.
	started, err := tr.Handle(context.Background(), Signal{Source: "ci", Goal: "fix the failing test"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if started {
		t.Error("a nil Start must report started=false; nothing ran")
	}

	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if got := countEvents(t, logPath, "trigger_start"); got != 0 {
		t.Errorf("nil Start must emit no trigger_start event, got %d", got)
	}
}

// TestRateLimiterDailyCap proves the per-day cap: N starts are allowed within a
// rolling window, the N+1th is refused with reason "daily-cap", and the budget
// refills once the window rolls. A fixed injected clock keeps it deterministic.
func TestRateLimiterDailyCap(t *testing.T) {
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	clock := base
	rl := &RateLimiter{MaxPerDay: 3, Now: func() time.Time { return clock }}

	for i := 0; i < 3; i++ {
		if ok, _ := rl.Allow(); !ok {
			t.Fatalf("self-start %d within the cap should be allowed", i)
		}
	}
	if ok, reason := rl.Allow(); ok || reason != "daily-cap" {
		t.Fatalf("the over-cap self-start must be refused with daily-cap, got ok=%v reason=%q", ok, reason)
	}
	// After the trailing window rolls, the earlier starts age out and the cap refills.
	clock = base.Add(rateWindow + time.Minute)
	if ok, _ := rl.Allow(); !ok {
		t.Fatal("after the 24h window rolls, a self-start should be allowed again")
	}
}

// TestRateLimiterCooldown proves the min-interval cooldown between consecutive
// self-starts, independent of the daily cap.
func TestRateLimiterCooldown(t *testing.T) {
	clock := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	rl := &RateLimiter{MinInterval: time.Minute, Now: func() time.Time { return clock }}

	if ok, _ := rl.Allow(); !ok {
		t.Fatal("first self-start should be allowed")
	}
	clock = clock.Add(30 * time.Second) // still inside the cooldown
	if ok, reason := rl.Allow(); ok || reason != "cooldown" {
		t.Fatalf("a self-start inside the cooldown must be refused, got ok=%v reason=%q", ok, reason)
	}
	clock = clock.Add(31 * time.Second) // now > 1m since the first
	if ok, _ := rl.Allow(); !ok {
		t.Fatal("after the cooldown elapses, a self-start should be allowed")
	}
}

// TestNilLimiterUnbounded proves a nil *RateLimiter and a zero value both allow
// everything, so an unwired trigger keeps its original unbounded behaviour.
func TestNilLimiterUnbounded(t *testing.T) {
	var nilRL *RateLimiter
	if ok, _ := nilRL.Allow(); !ok {
		t.Error("a nil RateLimiter must allow (opt-in default)")
	}
	zero := &RateLimiter{}
	for i := 0; i < 100; i++ {
		if ok, _ := zero.Allow(); !ok {
			t.Fatalf("a zero-value RateLimiter (no bound set) must allow, refused at %d", i)
		}
	}
}

// TestHandleRateLimitedRefusesAndAudits proves the self-start path honours the cap:
// starts beyond the daily cap are refused (started=false, Start never runs) AND each
// rejection emits an append-only trigger_ratelimited audit event (I5). Covers the
// "daily cap refused" and "audit on rejection" acceptance criteria together.
func TestHandleRateLimitedRefusesAndAudits(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	clock := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	var starts int
	tr := &Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false }, // reversible signal below never asks
		Start:   func(context.Context, string) error { starts++; return nil },
		Limiter: &RateLimiter{MaxPerDay: 2, Now: func() time.Time { return clock }},
		Log:     log,
	}
	sig := Signal{Source: "issue", Goal: "fix the failing test in math_test.go"} // reversible

	for i := 0; i < 2; i++ {
		clock = clock.Add(time.Hour) // step past any incidental cooldown; only a cap here
		if started, err := tr.Handle(context.Background(), sig); err != nil || !started {
			t.Fatalf("self-start %d within the cap should proceed: started=%v err=%v", i, started, err)
		}
	}
	// The third self-start in the same day is over the cap: refused, and Start is not run.
	clock = clock.Add(time.Hour)
	started, err := tr.Handle(context.Background(), sig)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if started {
		t.Error("a self-start beyond the daily cap must be refused (started=false)")
	}
	if starts != 2 {
		t.Errorf("Start ran %d times, want 2 (the capped self-start must not run)", starts)
	}

	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if got := countEvents(t, logPath, "trigger_ratelimited"); got != 1 {
		t.Errorf("a capped self-start must emit exactly one trigger_ratelimited event, got %d", got)
	}
	if got := countEvents(t, logPath, "trigger_start"); got != 2 {
		t.Errorf("only the 2 allowed self-starts should emit trigger_start, got %d", got)
	}
}

// countEvents counts append-only events of the given Kind in a JSONL event log.
func countEvents(t *testing.T, path, kind string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log for read: %v", err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"kind":"`+kind+`"`) {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return n
}
