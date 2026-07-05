// Package embed provides a provider-backed text embedder that satisfies
// semantic.Embedder, so NilCore's semantic index can be turned on with a real
// vectorizer instead of degrading to lexical search. It speaks the OpenAI-
// compatible /embeddings wire shape (which OpenAI, OpenRouter, and most self-
// hosted inference servers expose), so a single small client covers the common
// providers.
//
// The API key is held only to set a per-request header (invariant I3): it is
// never logged, never placed in a prompt, and never given to the model. Stdlib
// only (invariant I6): net/http + encoding/json.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultModel is a small, cheap embedding model that most OpenAI-compatible
// endpoints support.
const DefaultModel = "text-embedding-3-small"

// DefaultBaseURL is the OpenAI embeddings base. An operator pointing at
// OpenRouter, vLLM, Ollama, or Azure overrides it via NewOpenAIWithBase so their
// key is never POSTed to OpenAI.
const DefaultBaseURL = "https://api.openai.com/v1"

// OpenAIEmbedder calls an OpenAI-compatible /embeddings endpoint. BaseURL is
// overridable for OpenRouter / self-hosted servers and for tests.
type OpenAIEmbedder struct {
	Key     string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// NewOpenAI returns an embedder for the given key and model (empty model uses
// DefaultModel), pointed at OpenAI's endpoint (DefaultBaseURL).
func NewOpenAI(key, model string) *OpenAIEmbedder {
	return NewOpenAIWithBase(key, model, "")
}

// NewOpenAIWithBase returns an embedder for the given key and model against an
// explicit OpenAI-compatible base URL. An empty model uses DefaultModel; an empty
// baseURL uses DefaultBaseURL. This is the seam that lets an operator target
// OpenRouter / vLLM / Ollama / Azure (NILCORE_EMBED_BASE_URL) so their key is sent
// to THEIR endpoint, never silently POSTed to OpenAI. A trailing slash on baseURL
// is tolerated (Embed trims it).
func NewOpenAIWithBase(key, model, baseURL string) *OpenAIEmbedder {
	if model == "" {
		model = DefaultModel
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	return &OpenAIEmbedder{
		Key:     key,
		Model:   model,
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// Embed returns the embedding vector for text. It is the semantic.Embedder seam.
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, err := json.Marshal(map[string]any{"model": e.Model, "input": text})
	if err != nil {
		return nil, fmt.Errorf("marshal embeddings request: %w", err)
	}

	endpoint := strings.TrimRight(e.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	// Per-request header only (I3) — never logged, never persisted.
	if e.Key != "" {
		req.Header.Set("authorization", "Bearer "+e.Key)
	}

	hc := e.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("embeddings api: %s: %s", resp.Status, tailErr(string(raw)))
	}

	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(out.Data) == 0 || len(out.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embeddings api returned no vector")
	}
	return out.Data[0].Embedding, nil
}

// tailErr trims an error body so a large response cannot flood logs; the token is
// never in the body (only the header), so this is safe to surface.
func tailErr(s string) string {
	const n = 500
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
