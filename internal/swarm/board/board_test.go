package board

// board_test.go exercises the live scoreboard's CONCURRENCY safety and its TALLY
// correctness — and, in the keystone TestLiveVsReplayAgree, proves the live board and
// the replayed report agree field-by-field. The whole file is built to run under
// `go test -race`: the 300-goroutine test is the race detector's target.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/meter"
	"nilcore/internal/report"
)

// newTestBoard builds a Board over a fresh ledger and the real conservative pricer,
// with coalescing disabled (minInterval 0) so a test that wants every EmitSnapshot to
// land gets it. Tests that want coalescing pass their own interval.
func newTestBoard() (*Board, *budget.Ledger) {
	led := budget.New()
	return New(led, meter.NewTable(), 0), led
}

// TestRecordIsVerdictDriven asserts Record — the ONLY count mover — moves exactly the
// tally the verdict dictates: a pass increments passed, a fail increments failed, and
// neither leaks into the other (I2: the verdict governs, nothing else).
func TestRecordIsVerdictDriven(t *testing.T) {
	b, _ := newTestBoard()
	b.SetTotal(2)
	b.Record(ShardOutcome{ID: "a", Pass: 1, Passed: true})
	b.Record(ShardOutcome{ID: "b", Pass: 1, Passed: false})

	s := b.Snapshot()
	if s.Checked != 2 || s.Passed != 1 || s.Failed != 1 {
		t.Fatalf("checked/passed/failed = %d/%d/%d, want 2/1/1", s.Checked, s.Passed, s.Failed)
	}
	if s.RetryPass != 0 {
		t.Fatalf("retryPass = %d, want 0 (no retry yet)", s.RetryPass)
	}
	if s.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (only a passed)", s.Remaining)
	}
}

// TestRetryPassDetection drives a shard fail→pass across two passes and asserts
// RetryPass bumps exactly once — and only on the later pass (a first-pass green is never
// a retry).
func TestRetryPassDetection(t *testing.T) {
	b, _ := newTestBoard()
	b.SetTotal(1)

	// Pass 1: the shard fails.
	b.Record(ShardOutcome{ID: "x", Pass: 1, Passed: false})
	if s := b.Snapshot(); s.RetryPass != 0 || s.Failed != 1 {
		t.Fatalf("after pass1: retryPass=%d failed=%d, want 0/1", s.RetryPass, s.Failed)
	}

	// Pass 2: the same shard passes — a retry-pass.
	b.Record(ShardOutcome{ID: "x", Pass: 2, Passed: true})
	s := b.Snapshot()
	if s.RetryPass != 1 {
		t.Fatalf("after pass2: retryPass=%d, want 1", s.RetryPass)
	}
	if s.Passed != 1 || s.Failed != 0 {
		t.Fatalf("after pass2 roll: passed=%d failed=%d, want 1/0", s.Passed, s.Failed)
	}
	if s.Pass != 2 {
		t.Fatalf("pass = %d, want 2", s.Pass)
	}
	if s.Remaining != 0 {
		t.Fatalf("remaining = %d, want 0 (x now green)", s.Remaining)
	}
}

// TestFirstPassGreenIsNotRetry guards the everFailed/Pass>1 condition: a shard that
// passes on pass 1 with no prior failure must NOT count as a retry-pass.
func TestFirstPassGreenIsNotRetry(t *testing.T) {
	b, _ := newTestBoard()
	b.SetTotal(1)
	b.Record(ShardOutcome{ID: "x", Pass: 1, Passed: true})
	if s := b.Snapshot(); s.RetryPass != 0 {
		t.Fatalf("first-pass green counted as retry: retryPass=%d", s.RetryPass)
	}
}

