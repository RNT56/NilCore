package session

import (
	"context"
	"testing"

	"nilcore/internal/model"
)

// ContextUsage divides the last call's input tokens by the model's window, and
// reports 0/0 before anything is measured or with no window resolver.
func TestContextUsage(t *testing.T) {
	s := New("c", "local", "/repo", nil)

	// No CtxWindow wired ⇒ unmeasured.
	s.RecordUsage("claude-opus", 5000, 100)
	if pct, _, window := s.ContextUsage(); pct != 0 || window != 0 {
		t.Errorf("no CtxWindow: got pct=%d window=%d, want 0/0", pct, window)
	}

	// With a 10k window and 5k last input ⇒ 50%.
	s.CtxWindow = func(string) int { return 10000 }
	if pct, used, window := s.ContextUsage(); pct != 50 || used != 5000 || window != 10000 {
		t.Errorf("got pct=%d used=%d window=%d, want 50/5000/10000", pct, used, window)
	}

	// Over-full clamps to 100.
	s.RecordUsage("claude-opus", 99999, 0)
	if pct, _, _ := s.ContextUsage(); pct != 100 {
		t.Errorf("over-full pct = %d, want 100", pct)
	}
}

// scriptedSummarizer is a model.Provider that returns a fixed summary text, so the
// compaction test is hermetic (no network).
type scriptedSummarizer struct{ calls int }

func (s *scriptedSummarizer) Model() string { return "claude-opus" }
func (s *scriptedSummarizer) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	s.calls++
	// summarize.Summarize parses the first JSON object out of the reply.
	return model.Response{Content: []model.Block{{Type: "text", Text: `{"goal":"ship it","remaining":"two requests handled"}`}}}, nil
}

// Auto-compaction: above the threshold, maybeCompact summarizes the prior turns
// into a seed and keeps the last turn verbatim; below it, History is untouched.
func TestAutoCompaction(t *testing.T) {
	s := New("c", "local", "/repo", nil)
	s.CtxWindow = func(string) int { return 1000 }
	s.Summarizer = &scriptedSummarizer{}

	history := []model.Message{
		userTurn("first request"),
		{Role: "assistant", Content: []model.Block{{Type: "text", Text: "did the first thing"}}},
		userTurn("second request"),
	}
	// In production the snapshot passed to maybeCompact is a copy of s.History; mirror
	// that here so the in-place splice has the live slice to operate on.
	s.History = append([]model.Message(nil), history...)

	// Below threshold (10% full) ⇒ unchanged.
	s.RecordUsage("claude-opus", 100, 0)
	if out := s.maybeCompact(context.Background(), s.State, history); len(out) != len(history) {
		t.Errorf("below threshold must not compact: got %d turns, want %d", len(out), len(history))
	}

	// At/above 80% ⇒ compact: prior turns → one summary seed, last turn kept.
	s.RecordUsage("claude-opus", 850, 0) // 85% of 1000
	out := s.maybeCompact(context.Background(), s.State, history)
	if len(out) != 2 {
		t.Fatalf("compaction should yield [summary, last]; got %d turns", len(out))
	}
	if txt := out[0].Content[0].Text; !contains(txt, "compacted") || !contains(txt, "ship it") {
		t.Errorf("first turn should be the compacted summary, got %q", txt)
	}
	if out[1].Content[0].Text != "second request" {
		t.Errorf("the latest turn must be kept verbatim, got %q", out[1].Content[0].Text)
	}
	// History was spliced in place to the compacted seed (summary + last turn).
	if len(s.History) != 2 {
		t.Errorf("s.History should be the 2-turn compacted seed, got %d", len(s.History))
	}
	if s.History[1].Content[0].Text != "second request" {
		t.Errorf("spliced History should keep the last turn verbatim, got %q", s.History[1].Content[0].Text)
	}
}

// A follow-up Turn that appends to s.History WHILE the summarize model call is in
// flight (s.mu released) must survive the compaction splice: the fix replaces only
// the summarized prefix and preserves every turn appended after the pre-lock
// snapshot. Before the fix, s.History was blindly overwritten with a slice built
// from the stale snapshot, dropping the concurrent turn.
func TestCompactionPreservesConcurrentAppend(t *testing.T) {
	s := New("c", "local", "/repo", nil)
	s.CtxWindow = func(string) int { return 1000 }

	history := []model.Message{
		userTurn("first request"),
		{Role: "assistant", Content: []model.Block{{Type: "text", Text: "did the first thing"}}},
		userTurn("second request"),
	}
	s.History = append([]model.Message(nil), history...)
	s.RecordUsage("claude-opus", 850, 0) // 85% of 1000 ⇒ over threshold

	// A summarizer that, mid-call, simulates a concurrent follow-up Turn appending a
	// user line to s.History (as Turn's in-flight branch does under s.mu). The splice
	// must preserve this appended turn.
	appended := userTurn("mid-summarize follow-up")
	s.Summarizer = &appendingSummarizer{s: s, appended: appended}

	out := s.maybeCompact(context.Background(), s.State, history)
	// The drive's own seed is still the compact [summary, last] shape.
	if len(out) != 2 || out[1].Content[0].Text != "second request" {
		t.Fatalf("returned seed should be [summary, last]; got %d turns", len(out))
	}
	// The LIVE History must be [summary, last, follow-up] — the concurrent append survived.
	if len(s.History) != 3 {
		t.Fatalf("s.History should be [summary, last, follow-up]; got %d turns", len(s.History))
	}
	if txt := s.History[0].Content[0].Text; !contains(txt, "compacted") {
		t.Errorf("first turn should be the compacted summary, got %q", txt)
	}
	if s.History[1].Content[0].Text != "second request" {
		t.Errorf("second turn should be the preserved last turn, got %q", s.History[1].Content[0].Text)
	}
	if s.History[2].Content[0].Text != "mid-summarize follow-up" {
		t.Errorf("the concurrent follow-up must survive compaction, got %q", s.History[2].Content[0].Text)
	}
}

// appendingSummarizer simulates a follow-up Turn racing the summarize call: it
// appends a turn to s.History (under s.mu, exactly as Turn's in-flight branch does)
// while the model round-trip is notionally in flight, then returns a fixed summary.
type appendingSummarizer struct {
	s        *Session
	appended model.Message
}

func (a *appendingSummarizer) Model() string { return "claude-opus" }
func (a *appendingSummarizer) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	a.s.mu.Lock()
	a.s.History = append(a.s.History, a.appended)
	a.s.mu.Unlock()
	return model.Response{Content: []model.Block{{Type: "text", Text: `{"goal":"ship it","remaining":"handled"}`}}}, nil
}

// No Summarizer ⇒ never compacts (byte-identical), regardless of pressure.
func TestNoCompactionWithoutSummarizer(t *testing.T) {
	s := New("c", "local", "/repo", nil)
	s.CtxWindow = func(string) int { return 100 }
	s.RecordUsage("m", 99, 0) // 99% full
	history := []model.Message{userTurn("a"), userTurn("b")}
	if out := s.maybeCompact(context.Background(), s.State, history); len(out) != 2 {
		t.Errorf("no Summarizer must never compact, got %d turns", len(out))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
