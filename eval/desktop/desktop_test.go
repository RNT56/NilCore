package desktop

import (
	"testing"

	"nilcore/eval/browse"
)

func TestFaultsIncludeDesktopAndTransport(t *testing.T) {
	fs := Faults()
	has := func(k browse.FaultKind) bool {
		for _, f := range fs {
			if f == k {
				return true
			}
		}
		return false
	}
	for _, want := range []browse.FaultKind{FaultDPIChange, FaultA11yEmpty, browse.FaultHTTP5xx} {
		if !has(want) {
			t.Errorf("Faults() missing %q", want)
		}
	}
}

func TestDefaultScenariosValid(t *testing.T) {
	for _, s := range DefaultScenarios() {
		if s.Name == "" || s.Goal == "" {
			t.Fatalf("scenario missing fields: %+v", s)
		}
		if s.ExpectField == "" && s.ExpectText == "" {
			t.Fatalf("scenario %q has no success criterion", s.Name)
		}
	}
}

// TestReuseHarness proves the desktop catalog grades + scores through the UNCHANGED
// eval/browse harness (the reuse claim).
func TestReuseHarness(t *testing.T) {
	s := DefaultScenarios()[0] // calculator-add, ExpectField=sum, ExpectValue=42
	var r browse.Reliability
	// Three runs: two succeed (recorded sum=42), one fails under a desktop fault.
	r.Record(browse.Grade(s, map[string]string{"sum": "42"}, ""))
	r.Record(browse.Grade(s, map[string]string{"sum": "42"}, ""))
	r.Record(browse.Grade(s, map[string]string{"sum": "41"}, "")) // mis-computed
	if r.PassAt1() < 0.66 || r.PassAt1() > 0.67 {
		t.Fatalf("pass@1 = %v, want ~0.667", r.PassAt1())
	}
	if r.PassPowK() {
		t.Fatal("pass^k must be false when any run fails")
	}
	// A clean sweep under a fault plan.
	plan := browse.Plan(FaultA11yEmpty, 2, 6)
	if len(plan.Steps()) == 0 {
		t.Fatal("desktop fault plan should schedule injections")
	}
}
