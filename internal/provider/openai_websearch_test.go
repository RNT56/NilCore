package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// captureBodyTools captures the marshalled request body for a Complete call carrying
// the given tools (the maxtokens helper passes nil tools).
func captureBodyTools(t *testing.T, o *OpenAI, tools []model.Tool) string {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	o.baseURL = srv.URL
	_, err := o.Complete(context.Background(), "sys",
		[]model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}}}, tools, 100)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return body
}

func TestWebSearchRenderOpenAI(t *testing.T) {
	o := NewOpenAICompatible("gpt-4o-search-preview", WithKey("k"))
	body := captureBodyTools(t, o, []model.Tool{
		model.NewWebSearchTool(5),
		{Name: "foo", Description: "d", InputSchema: []byte(`{"type":"object"}`)},
	})
	if !strings.Contains(body, `"web_search_options":{}`) {
		t.Fatalf("OpenAI web search should set web_search_options: %s", body)
	}
	// The web-search builtin is lifted OUT of tools[]; only the function tool remains
	// (web_search_options is a top-level key, NOT a tool — assert on the tool name).
	if strings.Contains(body, `"name":"web_search"`) {
		t.Fatalf("web_search builtin must not appear as a function tool: %s", body)
	}
	if !strings.Contains(body, `"name":"foo"`) {
		t.Fatalf("the function tool should still be advertised: %s", body)
	}
}

func TestWebSearchRenderOpenRouter(t *testing.T) {
	o := NewOpenRouter("k", "anthropic/claude-3.5-sonnet")
	body := captureBodyTools(t, o, []model.Tool{model.NewWebSearchTool(3)})
	if !strings.Contains(body, `"plugins":[{"id":"web"}]`) {
		t.Fatalf("OpenRouter web search should add a web plugin: %s", body)
	}
	if strings.Contains(body, "web_search_options") {
		t.Fatalf("OpenRouter must NOT use web_search_options (that's OpenAI's shape): %s", body)
	}
}

func TestNoWebSearchByteIdentical(t *testing.T) {
	// With no web-search builtin, neither the OpenAI option nor the OpenRouter plugin
	// appears — the body is unchanged.
	o := NewOpenAICompatible("gpt-x", WithKey("k"))
	body := captureBodyTools(t, o, nil)
	if strings.Contains(body, "web_search_options") || strings.Contains(body, `"plugins"`) {
		t.Fatalf("no web-search builtin must leave the body web-free: %s", body)
	}
	// And a bare OpenRouter call (no extras, no web) stays plugin-free too.
	or := NewOpenRouter("k", "x/y")
	if b := captureBodyTools(t, or, nil); strings.Contains(b, `"plugins"`) {
		t.Fatalf("bare OpenRouter call must not add plugins: %s", b)
	}
}
