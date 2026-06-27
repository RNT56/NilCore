// Package agenticflows maps selected agentic-flows contracts onto NilCore's
// existing supervisor and sandbox seams. It does not parse YAML directly; callers
// pass a decoded flow shape from a pinned agentic-flows commit or vendored copy.
package agenticflows

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"nilcore/internal/sandbox"
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
// through NilCore's sandbox boundary.
type ToolSandboxPlan struct {
	NodeID          string
	Tool            string
	RequiresSandbox bool
}

// ToolCommandFunc maps a trusted sandbox plan to the bounded command NilCore
// should execute inside its sandbox boundary.
type ToolCommandFunc func(ToolSandboxPlan) (string, error)

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

// AgentTaskSubtasks converts agent_task nodes into NilCore spawn subtasks.
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
	return subtasks, nil
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

// RunToolSandboxPlans executes every tool plan through NilCore's sandbox
// boundary. It refuses host-side shortcuts by requiring each plan to be marked
// sandboxed and by accepting only a sandbox.Sandbox executor.
func RunToolSandboxPlans(ctx context.Context, box sandbox.Sandbox, plans []ToolSandboxPlan, commandFor ToolCommandFunc) ([]sandbox.Result, error) {
	if box == nil {
		return nil, errors.New("agentic flow tool execution requires a sandbox")
	}
	if commandFor == nil {
		return nil, errors.New("agentic flow tool execution requires a command mapper")
	}
	if len(plans) == 0 {
		return nil, errors.New("agentic flow has no tool plans to run")
	}

	results := make([]sandbox.Result, 0, len(plans))
	for _, plan := range plans {
		if !plan.RequiresSandbox {
			return nil, fmt.Errorf("tool node %s is not marked for sandbox execution", plan.NodeID)
		}
		cmd, err := commandFor(plan)
		if err != nil {
			return nil, fmt.Errorf("tool node %s command: %w", plan.NodeID, err)
		}
		if cmd == "" {
			return nil, fmt.Errorf("tool node %s command is empty", plan.NodeID)
		}
		result, err := box.Exec(ctx, cmd)
		if err != nil {
			return nil, fmt.Errorf("tool node %s sandbox exec: %w", plan.NodeID, err)
		}
		results = append(results, result)
	}
	return results, nil
}
