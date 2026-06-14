// Package tools is the registry and the structured, auditable tools the native
// loop prefers over the raw shell escape hatch: read, write, edit, search, git.
// Adding a tool means registering it — the loop loads its definitions and
// dispatches calls through the registry, so the core loop never changes
// (CLAUDE.md tool-surface design; P1-T08). Each tool declares a JSON schema and
// operates against a worktree directory; paths are confined to that directory so
// access stays inspectable (the Phase-2 policy engine scopes them precisely).
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"nilcore/internal/model"
)

// Tool is one structured capability. Run executes it against workdir with the
// model's JSON input and returns a human/model-readable result.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Run(ctx context.Context, workdir string, input json.RawMessage) (string, error)
}

// Registry holds the registered tools in a stable order.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry builds a registry from the given tools (registration order kept).
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{tools: map[string]Tool{}}
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

// Register adds (or replaces) a tool.
func (r *Registry) Register(t Tool) {
	if _, ok := r.tools[t.Name()]; !ok {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// Has reports whether a tool with this name is registered.
func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.tools[name]
	return ok
}

// Defs returns the tool definitions to advertise to the model, in order.
func (r *Registry) Defs() []model.Tool {
	if r == nil {
		return nil
	}
	defs := make([]model.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, model.Tool{Name: t.Name(), Description: t.Description(), InputSchema: t.Schema()})
	}
	return defs
}

// Dispatch runs the named tool against workdir.
func (r *Registry) Dispatch(ctx context.Context, name, workdir string, input json.RawMessage) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return t.Run(ctx, workdir, input)
}
