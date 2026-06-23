package model

import (
	"encoding/json"
	"testing"
)

func TestWebSearchToolMarshal(t *testing.T) {
	wt := NewWebSearchTool(5)
	if !wt.IsWebSearch() {
		t.Fatal("NewWebSearchTool should report IsWebSearch")
	}
	b, err := json.Marshal(wt)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != WebSearchToolType || m["name"] != WebSearchName {
		t.Fatalf("web-search marshal = %s", b)
	}
	if m["max_uses"] != float64(5) {
		t.Fatalf("max_uses not rendered: %s", b)
	}
	// No display dims for a web-search tool.
	if _, ok := m["display_width_px"]; ok {
		t.Fatalf("web search must not carry display dims: %s", b)
	}
}

func TestWebSearchZeroMaxUsesOmitted(t *testing.T) {
	b, _ := json.Marshal(NewWebSearchTool(0))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["max_uses"]; ok {
		t.Fatalf("max_uses=0 must be omitted: %s", b)
	}
}

func TestNonWebToolNotWebSearch(t *testing.T) {
	if (Tool{Name: "read"}).IsWebSearch() {
		t.Fatal("a normal tool must not report IsWebSearch")
	}
	if NewComputerTool(1280, 800).IsWebSearch() {
		t.Fatal("the computer tool must not report IsWebSearch")
	}
}
