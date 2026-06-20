package super

import (
	"reflect"
	"testing"

	"nilcore/internal/spawn"
)

// TestSubagentReportContinueFrom proves the additive (P11-T17a) enrichment of the
// subagent_report event Detail: byte-identical {passed,branch,has_err} on a normal
// (non-retry) report, and the extra continue_from/base keys ONLY when the spec is an
// actual retry (spec.ContinueFrom != ""). This keeps default logs untouched while
// handing Pillar 6's report projection a secondary retry-history signal.
func TestSubagentReportContinueFrom(t *testing.T) {
	t.Run("non-retry is byte-identical to the frozen shape", func(t *testing.T) {
		st := &runState{handles: map[string]*Handle{}}
		spec := SubagentSpec{ID: "node-a"}
		res := spawn.Result{Passed: true, Branch: "nilcore/node-a"}

		got := subagentReportDetail(st, spec, res)
		want := map[string]any{
			"passed":  true,
			"branch":  "nilcore/node-a",
			"has_err": false,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("non-retry Detail = %#v, want frozen shape %#v", got, want)
		}
		// No retry keys leak onto a non-retry report.
		if _, ok := got["continue_from"]; ok {
			t.Errorf("non-retry Detail must not carry continue_from")
		}
		if _, ok := got["base"]; ok {
			t.Errorf("non-retry Detail must not carry base")
		}
	})

	t.Run("retry additively carries continue_from and base", func(t *testing.T) {
		// The prior attempt sits in st.handles with its preserved branch — base mirrors
		// continueBase's value (the cut point), so the report agrees with the
		// subagent_continue event without recomputing BaseRef.
		st := &runState{handles: map[string]*Handle{
			"node-a": {Result: spawn.Result{Branch: "nilcore/node-a-wip"}, Done: true},
		}}
		spec := SubagentSpec{ID: "node-a-retry", ContinueFrom: "node-a"}
		res := spawn.Result{Passed: false, Branch: "nilcore/node-a-retry", Err: nil}

		got := subagentReportDetail(st, spec, res)
		want := map[string]any{
			"passed":        false,
			"branch":        "nilcore/node-a-retry",
			"has_err":       false,
			"continue_from": "node-a",
			"base":          "nilcore/node-a-wip",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("retry Detail = %#v, want %#v", got, want)
		}
	})

	t.Run("retry whose prior attempt changed nothing reports an empty base", func(t *testing.T) {
		// A prior attempt that produced no branch (changed nothing) ⇒ base is "" — the
		// same clean degradation continueBase makes; the keys are still PRESENT (this is
		// a real retry), they are simply empty.
		st := &runState{handles: map[string]*Handle{
			"node-a": {Result: spawn.Result{Branch: ""}, Done: true},
		}}
		spec := SubagentSpec{ID: "node-a-retry", ContinueFrom: "node-a"}
		res := spawn.Result{Passed: true, Branch: "nilcore/node-a-retry"}

		got := subagentReportDetail(st, spec, res)
		if cf, ok := got["continue_from"]; !ok || cf != "node-a" {
			t.Errorf("continue_from = %v, ok=%v; want node-a present", cf, ok)
		}
		base, ok := got["base"]
		if !ok {
			t.Fatalf("base key must be present on a retry even when empty")
		}
		if base != "" {
			t.Errorf("base = %q, want empty (prior attempt changed nothing)", base)
		}
	})

	t.Run("retry with an absent prior handle still emits keys (defensive)", func(t *testing.T) {
		// checkSpawnRails validates existence before dispatch; this is the defensive path
		// (matches continueBase): a missing handle yields an empty base, never a panic.
		st := &runState{handles: map[string]*Handle{}}
		spec := SubagentSpec{ID: "node-a-retry", ContinueFrom: "ghost"}
		res := spawn.Result{Passed: false, Branch: ""}

		got := subagentReportDetail(st, spec, res)
		if got["continue_from"] != "ghost" {
			t.Errorf("continue_from = %v, want ghost", got["continue_from"])
		}
		if got["base"] != "" {
			t.Errorf("base = %v, want empty for an absent prior handle", got["base"])
		}
	})
}
