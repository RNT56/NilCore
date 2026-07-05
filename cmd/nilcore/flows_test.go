package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/agenticflows"
	"nilcore/internal/artifact"
	"nilcore/internal/summarize"
	"nilcore/internal/swarm"
)

const sampleFlowJSON = `{
  "spec_version": "agentic-flows/v1",
  "id": "coding.feature-implementation",
  "version": "0.1.0",
  "title": "Feature implementation",
  "runtime": {
    "supported_cores": ["thinclaw", "nilcore", "crustcore"],
    "required_capabilities": ["repo.checkout", "patch.apply", "command.run", "evidence.capture", "human.review"]
  },
  "nodes": [
    {"id": "intake", "type": "intake", "produces": ["scoped-task"]},
    {"id": "plan", "type": "agent_task", "description": "build a plan", "requires": ["scoped-task"], "produces": ["impl-plan"]},
    {"id": "implement", "type": "agent_task", "description": "apply the change", "requires": ["impl-plan"], "produces": ["patch"]},
    {"id": "build", "type": "tool", "uses": "command-runner", "requires": ["patch"]}
  ]
}`

func writeFlow(t *testing.T, js string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flow.json")
	if err := os.WriteFile(p, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFlowDecodeAndEdgeDerivation proves the JSON decode maps onto the adapter shape and
// that edges are derived from the produces→requires dataflow, so AgentTaskSubtasks sees
// the dependency (implement depends on plan).
func TestFlowDecodeAndEdgeDerivation(t *testing.T) {
	doc, err := loadFlow(writeFlow(t, sampleFlowJSON))
	if err != nil {
		t.Fatal(err)
	}
	flow := doc.toAdapterFlow()

	if !agenticflows.SupportsNilCore(flow) {
		t.Fatal("flow lists nilcore; SupportsNilCore should be true")
	}
	if missing := agenticflows.UnsupportedCapabilities(flow, nilcoreCapabilities); len(missing) != 0 {
		t.Fatalf("all sample capabilities are supported, got missing: %v", missing)
	}
	subs, err := agenticflows.AgentTaskSubtasks(flow, summarize.ContextSummary{})
	if err != nil {
		t.Fatalf("AgentTaskSubtasks: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("want 2 agent_task subtasks (plan, implement), got %d: %+v", len(subs), subs)
	}
	// implement requires impl-plan which plan produces ⇒ implement depends on plan.
	byID := map[string][]string{}
	for _, s := range subs {
		byID[s.ID] = s.DependsOn
	}
	if deps := byID["implement"]; len(deps) != 1 || deps[0] != "plan" {
		t.Fatalf("implement must depend on plan (derived from requires/produces), got %v", deps)
	}
	// The tool node becomes a sandbox plan keyed by its "uses" name.
	plans, err := agenticflows.ToolSandboxPlans(flow)
	if err != nil {
		t.Fatalf("ToolSandboxPlans: %v", err)
	}
	if len(plans) != 1 || plans[0].Tool != "command-runner" {
		t.Fatalf("want one sandbox tool plan 'command-runner', got %+v", plans)
	}
}

// TestFlowRunPreservesDAGThroughSwarm proves the flows→swarm path keeps the flow's
// dependency structure: flowTree lifts the agent_tasks into a planner.Tree and the swarm
// TreeSharder (the exact seam flowsRun feeds) turns the DependsOn edge into a real
// Shard.Dep. This is the regression guard for the collapse bug — the old decompose path
// flattened the DAG into a newline goal list, so `implement` ran independently off HEAD
// instead of on `plan`'s integrated tip. The run goal must also carry the flow title +
// agentic-flows provenance.
func TestFlowRunPreservesDAGThroughSwarm(t *testing.T) {
	doc, err := loadFlow(writeFlow(t, sampleFlowJSON))
	if err != nil {
		t.Fatal(err)
	}
	subs, err := agenticflows.AgentTaskSubtasks(doc.toAdapterFlow(), summarize.ContextSummary{})
	if err != nil {
		t.Fatalf("AgentTaskSubtasks: %v", err)
	}

	tree, err := flowTree(doc, subs)
	if err != nil {
		t.Fatalf("flowTree: %v", err)
	}
	if want := "Feature implementation — agentic-flows source: coding.feature-implementation@0.1.0"; tree.Goal != want {
		t.Errorf("tree goal = %q, want %q", tree.Goal, want)
	}
	// Contract-first: every flow task must carry a non-empty Acceptance criterion so the
	// tree passes planner.Validate (flowTree itself validates, but assert the criterion is
	// actually populated, not just non-empty by accident).
	for _, tk := range tree.Tasks {
		if strings.TrimSpace(tk.Acceptance) == "" {
			t.Errorf("flow task %q has empty Acceptance — contract-first waived", tk.ID)
		}
	}
	if err := tree.Validate(); err != nil {
		t.Errorf("flowTree produced a non-contract-valid tree: %v", err)
	}

	// Locate plan and implement in the (topologically ordered) tree.
	idx := map[string]int{}
	for i, tk := range tree.Tasks {
		idx[tk.ID] = i
	}
	planIdx, ok := idx["plan"]
	if !ok {
		t.Fatalf("plan task missing from tree: %+v", tree.Tasks)
	}
	implIdx, ok := idx["implement"]
	if !ok {
		t.Fatalf("implement task missing from tree: %+v", tree.Tasks)
	}

	shards, err := swarm.TreeSharder{Tree: tree, Kind: artifact.KindSpec, Pack: "code", Role: "implementer"}.Shards(context.Background(), "", "runF")
	if err != nil {
		t.Fatalf("TreeSharder.Shards: %v", err)
	}
	if len(shards) != len(tree.Tasks) {
		t.Fatalf("got %d shards, want %d", len(shards), len(tree.Tasks))
	}
	// The dependent (implement) must carry a real Shard.Dep on its dependency (plan) —
	// the flow's DAG has become runner-honored edges, not a flattened goal list.
	planShardID := shards[planIdx].ID
	found := false
	for _, d := range shards[implIdx].Deps {
		if d == planShardID {
			found = true
		}
	}
	if !found {
		t.Errorf("implement shard %q Deps = %v, must include plan shard %q", shards[implIdx].ID, shards[implIdx].Deps, planShardID)
	}
}

// TestFlowValidateRejectsUnsupported proves validate fails closed (exit 1) for a flow
// that does not list nilcore or needs a capability NilCore does not advertise.
func TestFlowValidateRejectsUnsupported(t *testing.T) {
	const noNilcore = `{"id":"x","version":"1","runtime":{"supported_cores":["thinclaw"]},"nodes":[{"id":"a","type":"agent_task","description":"do","produces":["y"]}]}`
	doc, err := loadFlow(writeFlow(t, noNilcore))
	if err != nil {
		t.Fatal(err)
	}
	if code := flowsValidate(doc, doc.toAdapterFlow()); code != 1 {
		t.Fatalf("a flow without nilcore support must validate to exit 1, got %d", code)
	}

	const badCap = `{"id":"x","version":"1","runtime":{"supported_cores":["nilcore"],"required_capabilities":["quantum.entangle"]},"nodes":[{"id":"a","type":"agent_task","description":"do","produces":["y"]}]}`
	doc2, err := loadFlow(writeFlow(t, badCap))
	if err != nil {
		t.Fatal(err)
	}
	if code := flowsValidate(doc2, doc2.toAdapterFlow()); code != 1 {
		t.Fatalf("a flow needing an unsupported capability must validate to exit 1, got %d", code)
	}
}
