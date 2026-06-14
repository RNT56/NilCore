package advisor

import (
	"context"
	"errors"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

type fakeModel struct {
	reply string
	calls int
}

func (f *fakeModel) Model() string { return "advisor-fake" }
func (f *fakeModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	f.calls++
	return model.Response{Content: []model.Block{{Type: "text", Text: f.reply}}}, nil
}

func TestConsult(t *testing.T) {
	m := &fakeModel{reply: "write the failing test first"}
	a := New(m, 0)
	out, err := a.Consult(context.Background(), summarize.ContextSummary{Goal: "fix it"}, "how?")
	if err != nil || out != "write the failing test first" {
		t.Fatalf("Consult = %q, %v", out, err)
	}
	if a.Calls() != 1 || m.calls != 1 {
		t.Errorf("calls = %d / %d", a.Calls(), m.calls)
	}
}

func TestCeiling(t *testing.T) {
	m := &fakeModel{reply: "ok"}
	a := New(m, 2)
	for i := 0; i < 2; i++ {
		if _, err := a.Consult(context.Background(), summarize.ContextSummary{}, "q"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := a.Consult(context.Background(), summarize.ContextSummary{}, "q"); !errors.Is(err, ErrCeiling) {
		t.Errorf("third call = %v, want ErrCeiling", err)
	}
	if m.calls != 2 {
		t.Errorf("model called %d times past the ceiling", m.calls)
	}
}

func TestShouldEscalate(t *testing.T) {
	if !ShouldEscalate(3, 3) || !ShouldEscalate(4, 3) {
		t.Error("should escalate at/after K failures")
	}
	if ShouldEscalate(2, 3) || ShouldEscalate(5, 0) {
		t.Error("should not escalate below K, or when K=0")
	}
}

func TestToolDefinition(t *testing.T) {
	if Tool().Name != "ask_advisor" {
		t.Errorf("tool name = %q", Tool().Name)
	}
}
