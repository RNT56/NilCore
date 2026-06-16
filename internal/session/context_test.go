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
	// History was rewritten in place to the compacted seed.
	if len(s.History) != 2 {
		t.Errorf("s.History should be the 2-turn compacted seed, got %d", len(s.History))
	}
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
