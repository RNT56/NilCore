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

// captureRequest spins up an httptest server that records the first inbound
// request's path, raw query, the configured auth header, and the body, then
// returns a minimal valid chat-completions reply. baseFn turns the server URL into
// the baseURL the adapter should be pointed at (so a trailing-slash base can be
// exercised against the same backend).
type captured struct {
	path    string
	authHdr string
	authVal string
	body    string
}

// TestNewOpenAICompatibleURLJoin asserts the outbound request path is exactly
// "<base-path>/chat/completions" for every real-world base — one join, no doubled
// slash, no injected "/v1" — including the trailing-slash Azure case.
func TestNewOpenAICompatibleURLJoin(t *testing.T) {
	// basePath is the path segment each provider's public base carries before
	// /chat/completions; the names document the real endpoint each case stands for
	// (openai api.openai.com/v1, openrouter openrouter.ai/api/v1, groq
	// api.groq.com/openai/v1, fireworks api.fireworks.ai/inference/v1, azure
	// x.openai.azure.com/openai/v1/ — the trailing-slash case).
	cases := []struct {
		name     string
		basePath string // the path portion that must precede /chat/completions
		trailing bool   // append a trailing slash to the (test-server) base
	}{
		{"openai", "/v1", false},
		{"openrouter", "/api/v1", false},
		{"groq", "/openai/v1", false},
		{"fireworks", "/inference/v1", false},
		{"azure-trailing-slash", "/openai/v1", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			// The test server has no path of its own; graft the provider's path
			// segment on so the join behavior under TrimRight is what is exercised.
			base := srv.URL + c.basePath
			if c.trailing {
				base += "/"
			}
			o := NewOpenAICompatible("m", WithKey("k"), WithBaseURL(base))
			if _, err := o.Complete(context.Background(), "", nil, nil, 10); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			want := c.basePath + "/chat/completions"
			if gotPath != want {
				t.Errorf("path = %q, want %q (no doubled slash, no injected /v1)", gotPath, want)
			}
			if strings.Contains(gotPath, "//") {
				t.Errorf("path %q contains a doubled slash", gotPath)
			}
		})
	}
}

// TestNewOpenAICompatibleAuth covers the three auth schemes: Bearer (default),
// Azure (api-key, raw value), and None (empty key emits NO auth header at all).
func TestNewOpenAICompatibleAuth(t *testing.T) {
	cases := []struct {
		name       string
		opts       []Option
		wantHeader string // header expected to carry the credential ("" = none)
		wantValue  string
		absent     []string // headers that must NOT be present
	}{
		{
			name:       "bearer-default",
			opts:       []Option{WithKey("secret")},
			wantHeader: "Authorization",
			wantValue:  "Bearer secret",
		},
		{
			name:       "azure-api-key",
			opts:       []Option{WithKey("secret"), WithAuth("api-key", "")},
			wantHeader: "Api-Key",
			wantValue:  "secret",
			absent:     []string{"Authorization"},
		},
		{
			name:   "none-empty-key",
			opts:   []Option{}, // no key at all
			absent: []string{"Authorization", "Api-Key"},
		},
		{
			name:   "none-explicit-empty-key",
			opts:   []Option{WithKey("")},
			absent: []string{"Authorization", "Api-Key"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got captured
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.authVal = r.Header.Get("authorization")
				if c.wantHeader != "" {
					got.authVal = r.Header.Get(c.wantHeader)
				}
				for _, h := range c.absent {
					if v := r.Header.Get(h); v != "" {
						t.Errorf("header %q present (%q), want absent", h, v)
					}
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()

			opts := append([]Option{WithBaseURL(srv.URL)}, c.opts...)
			o := NewOpenAICompatible("m", opts...)
			if _, err := o.Complete(context.Background(), "", nil, nil, 10); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if c.wantHeader != "" && got.authVal != c.wantValue {
				t.Errorf("%s = %q, want %q", c.wantHeader, got.authVal, c.wantValue)
			}
		})
	}
}

// TestNewOpenAIByteIdentical proves that with default options the outbound request
// body AND headers are byte-for-byte identical between today's NewOpenAI /
// NewOpenRouter and the new NewOpenAICompatible-backed re-expression: the body and
// the authorization header captured on the wire match exactly.
func TestNewOpenAIByteIdentical(t *testing.T) {
	msgs := []model.Message{
		{Role: "user", Content: []model.Block{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "tc1", Name: "run", Input: jsonRaw(`{"cmd":"ls"}`)}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "tc1", Content: "out"}}},
	}
	tools := []model.Tool{{Name: "run", Description: "d", InputSchema: jsonRaw(`{"type":"object"}`)}}

	// capture the exact wire body + auth header an adapter produces.
	capture := func(build func(base string) *OpenAI) captured {
		var got captured
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got.path = r.URL.Path
			got.authVal = r.Header.Get("authorization")
			got.authHdr = r.Header.Get("content-type")
			b, _ := io.ReadAll(r.Body)
			got.body = string(b)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		}))
		defer srv.Close()
		o := build(srv.URL)
		if _, err := o.Complete(context.Background(), "sys", msgs, tools, 100); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		return got
	}

	// NewOpenAI vs. an explicit default NewOpenAICompatible: identical wire shape.
	gotLegacy := capture(func(base string) *OpenAI {
		o := NewOpenAI("k", "gpt-x")
		o.baseURL = base
		return o
	})
	gotNew := capture(func(base string) *OpenAI {
		return NewOpenAICompatible("gpt-x", WithKey("k"), WithBaseURL(base))
	})

	if gotLegacy.body != gotNew.body {
		t.Errorf("body differs:\n legacy: %s\n new:    %s", gotLegacy.body, gotNew.body)
	}
	if gotLegacy.authVal != gotNew.authVal {
		t.Errorf("authorization differs: legacy %q vs new %q", gotLegacy.authVal, gotNew.authVal)
	}
	if gotLegacy.authVal != "Bearer k" {
		t.Errorf("authorization = %q, want \"Bearer k\" (byte-identical to today)", gotLegacy.authVal)
	}
	if gotLegacy.authHdr != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotLegacy.authHdr)
	}
	if gotLegacy.path != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotLegacy.path)
	}

	// NewOpenRouter byte-identical to its NewOpenAICompatible re-expression.
	gotRouterLegacy := capture(func(base string) *OpenAI {
		o := NewOpenRouter("k", "meta-llama/llama-3.1-70b")
		o.baseURL = base
		return o
	})
	gotRouterNew := capture(func(base string) *OpenAI {
		return NewOpenAICompatible("meta-llama/llama-3.1-70b", WithKey("k"), WithBaseURL(base))
	})
	if gotRouterLegacy.body != gotRouterNew.body {
		t.Errorf("openrouter body differs:\n legacy: %s\n new:    %s", gotRouterLegacy.body, gotRouterNew.body)
	}
	if gotRouterLegacy.authVal != "Bearer k" {
		t.Errorf("openrouter authorization = %q, want \"Bearer k\"", gotRouterLegacy.authVal)
	}
}

