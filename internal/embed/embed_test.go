package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilcore/internal/codeintel/semantic"
)

// Compile-time proof the embedder satisfies the semantic.Embedder seam.
var _ semantic.Embedder = (*OpenAIEmbedder)(nil)

func TestEmbedSendsRequestAndDecodesVector(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	e := &OpenAIEmbedder{Key: "sk-secret", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	vec, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Fatalf("vec = %v", vec)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["model"] != "m" || body["input"] != "hello world" {
		t.Errorf("body = %v", body)
	}
}

func TestEmbedErrorDoesNotLeakKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()
	e := &OpenAIEmbedder{Key: "sk-secret", BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := e.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("error leaked key: %v", err)
	}
}

func TestEmbedEmptyVectorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	e := &OpenAIEmbedder{BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := e.Embed(context.Background(), "x"); err == nil {
		t.Fatal("empty data must be an error")
	}
}

func TestNewOpenAIDefaults(t *testing.T) {
	e := NewOpenAI("k", "")
	if e.Model != DefaultModel {
		t.Errorf("default model = %q", e.Model)
	}
}

// TestEmbedderIsProviderAgnostic pins the embedder to the OpenAI-compatible wire
// shape (OpenAI / OpenRouter / self-hosted), NOT any single vendor. It is the
// regression for the stale "the semantic embedder is Anthropic-tied" assumption:
// the default endpoint is the OpenAI-compatible /v1 base and the BaseURL is freely
// overridable, so swapping providers needs no code change here.
func TestEmbedderIsProviderAgnostic(t *testing.T) {
	e := NewOpenAI("k", "m")
	if e.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("default BaseURL = %q, want the OpenAI-compatible /v1 base", e.BaseURL)
	}
	if strings.Contains(strings.ToLower(e.BaseURL), "anthropic") {
		t.Errorf("BaseURL must not be Anthropic-tied: %q", e.BaseURL)
	}

	// The endpoint is BaseURL + /embeddings; an overridden base (e.g. OpenRouter or
	// a self-hosted server) is honored with no other change.
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1]}]}`))
	}))
	defer srv.Close()
	e2 := NewOpenAI("k", "m")
	e2.BaseURL = srv.URL
	e2.HTTP = srv.Client()
	if _, err := e2.Embed(context.Background(), "x"); err != nil {
		t.Fatalf("embed against an overridden base: %v", err)
	}
	if gotPath != "/embeddings" {
		t.Errorf("endpoint path = %q, want /embeddings off the overridden base", gotPath)
	}
}
