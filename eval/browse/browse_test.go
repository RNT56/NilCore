package browse

import (
	"reflect"
	"testing"
)

func TestPlanDeterministic(t *testing.T) {
	p := Plan(FaultHTTP5xx, 3, 10) // every 3rd step, 0-based: 2,5,8
	if got := p.Steps(); !reflect.DeepEqual(got, []int{2, 5, 8}) {
		t.Fatalf("steps = %v, want [2 5 8]", got)
	}
	if p.FaultAt(2) != FaultHTTP5xx || p.FaultAt(3) != FaultNone {
		t.Fatalf("FaultAt wrong: 2=%q 3=%q", p.FaultAt(2), p.FaultAt(3))
	}
	// Reproducible: a second Plan with the same args is identical.
	if !reflect.DeepEqual(p, Plan(FaultHTTP5xx, 3, 10)) {
		t.Fatal("Plan is not deterministic")
	}
	// Clean baseline.
	if len(Plan(FaultNone, 3, 10).Faults) != 0 || len(Plan(FaultHTTP5xx, 0, 10).Faults) != 0 {
		t.Fatal("a none/everyN<=0 plan must be empty")
	}
}

func TestPlanMerge(t *testing.T) {
	a := Plan(FaultNetworkDelay, 2, 6) // 1,3,5
	b := Plan(FaultPopup, 3, 6)        // 2,5
	m := a.Merge(b)
	if m.FaultAt(1) != FaultNetworkDelay || m.FaultAt(2) != FaultPopup || m.FaultAt(5) != FaultPopup {
		t.Fatalf("merge wrong: 1=%q 2=%q 5=%q (b wins on the shared step)", m.FaultAt(1), m.FaultAt(2), m.FaultAt(5))
	}
}

func TestGradeExtraction(t *testing.T) {
	s := Scenario{Name: "x", ExpectField: "ver", ExpectValue: "v1.4.2"}
	if o := Grade(s, map[string]string{"ver": "v1.4.2"}, ""); !o.Achieved {
		t.Fatalf("matching verified finding should pass: %+v", o)
	}
	if o := Grade(s, map[string]string{"ver": "v9.9.9"}, ""); o.Achieved {
		t.Fatal("wrong value must fail")
	}
	if o := Grade(s, map[string]string{}, ""); o.Achieved {
		t.Fatal("missing finding must fail")
	}
}

func TestGradeText(t *testing.T) {
	s := Scenario{Name: "t", ExpectText: "Getting Started"}
	if o := Grade(s, nil, "Welcome — Getting started guide"); !o.Achieved {
		t.Fatalf("case-insensitive text match should pass: %+v", o)
	}
	if o := Grade(s, nil, "nothing relevant"); o.Achieved {
		t.Fatal("absent text must fail")
	}
}

func TestReliabilityMetrics(t *testing.T) {
	// 3 of 4 runs succeed: pass@1 = 0.75, pass^k = false (not every run passed).
	var r Reliability
	for _, ok := range []bool{true, true, false, true} {
		r.Record(Outcome{Achieved: ok})
	}
	if r.PassAt1() != 0.75 {
		t.Fatalf("pass@1 = %v, want 0.75", r.PassAt1())
	}
	if r.PassPowK() {
		t.Fatal("pass^k must be false when any run fails")
	}

	// All pass ⇒ pass^k true.
	var all Reliability
	for i := 0; i < 5; i++ {
		all.Record(Outcome{Achieved: true})
	}
	if !all.PassPowK() || all.PassAt1() != 1.0 {
		t.Fatalf("all-pass: pass^k=%v pass@1=%v", all.PassPowK(), all.PassAt1())
	}

	// Zero runs ⇒ not reliable, rate 0 (no division by zero).
	var none Reliability
	if none.PassPowK() || none.PassAt1() != 0 {
		t.Fatal("zero runs must be 0 / not-reliable")
	}
}

func TestDefaultScenariosValid(t *testing.T) {
	for _, s := range DefaultScenarios() {
		if s.Name == "" || s.Goal == "" || s.StartURL == "" {
			t.Fatalf("scenario missing required fields: %+v", s)
		}
		if s.ExpectField == "" && s.ExpectText == "" {
			t.Fatalf("scenario %q has no success criterion", s.Name)
		}
	}
}