// TestNewOpenRouterDefaults proves the public defaults survive: OpenRouter's base
// URL and Fusion fallback model are unchanged, and Bearer auth is intact.
func TestNewOpenRouterDefaults(t *testing.T) {
	o := NewOpenRouter("k", "")
	if o.Model() != DefaultOpenRouterModel {
		t.Errorf("empty model = %q, want %q", o.Model(), DefaultOpenRouterModel)
	}
	if o.baseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("baseURL = %q, want https://openrouter.ai/api/v1", o.baseURL)
	}
	if o.authHeader != "authorization" || o.authPrefix != "Bearer " {
		t.Errorf("auth = %q/%q, want authorization/\"Bearer \"", o.authHeader, o.authPrefix)
	}

	d := NewOpenAI("k", "gpt-x")
	if d.baseURL != "https://api.openai.com/v1" {
		t.Errorf("openai baseURL = %q, want https://api.openai.com/v1", d.baseURL)
	}
	if d.maxTokensField != "max_tokens" {
		t.Errorf("maxTokensField = %q, want max_tokens", d.maxTokensField)
	}
}

// TestWithMaxTokensFieldStoredNotApplied proves WithMaxTokensField records the
// field name but does NOT yet change how the body marshals — the wire body is
// still "max_tokens" (the field-name switch lands in a later task).
func TestWithMaxTokensFieldStoredNotApplied(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	o := NewOpenAICompatible("m", WithKey("k"), WithBaseURL(srv.URL), WithMaxTokensField("max_completion_tokens"))
	if o.maxTokensField != "max_completion_tokens" {
		t.Errorf("maxTokensField = %q, want max_completion_tokens (stored)", o.maxTokensField)
	}
	if _, err := o.Complete(context.Background(), "", nil, nil, 100); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !strings.Contains(body, `"max_tokens":100`) {
		t.Errorf("body = %s, want it to still carry \"max_tokens\":100 (field switch not yet wired)", body)
	}
	if strings.Contains(body, "max_completion_tokens") {
		t.Errorf("body = %s, must NOT yet emit max_completion_tokens", body)
	}
}

// jsonRaw is a tiny helper so the table data reads cleanly.
func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }
