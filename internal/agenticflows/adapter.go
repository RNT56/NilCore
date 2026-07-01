// Package agenticflows maps selected agentic-flows contracts onto NilCore's
// existing supervisor and sandbox seams. It does not parse YAML directly; callers
// pass a decoded flow shape from a pinned agentic-flows commit or vendored copy.
package agenticflows

import (
	"errors"
	"fmt"
	"sort"

	"nilcore/internal/spawn"
	"nilcore/internal/summarize"
)

// Flow is the subset of an agentic-flows document NilCore needs at the adapter
// boundary.
type Flow struct {
	ID      string
	Version string
	Runtime Runtime
	Nodes   []Node
	Edges   []Edge
}

// Runtime declares consumer support and required capabilities.
type Runtime struct {
	SupportedCores       []string
	RequiredCapabilities []string
}

// Node is one agentic-flows node.
type Node struct {
	ID          string
	Type        string
	Title       string
	Description string
	Agent       string
	Tool        string
}

// Edge connects two nodes.
type Edge struct {
	From string
	To   string
}

// ToolSandboxPlan is the trusted host-side decision to execute a tool node
// through NilCore's sandbox boundary. It is INSPECTION metadata: `flows validate`
// surfaces it so an operator can see which tool nodes would need the sandbox. The
// flow's tool nodes are not pre-executed by the flows command — a tool a flow
// invokes is reached transitively as a model-emitted command inside an agent_task,
// which hits the sandbox (I4) like any other command.
type ToolSandboxPlan struct {
	NodeID          string
	Tool            string
	RequiresSandbox bool
}

// UnsupportedCapabilities returns required flow capabilities that NilCore has
// not explicitly advertised for this adapter invocation.
func UnsupportedCapabilities(flow Flow, supported map[string]bool) []string {
	var missing []string
	for _, cap := range flow.Runtime.RequiredCapabilities {
		if !supported[cap] {
			missing = append(missing, cap)
		}
	}
	sort.Strings(missing)
	return missing
}

// SupportsNilCore reports whether the source flow opted into NilCore.
func SupportsNilCore(flow Flow) bool {
	for _, core := range flow.Runtime.SupportedCores {
		if core == "nilcore" {
			return true
		}
	}
	return false
}

// AgentTaskSubtasks converts agent_task nodes into NilCore spawn subtasks, returned
// in TOPOLOGICAL order (a dependency always precedes the tasks that require it). The
// edges are the derived produces→requires dataflow, so a downstream consumer (the
// `flows run` path, which flattens the subtasks into an ordered goal list) honors the
// dependency sequence rather than running children in arbitrary node-declaration order.
// A dependency cycle is reported rather than silently dropped.
func AgentTaskSubtasks(flow Flow, base summarize.ContextSummary) ([]spawn.Subtask, error) {
	if !SupportsNilCore(flow) {
		return nil, fmt.Errorf("agentic flow %s@%s does not list nilcore as a supported core", flow.ID, flow.Version)
	}

	nodeTypes := make(map[string]string, len(flow.Nodes))
	for _, node := range flow.Nodes {
		if node.ID == "" {
			return nil, errors.New("agentic flow node missing id")
		}
		nodeTypes[node.ID] = node.Type
	}

	deps := make(map[string][]string)
	for _, edge := range flow.Edges {
		if nodeTypes[edge.From] == "agent_task" && nodeTypes[edge.To] == "agent_task" {
			deps[edge.To] = append(deps[edge.To], edge.From)
		}
	}

	var subtasks []spawn.Subtask
	for _, node := range flow.Nodes {
		if node.Type != "agent_task" {
			continue
		}
		goal := node.Description
		if goal == "" {
			goal = node.Title
		}
		if goal == "" {
			return nil, fmt.Errorf("agent_task node %s missing title or description", node.ID)
		}
		summary := base
		summary.Goal = goal
		summary.Constraints = append(
			append([]string{}, base.Constraints...),
			fmt.Sprintf("agentic-flows source: %s@%s", flow.ID, flow.Version),
		)
		subtasks = append(subtasks, spawn.Subtask{
			ID:        node.ID,
			Goal:      goal,
			DependsOn: append([]string{}, deps[node.ID]...),
			Summary:   summary,
		})
	}
	if len(subtasks) == 0 {
		return nil, errors.New("agentic flow has no agent_task nodes to dispatch")
	}
	return topoSortSubtasks(subtasks)
}

// topoSortSubtasks returns subs in dependency order: every subtask appears after the
// subtasks it DependsOn. Ties (independent tasks) keep their original relative order,
// so a flow with no inter-task edges is returned byte-identically. A cycle (no
// dependency-free task remains) is an error — the flow's dataflow is contradictory.
func topoSortSubtasks(subs []spawn.Subtask) ([]spawn.Subtask, error) {
	byID := make(map[string]spawn.Subtask, len(subs))
	indeg := make(map[string]int, len(subs))
	order := make([]string, 0, len(subs)) // stable original order
	for _, s := range subs {
		byID[s.ID] = s
		order = append(order, s.ID)
	}
	// Count only deps that reference a real agent_task subtask (a dangling DependsOn
	// is ignored, never a phantom blocker).
	dependents := make(map[string][]string, len(subs))
	for _, s := range subs {
		for _, d := range s.DependsOn {
			if _, ok := byID[d]; !ok {
				continue
			}
			indeg[s.ID]++
			dependents[d] = append(dependents[d], s.ID)
		}
	}

	out := make([]spawn.Subtask, 0, len(subs))
	emitted := make(map[string]bool, len(subs))
	for len(out) < len(subs) {
		progressed := false
		for _, id := range order { // stable: scan in original order each round
			if emitted[id] || indeg[id] != 0 {
				continue
			}
			out = append(out, byID[id])
			emitted[id] = true
			progressed = true
			for _, dep := range dependents[id] {
				indeg[dep]--
			}
		}
		if !progressed {
			return nil, errors.New("agentic flow has a dependency cycle among agent_task nodes")
		}
	}
	return out, nil
}

// ToolSandboxPlans converts tool nodes into sandbox-required execution plans.
func ToolSandboxPlans(flow Flow) ([]ToolSandboxPlan, error) {
	var plans []ToolSandboxPlan
	for _, node := range flow.Nodes {
		if node.Type != "tool" {
			continue
		}
		if node.Tool == "" {
			return nil, fmt.Errorf("tool node %s missing tool name", node.ID)
		}
		plans = append(plans, ToolSandboxPlan{
			NodeID:          node.ID,
			Tool:            node.Tool,
			RequiresSandbox: true,
		})
	}
	if len(plans) == 0 {
		return nil, errors.New("agentic flow has no tool nodes to sandbox")
	}
	return plans, nil
}