// TestExhaustedRedCounts asserts an exhausted shard stays RED in the per-shard table
// and is not counted as passed/remaining-resolved.
func TestExhaustedRedCounts(t *testing.T) {
	b, _ := newTestBoard()
	b.SetTotal(2)
	b.Record(ShardOutcome{ID: "ok", Pass: 1, Passed: true})
	b.Record(ShardOutcome{ID: "dead", Pass: 3, Passed: false, Exhausted: true})

	s := b.Snapshot()
	if s.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1 (dead never passed)", s.Remaining)
	}
	var dead ShardRow
	for _, r := range s.Shards {
		if r.ID == "dead" {
			dead = r
		}
	}
	if !dead.Exhausted || dead.Passed {
		t.Fatalf("dead row = %+v, want Exhausted && !Passed", dead)
	}
}

// TestCostRollupEqualsLedgerTotal asserts Snapshot.Cost is the ledger's LIVE total — not
// a Board-side accumulation. Charge the ledger directly and the snapshot must reflect it
// even though the Board never saw a token.
func TestCostRollupEqualsLedgerTotal(t *testing.T) {
	b, led := newTestBoard()
	if err := led.Charge(context.Background(), "shard-a", 1000, 1.25); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if err := led.Charge(context.Background(), "shard-b", 500, 0.75); err != nil {
		t.Fatalf("charge: %v", err)
	}
	_, want := led.Total()
	if got := b.Snapshot().Cost; got != want {
		t.Fatalf("snapshot cost = %v, want ledger total %v", got, want)
	}
}

// TestOnUsageAccumulatesPerModel asserts OnUsage folds per-model token splits and the
// snapshot sums them, with the conservative pricer pricing each model line.
func TestOnUsageAccumulatesPerModel(t *testing.T) {
	b, _ := newTestBoard()
	b.OnUsage("claude-opus-4-8", 100, 50)
	b.OnUsage("claude-opus-4-8", 100, 50)
	b.OnUsage("gpt-5.5", 200, 0)

	s := b.Snapshot()
	if s.Tokens != 100+50+100+50+200 {
		t.Fatalf("tokens = %d, want 500", s.Tokens)
	}
	if len(s.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(s.Models))
	}
	// Models are sorted by id; claude sorts before gpt.
	if s.Models[0].Model != "claude-opus-4-8" || s.Models[0].In != 200 || s.Models[0].Out != 100 {
		t.Fatalf("claude row = %+v, want in=200 out=100", s.Models[0])
	}
	if s.Models[0].Dollars <= 0 {
		t.Fatalf("claude dollars = %v, want > 0 (priced)", s.Models[0].Dollars)
	}
}

// TestSnapshotImmutable asserts a returned Snapshot is a deep copy: mutating its slices
// cannot reach back into the Board, and a second Snapshot is unaffected.
func TestSnapshotImmutable(t *testing.T) {
	b, _ := newTestBoard()
	b.SetTotal(1)
	b.Record(ShardOutcome{ID: "x", Pass: 1, Passed: true, Status: "pass", SourceURL: "https://example.com/x"})
	b.OnUsage("claude-opus-4-8", 10, 5)

	first := b.Snapshot()
	// Mutate the returned slices aggressively.
	for i := range first.Shards {
		first.Shards[i].ID = "TAMPERED"
		first.Shards[i].SourceURL = "evil"
	}
	for i := range first.Models {
		first.Models[i].Model = "TAMPERED"
	}
	if len(first.Shards) > 0 {
		first.Shards = first.Shards[:0]
	}

	second := b.Snapshot()
	if len(second.Shards) != 1 || second.Shards[0].ID != "x" {
		t.Fatalf("second snapshot shards corrupted: %+v", second.Shards)
	}
	if second.Shards[0].SourceURL != "https://example.com/x" {
		t.Fatalf("second snapshot source corrupted: %q", second.Shards[0].SourceURL)
	}
	if len(second.Models) != 1 || second.Models[0].Model != "claude-opus-4-8" {
		t.Fatalf("second snapshot models corrupted: %+v", second.Models)
	}
}

