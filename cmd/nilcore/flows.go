package main

// flows.go — the `nilcore flows` command: NilCore's consumer of the portable
// agentic-flows contract (github.com/RNT56/agentic-flows). agentic-flows owns the
// workflow CONTRACT (versioned YAML/JSON flow specs); NilCore's role in that model is
// "sandboxed worker execution and supervision" — it consumes a flow's agent_task and
// tool nodes and runs them through its own verified machinery. This command is the
// reachable seam over internal/agenticflows (the pure adapter that maps a decoded flow
// onto NilCore's spawn + sandbox seams).
//
// Stdlib-only (I6): NilCore cannot parse YAML, so this consumes the flow as JSON — the
// `flowctl` CLI in agentic-flows emits JSON, or the operator converts the flow.yaml.
// The flow text is inert DATA (I7): it is decoded into typed Go values and its goals are
// executed through the SAME verified run path, never interpreted as control instructions.
//
// Two verbs:
//   - validate (default): decode the flow, check NilCore can consume it (it lists nilcore
//     as a supported core AND every required capability is one NilCore advertises), and
//     print the worker-dispatch plan (agent_task subtasks) + sandbox tool plans. Pure
//     inspection — NO execution. Exits non-zero on an unconsumable flow (a preflight gate).
//   - run: build the agent_task subtasks and execute the flow through the proven
//     `decompose` preset (each agent_task becomes an independent verified sub-run whose
//     branch is integrated into one re-verified tip — I2). Reuses the kernel's recursive
//     engine; adds no new dispatch loop.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"nilcore/internal/agenticflows"
	"nilcore/internal/summarize"
)

// nilcoreCapabilities is the capability vocabulary NilCore advertises to the
// agentic-flows contract — exactly what the sandboxed-worker runtime actually does
// (mirrors the consumer contract in agentic-flows' nilcore adapter-smoke manifests).
// A flow that requires anything outside this set is reported as unconsumable by
// validate rather than silently half-run.
var nilcoreCapabilities = map[string]bool{
	"repo.checkout":    true, // disposable git worktree per run
	"patch.apply":      true, // the host-side file/git tools (worktree-confined)
	"command.run":      true, // model-emitted commands run in the sandbox
	"tool.exec":        true, // sandboxed tool nodes
	"task.decompose":   true, // the kernel recursive decompose preset
	"worker.dispatch":  true, // the supervisor/swarm worker fan-out
	"evidence.capture": true, // verifier-backed artifact claims
	"human.review":     true, // the irreversible-action gate
}

