package model

import "encoding/json"

// BuiltinTool is a PROVIDER BUILT-IN tool — a tool whose action vocabulary is baked
// into the model rather than described by a caller-supplied JSON schema. The flagship
// is Anthropic's `computer` beta tool (Path A of desktop computer use, CU-T12): a
// typed tool declared with display dimensions and gated behind a beta header.
//
// It is the LONE frozen-contract addition for computer use and is OFF the default
// path: a model.Tool carries a nil Builtin in every existing code path, so the
// generic-tool path (Path B) and every non-computer tool serialize byte-identically.
// Only when an operator opts into Path A (NILCORE_COMPUTER_NATIVE) is a Builtin set,
// and only the Anthropic provider acts on it; other providers ignore it.
type BuiltinTool struct {
	// Type is the provider's typed-tool identifier, e.g. "computer_20251124".
	Type string
	// Name is the tool name the model emits in tool_use, e.g. "computer".
	Name string
	// DisplayWidthPx/DisplayHeightPx MUST equal the pixel dimensions of the screenshots
	// the harness sends (the model emits coordinates in this space; mismatch is the #1
	// mis-click bug). Zero ⇒ omitted (a tool that needs no display, e.g. bash).
	DisplayWidthPx  int
	DisplayHeightPx int
	// Beta is the anthropic-beta header value enabling this tool version, e.g.
	// "computer-use-2025-11-24". Web search is GA on Anthropic, so it carries none.
	Beta string
	// MaxUses caps how many times a server-side tool (web search) may run per turn.
	// Zero ⇒ omitted (provider default). Display tools leave it zero.
	MaxUses int
}

// Computer tool versions + beta headers (Anthropic). The version↔header↔model triple
// must match; the wiring picks the right one for the configured model (CU-T12).
const (
	ComputerToolV20251124 = "computer_20251124"
	ComputerBeta20251124  = "computer-use-2025-11-24"
	ComputerToolV20250124 = "computer_20250124"
	ComputerBeta20250124  = "computer-use-2025-01-24"
)

// Web-search built-in (Phase 15). Anthropic renders it as a tools[] entry with this
// typed identifier (GA — no beta header); OpenAI/OpenRouter ignore the Type and
// render their own request-level shape (web_search_options / web plugin), keyed off
// the "web_search" Name. The model emits a `web_search` tool_use the PROVIDER
// fulfils server-side — the harness makes no HTTP call.
const (
	WebSearchToolType = "web_search_20250305" // Anthropic native web search (GA)
	WebSearchName     = "web_search"
)

// NewWebSearchTool builds the provider-native web-search built-in. maxUses<=0 ⇒ the
// provider's default cap. It carries no display dims and no beta header.
func NewWebSearchTool(maxUses int) Tool {
	return Tool{
		Name: WebSearchName,
		Builtin: &BuiltinTool{
			Type:    WebSearchToolType,
			Name:    WebSearchName,
			MaxUses: maxUses,
		},
	}
}

// IsWebSearch reports whether a tool advertises the native web-search built-in (so a
// provider adapter can lift it out of the generic tools[] path and render its own
// request-level shape).
func (t Tool) IsWebSearch() bool {
	return t.Builtin != nil && t.Builtin.Name == WebSearchName
}

// NewComputerTool builds the Anthropic native `computer` built-in tool for the given
// display dimensions (the resized screenshot's pixel size the harness sends).
func NewComputerTool(displayW, displayH int) Tool {
	return Tool{
		Name: "computer",
		Builtin: &BuiltinTool{
			Type:            ComputerToolV20251124,
			Name:            "computer",
			DisplayWidthPx:  displayW,
			DisplayHeightPx: displayH,
			Beta:            ComputerBeta20251124,
		},
	}
}

// MarshalJSON renders a Builtin tool in the provider's typed-tool shape; for a normal
// tool (Builtin == nil) it renders exactly the {name, description, input_schema}
// shape the existing struct tags produce — so the default path is byte-identical.
func (t Tool) MarshalJSON() ([]byte, error) {
	if t.Builtin != nil {
		m := map[string]any{"type": t.Builtin.Type, "name": t.Builtin.Name}
		if t.Builtin.DisplayWidthPx > 0 {
			m["display_width_px"] = t.Builtin.DisplayWidthPx
		}
		if t.Builtin.DisplayHeightPx > 0 {
			m["display_height_px"] = t.Builtin.DisplayHeightPx
		}
		if t.Builtin.MaxUses > 0 {
			m["max_uses"] = t.Builtin.MaxUses
		}
		return json.Marshal(m)
	}
	type alias Tool // avoid recursion; the alias has no custom MarshalJSON
	return json.Marshal(alias{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
}

// BetaHeader returns the anthropic-beta value a builtin tool requires, or "".
func (t Tool) BetaHeader() string {
	if t.Builtin != nil {
		return t.Builtin.Beta
	}
	return ""
}