// TestMarkCleanIsGreenGate asserts FinalCleanPass flips ONLY when BOTH the worklist is
// empty AND the chain is reported OK — a tampered-chain signal can never green the board
// (I2/I5).
func TestMarkCleanIsGreenGate(t *testing.T) {
	b, _ := newTestBoard()
	b.MarkClean(true, false) // empty worklist but broken chain
	if b.Snapshot().FinalCleanPass {
		t.Fatal("FinalCleanPass true over a broken chain")
	}
	b.MarkClean(false, true) // verified chain but non-empty worklist
	if b.Snapshot().FinalCleanPass {
		t.Fatal("FinalCleanPass true with work remaining")
	}
	b.MarkClean(true, true) // both legs satisfied
	if !b.Snapshot().FinalCleanPass {
		t.Fatal("FinalCleanPass false with empty worklist AND verified chain")
	}
}

// TestConcurrentRecordAndUsage is the race-detector target: 300 goroutines hammer
// Record + OnUsage while one polls Snapshot. With -race it must be clean, and the final
// tally must be exactly right (every Record landed, none double-counted).
func TestConcurrentRecordAndUsage(t *testing.T) {
	b, _ := newTestBoard()
	const n = 300
	b.SetTotal(n)

	stop := make(chan struct{})
	var poller sync.WaitGroup
	poller.Add(1)
	go func() {
		defer poller.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = b.Snapshot() // concurrent reader; must never tear under -race
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := shardID(i)
			b.MarkQueued(id)
			b.MarkRunning(id)
			// Half pass, half fail — a deterministic split so the final tally is exact.
			b.Record(ShardOutcome{ID: id, Pass: 1, Passed: i%2 == 0})
			b.OnUsage("claude-opus-4-8", 10, 5)
		}(i)
	}
	wg.Wait()
	close(stop)
	poller.Wait()

	s := b.Snapshot()
	if s.Checked != n {
		t.Fatalf("checked = %d, want %d", s.Checked, n)
	}
	wantPass := n / 2
	if s.Passed != wantPass || s.Failed != n-wantPass {
		t.Fatalf("passed/failed = %d/%d, want %d/%d", s.Passed, s.Failed, wantPass, n-wantPass)
	}
	if s.Tokens != n*15 {
		t.Fatalf("tokens = %d, want %d", s.Tokens, n*15)
	}
}

// TestEmitSnapshotCoalesced asserts EmitSnapshot suppresses a too-soon emit (coalescing)
// and that what it DOES write is metadata-only and leaves eventlog.Verify green.
func TestEmitSnapshotCoalesced(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "swarm.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	// A long coalescing window so the second emit is certainly suppressed.
	b := New(budget.New(), meter.NewTable(), time.Hour)
	b.SetTotal(1)
	b.Record(ShardOutcome{ID: "x", Pass: 1, Passed: true})

	if !b.EmitSnapshot(log) {
		t.Fatal("first EmitSnapshot returned false, want a write")
	}
	if b.EmitSnapshot(log) {
		t.Fatal("second EmitSnapshot wrote within the coalescing window")
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := eventlog.Verify(logPath); err != nil {
		t.Fatalf("eventlog.Verify failed after EmitSnapshot: %v", err)
	}

	// Exactly one scoreboard_snapshot line, and its Detail is counts only — no model
	// Value/SourceURL key (I7), no secret (I3).
	lines := readLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1 (second emit coalesced)", len(lines))
	}
	assertMetadataOnly(t, lines[0])
}

