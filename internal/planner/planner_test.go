package planner

import (
	"context"
	"testing"

	"nilcore/internal/model"
)

type fakeModel struct{ text string }

func (fakeModel) Model() string { return "fake" }
func (f fakeModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: f.text}}}, nil
}

func TestPlanValid(t *testing.T) {
	m := fakeModel{text: `{"goal":"add feature","tasks":[
		{"id":"t1","goal":"write failing test","depends_on":[],"acceptance":"test exists and fails"},
		{"id":"t2","goal":"implement","depends_on":["t1"],"acceptance":"test passes"}]}`}
	tree, err := Plan(context.Background(), m, "add feature")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(tree.Tasks) != 2 || tree.Tasks[1].DependsOn[0] != "t1" {
		t.Fatalf("tree = %+v", tree)
	}
}

func TestPlanRejectsMissingAcceptance(t *testing.T) {
	m := fakeModel{text: `{"goal":"x","tasks":[{"id":"t1","goal":"do","depends_on":[],"acceptance":""}]}`}
	if _, err := Plan(context.Background(), m, "x"); err == nil {
		t.Error("plan without acceptance criteria must be rejected (contract-first)")
	}
}

func TestPlanRejectsBadDeps(t *testing.T) {
	tree := Tree{Tasks: []PlanTask{{ID: "t1", Goal: "g", Acceptance: "a", DependsOn: []string{"ghost"}}}}
	if err := tree.Validate(); err == nil {
		t.Error("dependency on an unknown task must be rejected")
	}
}

func TestPlanRejectsNonJSON(t *testing.T) {
	m := fakeModel{text: "I cannot do that"}
	if _, err := Plan(context.Background(), m, "x"); err == nil {
		t.Error("non-JSON plan output must error")
	}
}
