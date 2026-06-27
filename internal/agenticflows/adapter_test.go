package agenticflows

import (
	"context"
	"reflect"
	"testing"

	"nilcore/internal/sandbox"
	"nilcore/internal/summarize"
)

type recordingSandbox struct {
	commands []string
}

func (r *recordingSandbox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	r.commands = append(r.commands, cmd)
	return sandbox.Result{Stdout: "ok:" + cmd, ExitCode: 0}, nil
}

func (r *recordingSandbox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return r.Exec(ctx, cmd)
}

func (r *recordingSandbox) Workdir() string {
	return "/work"
}

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

func TestToolNodesRequireSandboxAndRunThroughSandbox(t *testing.T) {
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

	box := &recordingSandbox{}
	results, err := RunToolSandboxPlans(context.Background(), box, plans, func(plan ToolSandboxPlan) (string, error) {
		return "nilcore-tool " + plan.Tool, nil
	})
	if err != nil {
		t.Fatalf("RunToolSandboxPlans returned error: %v", err)
	}
	if !reflect.DeepEqual(box.commands, []string{"nilcore-tool test.run", "nilcore-tool lint.run"}) {
		t.Fatalf("expected sandbox commands, got %+v", box.commands)
	}
	if len(results) != 2 || results[0].Stdout != "ok:nilcore-tool test.run" {
		t.Fatalf("unexpected sandbox results: %+v", results)
	}
}

func TestRunToolSandboxPlansRejectsHostSidePlan(t *testing.T) {
	plans := []ToolSandboxPlan{{NodeID: "unsafe", Tool: "host.run", RequiresSandbox: false}}
	box := &recordingSandbox{}

	if _, err := RunToolSandboxPlans(context.Background(), box, plans, func(ToolSandboxPlan) (string, error) {
		return "host-run", nil
	}); err == nil {
		t.Fatal("expected unsandboxed-plan error")
	}
	if len(box.commands) != 0 {
		t.Fatalf("unsafe plan should not run commands, got %+v", box.commands)
	}
}
