package agenticflows

import (
	"reflect"
	"testing"

	"nilcore/internal/summarize"
)

func TestAgentTaskSubtasksMapNodesAndDependencies(t *testing.T) {
	flow := Flow{
		ID:      "coding.feature-implementation",
		Version: "0.1.0",
		Runtime: Runtime{SupportedCores: []string{"standalone", "nilcore"}},
		Nodes: []Node{
			{ID: "plan", Type: "agent_task", Title: "Plan implementation"},
			{ID: "implement", Type: "agent_task", Description: "Implement the accepted plan"},
			{ID: "verify", Type: "tool", Tool: "test.run"},
		},
		Edges: []Edge{{From: "plan", To: "implement"}, {From: "implement", To: "verify"}},
	}
	base := summarize.ContextSummary{
		Constraints: []string{"preserve user changes"},
		Remaining:   "finish the adapter",
	}

	subtasks, err := AgentTaskSubtasks(flow, base)
	if err != nil {
		t.Fatalf("AgentTaskSubtasks returned error: %v", err)
	}
	if len(subtasks) != 2 {
		t.Fatalf("expected 2 subtasks, got %d", len(subtasks))
	}
	if subtasks[0].ID != "plan" || subtasks[0].Goal != "Plan implementation" {
		t.Fatalf("unexpected first subtask: %+v", subtasks[0])
	}
	if subtasks[1].ID != "implement" || subtasks[1].Goal != "Implement the accepted plan" {
		t.Fatalf("unexpected second subtask: %+v", subtasks[1])
	}
	if !reflect.DeepEqual(subtasks[1].DependsOn, []string{"plan"}) {
		t.Fatalf("unexpected dependencies: %+v", subtasks[1].DependsOn)
	}
	wantConstraints := []string{
		"preserve user changes",
		"agentic-flows source: coding.feature-implementation@0.1.0",
	}
	if !reflect.DeepEqual(subtasks[1].Summary.Constraints, wantConstraints) {
		t.Fatalf("unexpected constraints: %+v", subtasks[1].Summary.Constraints)
	}
	if subtasks[1].Summary.Remaining != "finish the adapter" {
		t.Fatalf("expected base summary fields to be preserved, got %+v", subtasks[1].Summary)
	}
}

func TestAgentTaskSubtasksRejectsFlowsThatDoNotOptIntoNilCore(t *testing.T) {
	flow := Flow{
		ID:      "general.human-in-the-loop-review",
		Version: "0.1.0",
		Runtime: Runtime{SupportedCores: []string{"standalone"}},
		Nodes:   []Node{{ID: "review", Type: "agent_task", Title: "Review"}},
	}

	if _, err := AgentTaskSubtasks(flow, summarize.ContextSummary{}); err == nil {
		t.Fatal("expected unsupported-core error")
	}
}

func TestUnsupportedCapabilitiesAreSorted(t *testing.T) {
	flow := Flow{Runtime: Runtime{RequiredCapabilities: []string{"zeta", "alpha", "known"}}}

	missing := UnsupportedCapabilities(flow, map[string]bool{"known": true})

	if !reflect.DeepEqual(missing, []string{"alpha", "zeta"}) {
		t.Fatalf("unexpected missing capabilities: %+v", missing)
	}
}

func TestToolSandboxPlansMarkNodesSandboxRequired(t *testing.T) {
	flow := Flow{
		Nodes: []Node{
			{ID: "verify", Type: "tool", Tool: "test.run"},
			{ID: "lint", Type: "tool", Tool: "lint.run"},
		},
	}

	plans, err := ToolSandboxPlans(flow)
	if err != nil {
		t.Fatalf("ToolSandboxPlans returned error: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(plans))
	}
	for _, plan := range plans {
		if !plan.RequiresSandbox {
			t.Fatalf("plan is not sandbox-required: %+v", plan)
		}
	}

	// A tool node missing its tool name is a hard error (no stable handle to sandbox).
	if _, err := ToolSandboxPlans(Flow{Nodes: []Node{{ID: "x", Type: "tool"}}}); err == nil {
		t.Fatal("expected error for a tool node missing its tool name")
	}
}

// TestAgentTaskSubtasksTopologicalOrder proves the subtasks come back in dependency
// order even when the flow declares nodes out of order, so the `flows run` flattening
// honors the produces→requires DAG rather than node-declaration order.
func TestAgentTaskSubtasksTopologicalOrder(t *testing.T) {
	// Declared c, b, a but the dataflow is a -> b -> c, so the result must be a, b, c.
	flow := Flow{
		ID: "x", Version: "1",
		Runtime: Runtime{SupportedCores: []string{"nilcore"}},
		Nodes: []Node{
			{ID: "c", Type: "agent_task", Title: "third"},
			{ID: "b", Type: "agent_task", Title: "second"},
			{ID: "a", Type: "agent_task", Title: "first"},
		},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "c"}},
	}
	subs, err := AgentTaskSubtasks(flow, summarize.ContextSummary{})
	if err != nil {
		t.Fatalf("AgentTaskSubtasks: %v", err)
	}
	got := []string{subs[0].ID, subs[1].ID, subs[2].ID}
	if !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("subtask order = %v, want [a b c] (topological)", got)
	}
}

// TestAgentTaskSubtasksStableWhenIndependent proves independent tasks keep their
// original declaration order (the sort is stable; no edges ⇒ no reordering).
func TestAgentTaskSubtasksStableWhenIndependent(t *testing.T) {
	flow := Flow{
		ID: "x", Version: "1",
		Runtime: Runtime{SupportedCores: []string{"nilcore"}},
		Nodes: []Node{
			{ID: "one", Type: "agent_task", Title: "1"},
			{ID: "two", Type: "agent_task", Title: "2"},
			{ID: "three", Type: "agent_task", Title: "3"},
		},
	}
	subs, err := AgentTaskSubtasks(flow, summarize.ContextSummary{})
	if err != nil {
		t.Fatalf("AgentTaskSubtasks: %v", err)
	}
	got := []string{subs[0].ID, subs[1].ID, subs[2].ID}
	if !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("independent order = %v, want declaration order", got)
	}
}

// TestAgentTaskSubtasksRejectsCycle proves a contradictory dataflow (a depends on b,
// b depends on a) is reported, not silently dropped.
func TestAgentTaskSubtasksRejectsCycle(t *testing.T) {
	flow := Flow{
		ID: "x", Version: "1",
		Runtime: Runtime{SupportedCores: []string{"nilcore"}},
		Nodes: []Node{
			{ID: "a", Type: "agent_task", Title: "a"},
			{ID: "b", Type: "agent_task", Title: "b"},
		},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	if _, err := AgentTaskSubtasks(flow, summarize.ContextSummary{}); err == nil {
		t.Fatal("expected a dependency-cycle error")
	}
}