// TestEmitSnapshotForceBypassesCoalescing asserts the forced terminal emit always
// writes, even within the coalescing window — so a replayed report never loses the
// final scoreboard just because the last pass landed quickly after the prior emit.
func TestEmitSnapshotForceBypassesCoalescing(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "swarm.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	b := New(budget.New(), meter.NewTable(), time.Hour) // long window
	b.SetTotal(1)
	b.Record(ShardOutcome{ID: "x", Pass: 1, Passed: true})

	if !b.EmitSnapshot(log) {
		t.Fatal("first EmitSnapshot returned false, want a write")
	}
	if b.EmitSnapshot(log) {
		t.Fatal("coalesced emit unexpectedly wrote")
	}
	if !b.EmitSnapshotForce(log) {
		t.Fatal("forced emit must write even within the coalescing window")
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if lines := readLines(t, logPath); len(lines) != 2 {
		t.Fatalf("log has %d lines, want 2 (first + forced)", len(lines))
	}
}

// TestEmitSnapshotNoCoalesce asserts a non-positive interval disables coalescing — every
// EmitSnapshot writes.
func TestEmitSnapshotNoCoalesce(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "swarm.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	b, _ := newTestBoard() // minInterval 0
	b.SetTotal(1)
	b.Record(ShardOutcome{ID: "x", Pass: 1, Passed: true})
	wrote1 := b.EmitSnapshot(log)
	wrote2 := b.EmitSnapshot(log)
	if !wrote1 || !wrote2 {
		t.Fatal("EmitSnapshot suppressed a write with coalescing disabled")
	}
	_ = log.Close()
	if n := len(readLines(t, logPath)); n != 2 {
		t.Fatalf("log has %d lines, want 2", n)
	}
}

// TestLiveVsReplayAgree is the KEYSTONE: drive a Board through a scripted multi-pass
// sequence while appending the MATCHING events to a real eventlog.Log, then assert
// Board.Snapshot() equals ReplaySwarmReport(...).Swarm field-by-field. This is the proof
// that the live scoreboard and the replayed report never disagree — the live==replay
// contract the whole task hinges on.
func TestLiveVsReplayAgree(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "swarm.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}

	b := New(budget.New(), meter.NewTable(), 0) // no coalescing: every snapshot lands
	b.SetTotal(3)
	log.Append(eventlog.Event{Kind: SwarmStartKind, Detail: map[string]any{"total": 3}})

	// --- Pass 1: a and b pass, c fails. ---
	for _, id := range []string{"a", "b", "c"} {
		b.MarkQueued(id)
		log.Append(eventlog.Event{Kind: ShardEnqueuedKind, Detail: map[string]any{"id": id}})
	}
	driveShard(b, log, "a", 1, true)
	driveShard(b, log, "b", 1, true)
	driveShard(b, log, "c", 1, false)
	// c is requeued for pass 2.
	log.Append(eventlog.Event{Kind: ShardRequeuedKind, Detail: map[string]any{"id": "c", "attempt": 1}})
	// End-of-pass-1 snapshot: the runner's contract is one snapshot per pass boundary.
	if !b.EmitSnapshot(log) {
		t.Fatal("pass-1 EmitSnapshot did not write")
	}

	// --- Pass 2: c re-runs and passes (a retry-pass). ---
	driveShard(b, log, "c", 2, true)
	if !b.EmitSnapshot(log) {
		t.Fatal("pass-2 EmitSnapshot did not write")
	}

	// The pass converged: empty worklist on a (so-far) verified chain.
	b.MarkClean(true, true)
	log.Append(eventlog.Event{Kind: SwarmPassCleanKind, Detail: map[string]any{"pass": 2}})
	log.Append(eventlog.Event{Kind: SwarmDoneKind, Detail: map[string]any{}})

	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	if log.Err() != nil {
		t.Fatalf("log degraded mid-run: %v", log.Err())
	}

	// Replay the same log and compare the swarm dimension field-by-field.
	sr, err := report.ReplaySwarmReport(logPath, dir)
	if err != nil {
		t.Fatalf("ReplaySwarmReport: %v", err)
	}
	live := b.Snapshot()
	sw := sr.Swarm

	if live.Pass != sw.Pass {
		t.Errorf("Pass: live %d != replay %d", live.Pass, sw.Pass)
	}
	if live.Checked != sw.Checked {
		t.Errorf("Checked: live %d != replay %d", live.Checked, sw.Checked)
	}
	if live.Passed != sw.Passed {
		t.Errorf("Passed: live %d != replay %d", live.Passed, sw.Passed)
	}
	if live.Failed != sw.Failed {
		t.Errorf("Failed: live %d != replay %d", live.Failed, sw.Failed)
	}
	if live.RetryPass != sw.RetryPass {
		t.Errorf("RetryPass: live %d != replay %d", live.RetryPass, sw.RetryPass)
	}
	if live.Remaining != sw.Remaining {
		t.Errorf("Remaining: live %d != replay %d", live.Remaining, sw.Remaining)
	}
	if live.FinalCleanPass != sw.FinalCleanPass {
		t.Errorf("FinalCleanPass: live %v != replay %v", live.FinalCleanPass, sw.FinalCleanPass)
	}
	// Sanity on the values themselves: pass 2, one shard checked+passed, a retry, clean.
	okTally := live.Pass == 2 && live.Checked == 1 && live.Passed == 1 && live.Failed == 0 &&
		live.RetryPass == 1 && live.Remaining == 0 && live.FinalCleanPass
	if !okTally {
		t.Errorf("unexpected final live tally: %+v", live)
	}
}

// driveShard runs one shard's queued→running→verified lifecycle on the Board AND appends
// the matching dispatch + verified events to the log, so the live mutations and the
// logged events stay in lockstep (the whole point of the keystone).
func driveShard(b *Board, log *eventlog.Log, id string, pass int, passed bool) {
	b.MarkRunning(id)
	log.Append(eventlog.Event{Kind: ShardDispatchedKind, Detail: map[string]any{"id": id, "pass": pass}})
	b.Record(ShardOutcome{ID: id, Pass: pass, Passed: passed, Status: statusFor(passed)})
	log.Append(eventlog.Event{Kind: ShardVerifiedKind, Detail: map[string]any{"id": id, "pass": pass, "passed": passed}})
}

func statusFor(passed bool) string {
	if passed {
		return "pass"
	}
	return "fail"
}

// --- small test helpers ---

func shardID(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "s0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{digits[i%10]}, buf...)
		i /= 10
	}
	return "s" + string(buf)
}

