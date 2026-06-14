package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilcore/internal/model"
)

func TestAnthropicComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			t.Errorf("missing api key header")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		var req anthropicRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "claude-x" {
			t.Errorf("model = %q", req.Model)
		}
		if len(req.Messages) != 1 || len(req.Tools) != 1 {
			t.Errorf("messages/tools = %d/%d", len(req.Messages), len(req.Tools))
		}
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"run","input":{"cmd":"ls"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`)
	}))
	defer srv.Close()

	a := NewAnthropic("k", "claude-x")
	a.baseURL = srv.URL
	resp, err := a.Complete(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}},
		[]model.Tool{{Name: "run", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}, 100)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) != 2 || resp.Content[0].Text != "hi" || resp.Content[1].Name != "run" {
		t.Fatalf("response content = %+v", resp.Content)
	}
	if resp.StopReason != "tool_use" || resp.Usage.OutputTokens != 7 {
		t.Errorf("stop/usage = %q/%d", resp.StopReason, resp.Usage.OutputTokens)
	}
}

func TestOpenAITranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer k" {
			t.Errorf("authorization = %q", r.Header.Get("authorization"))
		}
		var req oaiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		// system + user + assistant(tool_calls) + tool
		var sawSystem, sawTool, sawAssistantToolCall bool
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				sawSystem = true
			case "tool":
				if m.ToolCallID == "tc1" && m.Content == "out" {
					sawTool = true
				}
			case "assistant":
				if len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "tc1" {
					sawAssistantToolCall = true
				}
			}
		}
		if !sawSystem || !sawTool || !sawAssistantToolCall {
			t.Errorf("translation gaps: system=%v tool=%v asstToolCall=%v", sawSystem, sawTool, sawAssistantToolCall)
		}
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "run" {
			t.Errorf("tools = %+v", req.Tools)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"run","arguments":"{\"cmd\":\"pwd\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	}))
	defer srv.Close()

	o := NewOpenAI("k", "gpt-x")
	o.baseURL = srv.URL
	msgs := []model.Message{
		{Role: "user", Content: []model.Block{{Type: "text", Text: "do it"}}},
		{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "tc1", Name: "run", Input: json.RawMessage(`{"cmd":"ls"}`)}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "tc1", Content: "out"}}},
	}
	resp, err := o.Complete(context.Background(), "sys", msgs,
		[]model.Tool{{Name: "run", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}, 100)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" || resp.Content[0].ID != "c1" {
		t.Fatalf("response = %+v", resp.Content)
	}
	if !strings.Contains(string(resp.Content[0].Input), "pwd") {
		t.Errorf("tool input = %s", resp.Content[0].Input)
	}
	if resp.StopReason != "tool_use" || resp.Usage.OutputTokens != 4 {
		t.Errorf("stop/usage = %q/%d", resp.StopReason, resp.Usage.OutputTokens)
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("OPENAI_API_KEY", "o")
	t.Setenv("OPENROUTER_API_KEY", "r")

	cases := []struct {
		spec      string
		wantModel string
	}{
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"anthropic:claude-opus-4-8", "claude-opus-4-8"},
		{"openai:gpt-5.5", "gpt-5.5"},
		{"openrouter:meta-llama/llama-3.1-70b", "meta-llama/llama-3.1-70b"},
		{"openrouter", "openrouter/fusion"},  // bare provider → Fusion default
		{"openrouter:", "openrouter/fusion"}, // empty model → Fusion default
	}
	for _, c := range cases {
		p, err := Resolve(c.spec)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", c.spec, err)
		}
		if p.Model() != c.wantModel {
			t.Errorf("Resolve(%q).Model() = %q, want %q", c.spec, p.Model(), c.wantModel)
		}
	}

	if _, err := Resolve("bogus:x"); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestResolveMissingKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := Resolve("anthropic:claude-x"); err == nil {
		t.Error("expected error when key is absent")
	}
}

// TestResolveWith proves the injected key lookup is honored (not the process
// environment), so the composition root can source keys from a SecretStore.
func TestResolveWith(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "") // ensure the real environment is empty
	lookup := func(name string) string {
		if name == "OPENROUTER_API_KEY" {
			return "from-lookup"
		}
		return ""
	}
	p, err := ResolveWith("openrouter", lookup)
	if err != nil {
		t.Fatalf("ResolveWith: %v", err)
	}
	if p.Model() != "openrouter/fusion" {
		t.Errorf("model = %q, want openrouter/fusion", p.Model())
	}
	if _, err := ResolveWith("anthropic:claude-x", func(string) string { return "" }); err == nil {
		t.Error("expected error when the lookup yields no key")
	}
}
