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
