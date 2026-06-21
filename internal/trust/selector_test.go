package trust

import (
	"context"
	"reflect"
	"testing"

	"nilcore/internal/backend"
)

// TestSelectOrdersBestFirst: a populated ledger orders the candidate names
// best-first by smoothed score (the strongest earned backend leads).
func TestSelectOrdersBestFirst(t *testing.T) {
	l := New()
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "codex", Passed: true}) // 10/10, strongest
	}
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "native", Passed: i < 5}) // 5/10, middle
	}
	for i := 0; i < 10; i++ {
		l.Record(Outcome{Backend: "weak", Passed: i < 1}) // 1/10, weakest
	}

	s := NewSelector(l)
	got := s.Select(context.Background(), backend.Task{}, []string{"native", "weak", "codex"})
	want := []string{"codex", "native", "weak"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Select = %v, want %v (best-first)", got, want)
	}
}

// TestSelectEmptyLedgerKeepsOrder: an empty ledger carries no earned signal, so the
// configured order is returned unchanged (byte-identical no-history path).
func TestSelectEmptyLedgerKeepsOrder(t *testing.T) {
	s := NewSelector(New())
	in := []string{"native", "codex", "claude-code"}
	got := s.Select(context.Background(), backend.Task{}, in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("empty-ledger Select = %v, want unchanged %v", got, in)
	}
}

// TestSelectNilLedgerKeepsOrder: a nil ledger (and a nil receiver) degrade to
// returning the names unchanged.
func TestSelectNilLedgerKeepsOrder(t *testing.T) {
	in := []string{"a", "b", "c"}

	s := NewSelector(nil)
	if got := s.Select(context.Background(), backend.Task{}, in); !reflect.DeepEqual(got, in) {
		t.Errorf("nil-ledger Select = %v, want unchanged %v", got, in)
	}

	var ns *Selector
	if got := ns.Select(context.Background(), backend.Task{}, in); !reflect.DeepEqual(got, in) {
		t.Errorf("nil-receiver Select = %v, want unchanged %v", got, in)
	}
}

// TestSelectorSatisfiesAgentSelectorShape is the structural-match tripwire restated
// in the test: *Selector must implement the same Select signature agent.Selector
// declares, proven WITHOUT importing agent.
func TestSelectorSatisfiesAgentSelectorShape(t *testing.T) {
	type agentSelectorShape interface {
		Select(ctx context.Context, t backend.Task, names []string) []string
	}
	var _ agentSelectorShape = NewSelector(New())
}
