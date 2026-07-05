package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Finding 1: header secret placeholder resolution (I3) -------------------

// fakeResolver is a minimal SecretResolver over an in-memory map.
type fakeResolver struct{ m map[string]string }

func (f fakeResolver) Get(name string) (string, error) {
	if v, ok := f.m[name]; ok {
		return v, nil
	}
	return "", errors.New("secret not found")
}

func TestResolveSecretsHeaderPlaceholder(t *testing.T) {
	cfg := Config{Servers: []ServerSpec{{
		Name:    "remote",
		URL:     "https://mcp.example.com",
		Headers: map[string]string{"Authorization": "Bearer {{secret:MY_TOKEN}}", "X-Static": "literal"},
	}}}
	resolved, err := cfg.ResolveSecrets(fakeResolver{m: map[string]string{"MY_TOKEN": "s3cr3t"}})
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	got := resolved.Servers[0].Headers["Authorization"]
	if got != "Bearer s3cr3t" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer s3cr3t")
	}
	if resolved.Servers[0].Headers["X-Static"] != "literal" {
		t.Errorf("static header must pass through verbatim, got %q", resolved.Servers[0].Headers["X-Static"])
	}
}

func TestResolveSecretsEnvPlaceholder(t *testing.T) {
	t.Setenv("MY_MCP_ENV_TOKEN", "envval")
	cfg := Config{Servers: []ServerSpec{{
		Name:    "remote",
		URL:     "https://mcp.example.com",
		Headers: map[string]string{"Authorization": "Bearer {{env:MY_MCP_ENV_TOKEN}}"},
	}}}
	resolved, err := cfg.ResolveSecrets(nil) // env placeholder needs no resolver
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if got := resolved.Servers[0].Headers["Authorization"]; got != "Bearer envval" {
		t.Errorf("env placeholder = %q, want %q", got, "Bearer envval")
	}
}

func TestResolveSecretsUnresolvedIsError(t *testing.T) {
	// A missing secret must be a hard error, never silently sent as the literal placeholder.
	cfg := Config{Servers: []ServerSpec{{
		Name:    "remote",
		URL:     "https://mcp.example.com",
		Headers: map[string]string{"Authorization": "Bearer {{secret:ABSENT}}"},
	}}}
	if _, err := cfg.ResolveSecrets(fakeResolver{m: map[string]string{}}); err == nil {
		t.Fatal("a missing secret must be an error, not silently the literal")
	}

	// A {{secret:…}} placeholder with no resolver at all is a clear error.
	if _, err := cfg.ResolveSecrets(nil); err == nil {
		t.Fatal("a secret placeholder without a resolver must error")
	}

	// A missing env var is also a hard error.
	envCfg := Config{Servers: []ServerSpec{{
		Name: "remote", URL: "https://x", Headers: map[string]string{"H": "{{env:NILCORE_DEFINITELY_UNSET_XYZ}}"},
	}}}
	if _, err := envCfg.ResolveSecrets(nil); err == nil {
		t.Fatal("a missing env var must be an error")
	}
}

func TestResolveSecretsIgnoresStdioAndNoPlaceholder(t *testing.T) {
	// stdio servers have no headers to resolve; a static header passes through with a nil
	// resolver because no placeholder is present.
	cfg := Config{Servers: []ServerSpec{
		{Name: "local", Command: []string{"srv"}, Headers: map[string]string{"unused": "{{secret:X}}"}},
		{Name: "remote", URL: "https://x", Headers: map[string]string{"Authorization": "Bearer static"}},
	}}
	resolved, err := cfg.ResolveSecrets(nil)
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if resolved.Servers[1].Headers["Authorization"] != "Bearer static" {
		t.Errorf("static header changed: %q", resolved.Servers[1].Headers["Authorization"])
	}
}