// flowDoc is the subset of an agentic-flows/v1 document NilCore consumes. It is
// decoded from JSON; unknown fields are ignored (forward-compatible with the evolving
// contract). Edges are DERIVED from each node's requires/produces dataflow.
type flowDoc struct {
	SpecVersion string `json:"spec_version"`
	ID          string `json:"id"`
	Version     string `json:"version"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	Runtime     struct {
		SupportedCores       []string `json:"supported_cores"`
		RequiredCapabilities []string `json:"required_capabilities"`
	} `json:"runtime"`
	Nodes []struct {
		ID          string   `json:"id"`
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Agent       string   `json:"agent"`
		Tool        string   `json:"tool"`
		Uses        string   `json:"uses"` // some specs name the tool under "uses"
		Requires    []string `json:"requires"`
		Produces    []string `json:"produces"`
	} `json:"nodes"`
}

// toAdapterFlow converts the decoded document into the pure adapter's Flow shape,
// deriving edges from the produces→requires dataflow (a node that requires an artifact
// depends on the node that produces it). A tool node with no explicit tool/uses name
// falls back to its node id so the sandbox-plan step has a stable handle.
func (d flowDoc) toAdapterFlow() agenticflows.Flow {
	producer := map[string]string{} // artifact -> producing node id
	for _, n := range d.Nodes {
		for _, art := range n.Produces {
			if _, seen := producer[art]; !seen {
				producer[art] = n.ID
			}
		}
	}
	var edges []agenticflows.Edge
	seen := map[string]bool{}
	for _, n := range d.Nodes {
		for _, art := range n.Requires {
			from, ok := producer[art]
			if !ok || from == n.ID {
				continue
			}
			key := from + "->" + n.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, agenticflows.Edge{From: from, To: n.ID})
		}
	}
	nodes := make([]agenticflows.Node, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		tool := n.Tool
		if tool == "" {
			tool = n.Uses
		}
		if tool == "" && n.Type == "tool" {
			tool = n.ID
		}
		nodes = append(nodes, agenticflows.Node{
			ID: n.ID, Type: n.Type, Title: n.Title, Description: n.Description, Agent: n.Agent, Tool: tool,
		})
	}
	return agenticflows.Flow{
		ID: d.ID, Version: d.Version,
		Runtime: agenticflows.Runtime{SupportedCores: d.Runtime.SupportedCores, RequiredCapabilities: d.Runtime.RequiredCapabilities},
		Nodes:   nodes, Edges: edges,
	}
}

// loadFlow reads and decodes a flow JSON file.
func loadFlow(path string) (flowDoc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return flowDoc{}, fmt.Errorf("read flow %s: %w", path, err)
	}
	var d flowDoc
	if err := json.Unmarshal(raw, &d); err != nil {
		return flowDoc{}, fmt.Errorf("decode flow %s (NilCore consumes the JSON form; convert a flow.yaml first): %w", path, err)
	}
	if len(d.Nodes) == 0 {
		return flowDoc{}, fmt.Errorf("flow %s has no nodes", path)
	}
	return d, nil
}

// flowsMain dispatches `nilcore flows <validate|run> -flow <file.json> [-dir <repo>]`.
// The verb comes first (git-style), then flags parsed via flag.NewFlagSet — the same
// flag machinery every other subcommand uses.
func flowsMain(args []string) {
	if len(args) == 0 {
		flowsUsage()
		os.Exit(2)
	}
	verb := args[0]
	fs := flag.NewFlagSet("flows "+verb, flag.ExitOnError)
	flowPath := fs.String("flow", "", "path to the flow JSON (required)")
	dir := fs.String("dir", ".", "repo directory to run the flow against (run only)")
	fs.Usage = flowsUsage
	_ = fs.Parse(args[1:])

	if *flowPath == "" {
		fmt.Fprintln(os.Stderr, "error: -flow <file.json> is required")
		flowsUsage()
		os.Exit(2)
	}

	doc, err := loadFlow(*flowPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	flow := doc.toAdapterFlow()

	switch verb {
	case "validate":
		os.Exit(flowsValidate(doc, flow))
	case "run":
		flowsRun(doc, flow, *dir)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown flows verb %q\n", verb)
		flowsUsage()
		os.Exit(2)
	}
}

// flowsValidate reports whether NilCore can consume the flow and prints the plan. It
// returns a process exit code: 0 consumable, 1 not (a preflight gate the operator/CI
// can branch on). NO execution.
func flowsValidate(doc flowDoc, flow agenticflows.Flow) int {
	fmt.Printf("flow: %s@%s  (%s)\n", doc.ID, doc.Version, doc.Title)
	ok := 0

	if !agenticflows.SupportsNilCore(flow) {
		fmt.Printf("  ✗ does not list \"nilcore\" in runtime.supported_cores (%s)\n", strings.Join(flow.Runtime.SupportedCores, ", "))
		ok = 1
	} else {
		fmt.Println("  ✓ lists nilcore as a supported core")
	}

	if missing := agenticflows.UnsupportedCapabilities(flow, nilcoreCapabilities); len(missing) > 0 {
		fmt.Printf("  ✗ requires capabilities NilCore does not advertise: %s\n", strings.Join(missing, ", "))
		ok = 1
	} else if len(flow.Runtime.RequiredCapabilities) > 0 {
		fmt.Println("  ✓ every required capability is supported")
	}

	// Worker-dispatch plan (agent_task → subtasks). A flow with no agent_task nodes is
	// not an error for validate — it simply has nothing for NilCore's worker tier to run.
	if subs, serr := agenticflows.AgentTaskSubtasks(flow, summarize.ContextSummary{}); serr == nil {
		fmt.Printf("  worker dispatch: %d agent_task node(s)\n", len(subs))
		for _, s := range subs {
			dep := ""
			if len(s.DependsOn) > 0 {
				dep = "  (after: " + strings.Join(s.DependsOn, ", ") + ")"
			}
			fmt.Printf("    - %s: %s%s\n", s.ID, truncFlow(s.Goal, 80), dep)
		}
	}

	// Sandbox tool plans (tool → in-box exec). Reported for visibility; not an error.
	if plans, perr := agenticflows.ToolSandboxPlans(flow); perr == nil && len(plans) > 0 {
		names := make([]string, 0, len(plans))
		for _, p := range plans {
			names = append(names, p.Tool)
		}
		sort.Strings(names)
		fmt.Printf("  sandbox tools: %s\n", strings.Join(names, ", "))
	}

	if ok == 0 {
		fmt.Println("  → consumable by NilCore")
	} else {
		fmt.Println("  → NOT consumable by NilCore (see ✗ above)")
	}
	return ok
}

// flowsRun executes a consumable flow's agent_task nodes through the proven decompose
// preset: it composes one sub-goal per agent_task (dependency order) into a multi-line
// goal, which decomposePlan splits back into independent verified sub-runs that are
// integrated into one re-verified tip (I2). It fails closed if the flow is not
// consumable. Tool/verify/approval nodes are honored by the run machinery itself (the
// sandbox, the verifier, the human gate) — this command does not pre-execute them.
func flowsRun(doc flowDoc, flow agenticflows.Flow, dir string) {
	if !agenticflows.SupportsNilCore(flow) {
		fmt.Fprintf(os.Stderr, "error: flow %s does not support nilcore (runtime.supported_cores)\n", doc.ID)
		os.Exit(1)
	}
	if missing := agenticflows.UnsupportedCapabilities(flow, nilcoreCapabilities); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "error: flow %s requires unsupported capabilities: %s\n", doc.ID, strings.Join(missing, ", "))
		os.Exit(1)
	}
	subs, err := agenticflows.AgentTaskSubtasks(flow, summarize.ContextSummary{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: flow %s has no runnable agent_task nodes: %v\n", doc.ID, err)
		os.Exit(1)
	}

	// Compose the worker-dispatch goal: the flow title as context, then one line per
	// agent_task (decomposePlan splits on newlines, so each becomes an independent
	// verified sub-run). Dependency order is preserved by AgentTaskSubtasks' topological
	// shape — the integrator re-verifies after each merge regardless.
	var b strings.Builder
	if doc.Title != "" {
		fmt.Fprintf(&b, "%s\n", doc.Title)
	}
	for _, s := range subs {
		fmt.Fprintf(&b, "%s\n", s.Goal)
	}
	goal := strings.TrimSpace(b.String())

	fmt.Fprintf(os.Stderr, "nilcore flows: running %s@%s as %d worker sub-task(s) via decompose\n", doc.ID, doc.Version, len(subs))
	// Reuse the verified decompose entrypoint wholesale (no new dispatch loop): it owns
	// the worktrees, the per-child verify, the integration + re-verify, and the gate.
	decomposeMain([]string{"-goal", goal, "-dir", dir})
}

func truncFlow(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func flowsUsage() {
	fmt.Fprint(os.Stderr, `nilcore flows — consume a portable agentic-flows workflow (github.com/RNT56/agentic-flows)

NilCore is the sandboxed-worker consumer of the agentic-flows contract: it runs a flow's
agent_task nodes through its verified machinery. Flows are consumed as JSON (stdlib-only;
convert a flow.yaml with flowctl first).

Usage:
  nilcore flows validate -flow <file.json>            preflight: can NilCore consume it? (no execution)
  nilcore flows run      -flow <file.json> [-dir .]    run the agent_task nodes via the decompose preset

Exit codes (validate): 0 = consumable, 1 = not consumable / decode error.
`)
}
