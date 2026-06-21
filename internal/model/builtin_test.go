package model

import (
	"encoding/json"
	"testing"
)

func TestNormalToolByteIdentical(t *testing.T) {
	// A nil-Builtin tool must serialize EXACTLY as the struct tags would — the
	// default-path byte-identical guarantee (the custom MarshalJSON must not drift).
	tool := Tool{Name: "read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}
	got, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"read","description":"read a file","input_schema":{"type":"object"}}`
	if string(got) != want {
		t.Fatalf("normal tool drifted:\n got %s\nwant %s", got, want)
	}
}

func TestBuiltinComputerTool(t *testing.T) {
	tool := NewComputerTool(1280, 800)
	got, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	// Built-in shape: type + name + display dims, NO description/input_schema.
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != "computer_20251124" || m["name"] != "computer" {
		t.Fatalf("builtin tool shape wrong: %s", got)
	}
	if m["display_width_px"].(float64) != 1280 || m["display_height_px"].(float64) != 800 {
		t.Fatalf("display dims wrong: %s", got)
	}
	if _, ok := m["input_schema"]; ok {
		t.Fatalf("builtin tool must not carry input_schema: %s", got)
	}
	if tool.BetaHeader() != ComputerBeta20251124 {
		t.Fatalf("beta header = %q, want %q", tool.BetaHeader(), ComputerBeta20251124)
	}
}

func TestNormalToolNoBetaHeader(t *testing.T) {
	if (Tool{Name: "x"}).BetaHeader() != "" {
		t.Fatal("a normal tool must require no beta header")
	}
}
