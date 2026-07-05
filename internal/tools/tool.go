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
	"os"

	"nilcore/internal/model"
)

// validToolName reports whether name matches the provider's tool-name contract
// ^[a-zA-Z0-9_-]{1,64}$. A single tool with an out-of-spec name (e.g. a skill
// whose SKILL.md frontmatter name carried a space or non-ASCII rune) would make
// the ENTIRE Messages request invalid, 400-ing every model call. Defs() uses this
// to skip+warn such a tool so one bad tool can never poison the whole request.
func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Tool is one structured capability. Run executes it against workdir with the
// model's JSON input and returns a human/model-readable result.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Run(ctx context.Context, workdir string, input json.RawMessage) (string, error)
}

// Image is an optional image a tool produces alongside its text result (e.g. a
// browser screenshot), to be handed to a vision-capable model as a follow-up user
// turn (D1-T02). Base64 is the raw base64-encoded image bytes; MediaType is e.g.
// "image/png".
type Image struct {
	MediaType string
	Base64    string
}

// ImageRunner is an OPTIONAL Tool capability. RunWithImage returns the same text
// result Run does, plus an optional captured image. The loop, after dispatching,
// hands a non-nil image to the model as a user image block (D1-T02). A Tool that
// does not implement ImageRunner delivers text only — byte-identical. The seam is
// stateless: the image is returned from the call, never stored on the tool.
type ImageRunner interface {
	RunWithImage(ctx context.Context, workdir string, input json.RawMessage) (string, *Image, error)
}

// BuiltinProvider is an OPTIONAL Tool capability: a tool that is a PROVIDER BUILT-IN
// (e.g. Anthropic's native `computer` beta tool, Path A of desktop computer use).
// When a tool implements it, Defs() carries the returned *model.BuiltinTool onto the
// advertised model.Tool, so the provider serializes the typed shape + sets the beta
// header. A tool that does not implement it is a normal tool — byte-identical. nil
// return ⇒ normal tool too. The seam is additive and off every default path.
type BuiltinProvider interface {
	BuiltinDef() *model.BuiltinTool
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

// Tools returns the registered Tool values in registration order. It lets a
// caller derive a NEW registry from an existing one (e.g. a per-worker clone
// that adds a box-bound tool) without aliasing the original's map — important
// for the read-only roles, whose curated registry is shared across workers and
// must never be mutated in place.
func (r *Registry) Tools() []Tool {
	if r == nil {
		return nil
	}
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}

// Defs returns the tool definitions to advertise to the model, in order.
func (r *Registry) Defs() []model.Tool {
	if r == nil {
		return nil
	}
	defs := make([]model.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		// Defense-in-depth: never advertise a tool whose name violates the provider's
		// ^[a-zA-Z0-9_-]{1,64}$ contract — a single bad name (e.g. a malformed installed
		// skill) invalidates the whole request and 400s every model call. Skip+warn it so
		// the valid tools still ship.
		if !validToolName(t.Name()) {
			fmt.Fprintf(os.Stderr, "nilcore: skipping tool %q: name violates the provider tool-name contract [a-zA-Z0-9_-]{1,64}\n", t.Name())
			continue
		}
		def := model.Tool{Name: t.Name(), Description: t.Description(), InputSchema: t.Schema()}
		// A provider built-in (e.g. the native `computer` tool) carries its typed def
		// so the provider serializes the builtin shape + sets the beta header (Path A).
		if bp, ok := t.(BuiltinProvider); ok {
			def.Builtin = bp.BuiltinDef()
		}
		defs = append(defs, def)
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

// DispatchRich runs the named tool and, when the tool implements ImageRunner,
// returns its optional captured image alongside the text. For every other tool the
// image is nil and the behavior is exactly Dispatch — so the loop can always call
// DispatchRich and append an image only when one is produced (D1-T02).
func (r *Registry) DispatchRich(ctx context.Context, name, workdir string, input json.RawMessage) (string, *Image, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", nil, fmt.Errorf("unknown tool %q", name)
	}
	if ir, ok := t.(ImageRunner); ok {
		return ir.RunWithImage(ctx, workdir, input)
	}
	out, err := t.Run(ctx, workdir, input)
	return out, nil, err
}