// assertMetadataOnly decodes one logged scoreboard_snapshot line and asserts its Detail
// is COUNTS ONLY — the six expected keys, every value a number, and NO model-authored
// key (value/source_url/statement) (I7) and no obviously secret-looking string (I3).
func assertMetadataOnly(t *testing.T, line string) {
	t.Helper()
	var e struct {
		Kind   string         `json:"kind"`
		Detail map[string]any `json:"detail"`
	}
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if e.Kind != ScoreboardSnapshotKind {
		t.Fatalf("kind = %q, want %q", e.Kind, ScoreboardSnapshotKind)
	}
	want := map[string]bool{
		detailPass: true, detailChecked: true, detailPassed: true,
		detailFailed: true, detailRetryPass: true, detailRemaining: true,
	}
	for k, v := range e.Detail {
		if !want[k] {
			t.Errorf("unexpected Detail key %q (must be counts-only metadata, I7)", k)
		}
		if _, ok := v.(float64); !ok { // JSON numbers decode as float64
			t.Errorf("Detail[%q] = %v (%T), want a number", k, v, v)
		}
	}
	for _, banned := range []string{"value", "source_url", "statement", "api_key", "token"} {
		if _, ok := e.Detail[banned]; ok {
			t.Errorf("snapshot Detail leaked %q — must be metadata only", banned)
		}
	}
	if strings.Contains(strings.ToLower(line), "secret") {
		t.Errorf("snapshot line contains a secret-looking token: %q", line)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return splitNonEmpty(string(data))
}

func splitNonEmpty(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if line := s[start:i]; line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		if line := s[start:]; line != "" {
			out = append(out, line)
		}
	}
	return out
}
