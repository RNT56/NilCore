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
//   - run: build the agent_task subtasks into a planner.Tree and execute the flow through
//     the verified `swarm` path with the "code" preset. Swarm HONORS the flow's DependsOn
//     edges (derived from produces→requires) — a dependent agent_task is coded on the
//     INTEGRATED TIP of its dependencies (a later pass), not the original HEAD — so the
//     flow's DAG survives instead of being flattened into an unordered goal list. Each
//     shard is pack-verified and the merged tip re-verified (I2). Adds no new dispatch loop.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"nilcore/internal/agenticflows"
	"nilcore/internal/planner"
	"nilcore/internal/spawn"
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
			ID: n.ID, Type: n.Type, Title: n.Title, Description: n.Description, Tool: tool,
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
	// A help request AS the verb (`nilcore flows -h|--help|help`) prints clean usage to
	// stdout and exits 0 — it is NOT an error. Without this, "-h" is taken as the verb
	// and the run falls through to the "-flow is required" error branch (exit 2), so a
	// help ask is mislabeled as a usage error. Mirrors the top-level `nilcore -h`.
	switch verb {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, flowsUsageText)
		return
	}
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

// flowTree lifts a flow's agent_task subtasks (topologically ordered, with DependsOn +
// provenance already resolved by AgentTaskSubtasks) into a planner.Tree the swarm
// TreeSharder can shard directly. Each subtask becomes a PlanTask carrying its id, goal,
// dependency edges, and a CONTRACT-FIRST Acceptance criterion (flowAcceptance) — a flow
// node carries no explicit acceptance field, but leaving it empty would waive the
// contract-first discipline planner.Tree.Validate enforces, so we synthesize an honest,
// verifier-anchored criterion from the node's goal. The tree Goal carries the flow title
// + agentic-flows provenance for the run report. It returns an error when the resulting
// tree is not contract-valid (e.g. a missing id/goal survived the adapter), so an
// unconsumable flow fails loudly here rather than shipping an unvalidated plan. This is
// the single place the flow→DAG mapping lives, so it is unit-testable without a swarm.
func flowTree(doc flowDoc, subs []spawn.Subtask) (planner.Tree, error) {
	tasks := make([]planner.PlanTask, 0, len(subs))
	for _, s := range subs {
		tasks = append(tasks, planner.PlanTask{
			ID: s.ID, Goal: s.Goal, DependsOn: s.DependsOn, Acceptance: flowAcceptance(s.Goal),
		})
	}
	runGoal := doc.Title
	if runGoal == "" {
		runGoal = doc.ID
	}
	tree := planner.Tree{
		Goal:  fmt.Sprintf("%s — agentic-flows source: %s@%s", runGoal, doc.ID, doc.Version),
		Tasks: tasks,
	}
	if err := tree.Validate(); err != nil {
		return planner.Tree{}, fmt.Errorf("flow %s@%s: invalid task tree: %w", doc.ID, doc.Version, err)
	}
	return tree, nil
}

// flowAcceptance synthesizes a contract-first Acceptance criterion for a flow agent_task
// (which carries none of its own): the node's goal is achieved AND the shard's typed
// artifact passes the swarm's per-claim verifier — the SAME gate the run actually
// enforces (I2), stated up front so the plan is contract-valid. The goal is clipped so a
// long description never bloats the criterion; it is inert DATA (I7), only embedded.
func flowAcceptance(goal string) string {
	g := strings.ReplaceAll(strings.TrimSpace(goal), "\n", " ")
	return "the stated work is completed (\"" + truncFlow(g, 120) + "\") and its typed artifact passes the shard verifier"
}

// flowsRun executes a consumable flow's agent_task DAG through the verified swarm path
// with the "code" preset: flowTree lifts the agent_tasks into a planner.Tree whose
// DependsOn edges swarm HONORS — a dependent task is coded on the integrated tip of its
// dependencies (a later pass), not the original HEAD — so the flow's structure survives
// (unlike the flat goal list decompose collapsed it into). It fails closed if the flow is
// not consumable. It does NOT pre-execute tool/verify/approval nodes: a tool a flow needs
// is reached transitively as a model-emitted command inside an agent_task, which then
// hits the sandbox/verifier/gate like any other command.
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

	tree, err := flowTree(doc, subs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Default swarm flags (unparsed ⇒ registration defaults), targeted at the flow's repo
	// with the code preset. preset + dir are set AFTER applyConfigDefaults so the
	// flows-specific choices win over any config default.
	fs := flag.NewFlagSet("flows-swarm", flag.ContinueOnError)
	sf := registerSwarmFlags(fs)
	b := loadBoot(*sf.common.config)
	applyConfigDefaults(sf.common, b.cfg, flagsSet(fs))
	*sf.preset = "code"
	*sf.common.dir = dir
	// The tree carries the per-shard goals (via TreeSharder); this run-level goal is only
	// the report/scoreboard header (SwarmState.Goal), so give it the flow title +
	// provenance rather than leaving it blank.
	*sf.goal = tree.Goal
	log := openLog(*sf.common.logPath)
	defer log.Close()

	fmt.Fprintf(os.Stderr, "nilcore flows: running %s@%s as %d agent_task shard(s) via swarm (code preset, DAG-honoring)\n", doc.ID, doc.Version, len(tree.Tasks))
	swarmRun(swarmDeps{flags: sf, boot: b, log: log, dir: mustAbs(dir), tree: &tree})
}

func truncFlow(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

const flowsUsageText = `nilcore flows — consume a portable agentic-flows workflow (github.com/RNT56/agentic-flows)

NilCore is the sandboxed-worker consumer of the agentic-flows contract: it runs a flow's
agent_task nodes through its verified machinery. Flows are consumed as JSON (stdlib-only;
convert a flow.yaml with flowctl first).

Usage:
  nilcore flows validate -flow <file.json>            preflight: can NilCore consume it? (no execution)
  nilcore flows run      -flow <file.json> [-dir .]    run the agent_task DAG via the swarm code preset

Exit codes (validate): 0 = consumable, 1 = not consumable / decode error.
`

// flowsUsage prints usage to stderr — the ERROR path (bad/missing args). A help
// REQUEST prints the same text to stdout and exits 0 (see flowsMain).
func flowsUsage() {
	fmt.Fprint(os.Stderr, flowsUsageText)
}
