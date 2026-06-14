package summarize

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/model"
)

type fakeModel struct {
	text string
}

func (fakeModel) Model() string { return "fake" }
func (f fakeModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: f.text}}}, nil
}

func TestSummarizeParsesJSON(t *testing.T) {
	m := fakeModel{text: `Here you go: {"goal":"fix bug","constraints":["no deps"],"decisions":["found it in x.go"],"remaining":"write the fix"}`}
	cs, err := Summarize(context.Background(), m, "fix bug", "lots of state")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if cs.Goal != "fix bug" || len(cs.Constraints) != 1 || len(cs.Decisions) != 1 || cs.Remaining != "write the fix" {
		t.Fatalf("parsed = %+v", cs)
	}
}

func TestSummarizeFallback(t *testing.T) {
	m := fakeModel{text: "no json here, sorry"}
	cs, err := Summarize(context.Background(), m, "the goal", "remaining work state")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if cs.Goal != "the goal" || cs.Remaining == "" {
		t.Errorf("fallback summary = %+v", cs)
	}
}

func TestContextSummaryString(t *testing.T) {
	cs := ContextSummary{Goal: "g", Constraints: []string{"c1"}, Decisions: []string{"d1"}, Remaining: "r"}
	s := cs.String()
	for _, want := range []string{"Goal: g", "c1", "d1", "Remaining: r"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q: %s", want, s)
		}
	}
}
