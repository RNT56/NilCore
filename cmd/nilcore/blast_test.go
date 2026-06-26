package main

import (
	"testing"
	"time"
)

func TestMintBlastBudget_OffIsNil(t *testing.T) {
	// "off" / empty / unknown ⇒ no budget (unfenced, byte-identical default-off).
	for _, p := range []string{"off", "", "bogus"} {
		if b := mintBlastBudget(p, nil); b != nil {
			t.Errorf("mintBlastBudget(%q) = non-nil, want nil (no fence)", p)
		}
	}
}

func TestMintBlastBudget_PresetCeilings(t *testing.T) {
	b := mintBlastBudget("standard", nil)
	if b == nil {
		t.Fatal("standard should mint a budget")
	}
	u := b.Used("2026-06-26")
	if u.HostCeiling != 8 || u.IrrevCeiling != 5 || u.WallCeiling != 20*time.Minute || u.DayCeiling != 5 {
		t.Errorf("standard ceilings = %+v, want hosts=8 irrev=5 wall=20m day=$5", u)
	}
	// No preset leaves any axis unbounded (every ceiling must be positive).
	for _, name := range []string{"tight", "standard"} {
		uu := mintBlastBudget(name, nil).Used("d")
		if uu.HostCeiling <= 0 || uu.IrrevCeiling <= 0 || uu.WallCeiling <= 0 || uu.DayCeiling <= 0 {
			t.Errorf("preset %q has an unbounded axis: %+v", name, uu)
		}
	}
}