// TestResolveSecretsAppliedToWire proves the RESOLVED value (not the placeholder) is what
// the HTTP transport actually sends — closing the loop from config to wire.
func TestResolveSecretsAppliedToWire(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "tools/call" {
			sawAuth = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		res, _ := dispatch(req.Method)
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	old := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = old }()

	cfg := Config{Servers: []ServerSpec{{
		Name: "remote", URL: srv.URL,
		Headers: map[string]string{"Authorization": "Bearer {{secret:TOK}}"},
	}}}
	cfg, err := cfg.ResolveSecrets(fakeResolver{m: map[string]string{"TOK": "wire-token"}})
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	m := NewManager(cfg)
	defer m.Close()
	if _, err := m.CallTool(context.Background(), "remote", "search", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if sawAuth != "Bearer wire-token" {
		t.Errorf("server saw Authorization %q, want the RESOLVED %q", sawAuth, "Bearer wire-token")
	}
}

// --- Finding 2: detached handshake ctx — one caller's cancel must not fail others ---

// TestGetHandshakeDetachedFromCallerCtx: caller A becomes the single-flight leader and
// blocks in Initialize; caller B (live ctx) waits on the reservation. A cancels its ctx.
// Because the shared handshake runs under a DETACHED ctx, A's cancel must NOT poison the
// reservation — B must still get the connection once the server replies.
func TestGetHandshakeDetachedFromCallerCtx(t *testing.T) {
	release := make(chan struct{})
	var initOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			// Block the handshake until the test releases it (after A has cancelled).
			initOnce.Do(func() { <-release })
		}
		w.Header().Set("Content-Type", "application/json")
		res, _ := dispatch(req.Method)
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	old := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = old }()

	m := NewManager(Config{Servers: []ServerSpec{{Name: "remote", URL: srv.URL}}})
	defer m.Close()

	ctxA, cancelA := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	go func() {
		_, err := m.CallTool(ctxA, "remote", "search", json.RawMessage(`{}`))
		aErr <- err
	}()

	// Let A become the leader and reach the blocked initialize.
	time.Sleep(100 * time.Millisecond)

	// B arrives with a live ctx and waits on the same reservation.
	bErr := make(chan error, 1)
	go func() {
		_, err := m.CallTool(context.Background(), "remote", "search", json.RawMessage(`{}`))
		bErr <- err
	}()
	time.Sleep(50 * time.Millisecond)

	// A abandons its request. Under the buggy code this would set c.err = context.Canceled
	// and B would fail spuriously. Under the fix, the detached handshake survives.
	cancelA()
	close(release) // now the server answers initialize

	select {
	case err := <-bErr:
		if err != nil {
			t.Fatalf("caller B (live ctx) must succeed despite A's cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller B hung — reservation not completed after A cancelled")
	}
	// Drain A (its own ctx was cancelled; its result is irrelevant to the contract).
	select {
	case <-aErr:
	case <-time.After(2 * time.Second):
	}
}

// --- Finding 3: slug collision disambiguation ------------------------------

func TestGenerateWrappersDisambiguatesSlugCollision(t *testing.T) {
	base := t.TempDir()
	// Two DISTINCT tool names that slug() folds to the same base "a_b" (every illegal rune
	// → '_'): "a/b" and "a:b". Without disambiguation the second write clobbers the first.
	tools := []Tool{
		{Name: "a/b", Description: "first", InputSchema: json.RawMessage(`{}`)},
		{Name: "a:b", Description: "second", InputSchema: json.RawMessage(`{}`)},
	}
	if err := GenerateWrappers(base, "srv", tools); err != nil {
		t.Fatalf("GenerateWrappers: %v", err)
	}
	dir := filepath.Join(base, "mcp", "servers", "srv")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("both tools must produce distinct descriptors, got %d: %v", len(ents), names(ents))
	}
	// Both original names must survive on disk (neither tool silently dropped).
	var joined string
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		joined += string(b)
	}
	for _, want := range []string{`"tool": "a/b"`, `"tool": "a:b"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("descriptor set missing %s (a tool was overwritten):\n%s", want, joined)
		}
	}
}

// TestGenerateWrappersIdempotentSameName: re-generating the SAME tool set yields the same
// filenames (disambiguation is stable, not a growing counter).
func TestGenerateWrappersIdempotentSameName(t *testing.T) {
	base := t.TempDir()
	tools := []Tool{
		{Name: "a/b", InputSchema: json.RawMessage(`{}`)},
		{Name: "a:b", InputSchema: json.RawMessage(`{}`)},
	}
	if err := GenerateWrappers(base, "srv", tools); err != nil {
		t.Fatal(err)
	}
	first := listNames(t, filepath.Join(base, "mcp", "servers", "srv"))
	if err := GenerateWrappers(base, "srv", tools); err != nil {
		t.Fatal(err)
	}
	second := listNames(t, filepath.Join(base, "mcp", "servers", "srv"))
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("regeneration changed filenames: %v -> %v", first, second)
	}
	if len(second) != 2 {
		t.Errorf("regeneration must keep exactly 2 descriptors, got %v", second)
	}
}

// --- Finding 4: Discover surfaces resource/prompt wrapper errors -----------

// TestDiscoverSurfacesResourceWrapperError: a failure generating the resource wrappers
// must be RETURNED in Discover's error slice, not swallowed with `_ =`.
func TestDiscoverSurfacesResourceWrapperError(t *testing.T) {
	srv := httptest.NewServer(httpMCPHandler(false))
	defer srv.Close()
	old := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = old }()

	base := t.TempDir()
	// Pre-create a FILE where GenerateResourceWrappers wants to MkdirAll its dir, forcing
	// the write to fail. The server dir must exist first (GenerateWrappers makes it), so
	// place a file at .../srv/resources.
	serverDir := filepath.Join(base, "mcp", "servers", "srv")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDir, "resources"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(Config{Servers: []ServerSpec{{Name: "srv", URL: srv.URL}}})
	defer m.Close()
	errs := m.Discover(context.Background(), base, true /* withResources */)
	var sawResourceErr bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "resources") {
			sawResourceErr = true
		}
	}
	if !sawResourceErr {
		t.Fatalf("Discover must surface the resource-wrapper error, got %v", errs)
	}
}

func names(ents []os.DirEntry) []string {
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return names(ents)
}
