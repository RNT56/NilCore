package super

import (
	"context"
	"strings"
	"testing"
)

// TestSupervisorAskUserDispatch: the ask_user tool decodes the batch, calls the AskUser
// seam, and returns the operator's answers as a tool_result (labels quoted; notes shown).
func TestSupervisorAskUserDispatch(t *testing.T) {
	var got []AskQuestion
	s := &Supervisor{AskUser: func(_ context.Context, qs []AskQuestion) ([]AskAnswer, error) {
		got = qs
		return []AskAnswer{{Selected: []string{"Postgres"}}, {Custom: "use staging"}}, nil
	}}
	in := map[string]any{"questions": []map[string]any{
		{"question": "which db?", "choices": []map[string]any{{"label": "Postgres"}, {"label": "SQLite"}}},
		{"question": "which env?"},
	}}
	res, fin, _ := s.dispatchOne(context.Background(), 0, nil, toolUse("u1", "ask_user", in))
	if fin {
		t.Fatal("ask_user is not a finish")
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if len(got) != 2 || got[0].Prompt != "which db?" || len(got[0].Choices) != 2 {
		t.Fatalf("questions not decoded: %+v", got)
	}
	for _, want := range []string{`chose: "Postgres"`, "use staging", "Q1.", "Q2."} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("result missing %q in:\n%s", want, res.Content)
		}
	}
}

// TestSupervisorAskUserNilFailsClosed: with no AskUser seam (headless), a stray ask_user
// is an unknown tool — it never blocks.
func TestSupervisorAskUserNilFailsClosed(t *testing.T) {
	s := &Supervisor{}
	res, _, _ := s.dispatchOne(context.Background(), 0, nil,
		toolUse("u1", "ask_user", map[string]any{"questions": []map[string]any{{"question": "q"}}}))
	if !res.IsError || !strings.Contains(res.Content, "unknown tool") {
		t.Fatalf("nil seam must fail closed, got %+v", res)
	}
}

// TestSupervisorAskUserValidation rejects an empty or oversized batch.
func TestSupervisorAskUserValidation(t *testing.T) {
	s := &Supervisor{AskUser: func(context.Context, []AskQuestion) ([]AskAnswer, error) { return nil, nil }}
	if res, _, _ := s.dispatchOne(context.Background(), 0, nil,
		toolUse("u1", "ask_user", map[string]any{"questions": []map[string]any{}})); !res.IsError {
		t.Fatal("empty batch should error")
	}
	big := make([]map[string]any, 6)
	for i := range big {
		big[i] = map[string]any{"question": "q"}
	}
	if res, _, _ := s.dispatchOne(context.Background(), 0, nil,
		toolUse("u2", "ask_user", map[string]any{"questions": big})); !res.IsError {
		t.Fatal(">5 questions should error")
	}
}
