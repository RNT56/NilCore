package trust

import (
	"context"
	"testing"

	"nilcore/internal/backend"
)

// fakeBackend is a minimal CodingBackend whose Name() is its routing identity. It
// never runs work — Route only SELECTS a backend, so Run is never called in these
// tests (the verifier/orchestrator owns execution; the router only orders).
type fakeBackend struct{ name string }

func (f fakeBackend) Name() string { return f.name }
func (f fakeBackend) Run(context.Context, backend.Task) (backend.Result, error) {
	return backend.Result{Backend: f.name}, nil
}

func wired(names ...string) map[string]backend.CodingBackend {
	m := map[string]backend.CodingBackend{}
	for _, n := range names {
		m[n] = fakeBackend{name: n}
	}
	return m
}

// TestRouteReturnsBestExisting: the highest-ranked WIRED backend wins the first
// attempt.
func TestRouteReturnsBestExisting(t *testing.T) {
	l := New()
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "codex", Passed: true}) // 10/10, the strongest
	}
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "native", Passed: i < 3}) // 3/10
	}

	def := fakeBackend{name: "default"}
	r := NewRouter(l, wired("native", "codex"), def)

	got := r.Route(context.Background(), backend.Task{}, def)
	if got.Name() != "codex" {
		t.Errorf("Route picked %q, want \"codex\" (strongest wired)", got.Name())
	}
}

// TestRouteSkipsUnwiredTopRank: the strongest backend exists in the ledger but is
// NOT wired here, so Route falls through to the next strongest that IS wired.
func TestRouteSkipsUnwiredTopRank(t *testing.T) {
	l := New()
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "ghost", Passed: true}) // strongest, but not wired below
	}
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "native", Passed: i < 7}) // wired, weaker
	}

	def := fakeBackend{name: "default"}
	r := NewRouter(l, wired("native"), def) // ghost intentionally absent

	got := r.Route(context.Background(), backend.Task{}, def)
	if got.Name() != "native" {
		t.Errorf("Route picked %q, want \"native\" (top rank unwired, fall to next)", got.Name())
	}
}

// TestRouteFallbackWhenNoneWired: every ranked backend is unknown to this router
// ⇒ the fallback is returned, byte-identical.
func TestRouteFallbackWhenNoneWired(t *testing.T) {
	l := New()
	l.Record(Outcome{Backend: "codex", Passed: true})

	def := fakeBackend{name: "default"}
	r := NewRouter(l, wired("somethingElse"), def)

	got := r.Route(context.Background(), backend.Task{}, def)
	if got != backend.CodingBackend(def) {
		t.Errorf("Route = %v, want the byte-identical fallback %v", got, def)
	}
}

// TestRouteFallbackWhenLedgerEmpty: no earned signal ⇒ the fallback is returned,
// and it is the SAME value passed in (byte-identical default path).
func TestRouteFallbackWhenLedgerEmpty(t *testing.T) {
	def := fakeBackend{name: "default"}
	r := NewRouter(New(), wired("native", "codex"), def)

	got := r.Route(context.Background(), backend.Task{}, def)
	if got != backend.CodingBackend(def) {
		t.Errorf("empty-ledger Route = %v, want byte-identical fallback %v", got, def)
	}
}

// TestRouteNilLedgerIsFallback: a nil ledger degrades to always-fallback.
func TestRouteNilLedgerIsFallback(t *testing.T) {
	def := fakeBackend{name: "default"}
	r := NewRouter(nil, wired("native"), def)

	got := r.Route(context.Background(), backend.Task{}, def)
	if got != backend.CodingBackend(def) {
		t.Errorf("nil-ledger Route = %v, want fallback %v", got, def)
	}
}

// TestRouterSatisfiesAgentRouterShape is the structural-match tripwire restated in
// the test: *Router must implement the same Route signature agent.Router declares,
// proven WITHOUT importing agent.
func TestRouterSatisfiesAgentRouterShape(t *testing.T) {
	type agentRouterShape interface {
		Route(ctx context.Context, t backend.Task, def backend.CodingBackend) backend.CodingBackend
	}
	var _ agentRouterShape = NewRouter(New(), nil, fakeBackend{})
}
