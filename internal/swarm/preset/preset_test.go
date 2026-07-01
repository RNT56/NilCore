package preset

import (
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/roster"
)

// wantPreset is the per-name expectation for the shipped bundles (SWARM.md §8.1).
// It pins every static fact a Resolve must reproduce so a future edit to the catalog that
// silently changes a Kind/Role/shape is caught.
type wantPreset struct {
	kind    artifact.Kind
	role    roster.Role
	packs   []string
	fanIn   FanIn
	shape   Shape
	sharder SharderKind
}

var wantPresets = map[string]wantPreset{
	"research": {
		kind:    artifact.KindDossier,
		role:    roster.RoleTypedResearch,
		packs:   []string{"web", "finance"},
		fanIn:   FanInCollate,
		shape:   ShapeFlat,
		sharder: SharderPlan,
	},
	"code": {
		kind:    artifact.KindSpec,
		role:    roster.RoleImplementer,
		packs:   []string{"software", "code"},
		fanIn:   FanInMerge,
		shape:   ShapeDAG,
		sharder: SharderPlan,
	},
	"fix": {
		kind:    artifact.KindSpec,
		role:    roster.RoleImplementer,
		packs:   []string{"software", "code"},
		fanIn:   FanInMerge,
		shape:   ShapeFlat,
		sharder: SharderFailure,
	},
	"audit": {
		kind:    artifact.KindReport,
		role:    roster.RoleAuditor,
		packs:   []string{"audit", "web"},
		fanIn:   FanInCollate,
		shape:   ShapeFlat,
		sharder: SharderPlan,
	},
	"benchmark": {
		kind:    artifact.KindBenchmark,
		role:    roster.RoleImplementer,
		packs:   []string{"benchmark"},
		fanIn:   FanInCollate,
		shape:   ShapeFlat,
		sharder: SharderList,
	},
	"ui": {
		kind:    artifact.KindReport,
		role:    roster.RoleUI,
		packs:   []string{"ui"},
		fanIn:   FanInCollate,
		shape:   ShapeFlat,
		sharder: SharderPlan,
	},
}

// Every named preset resolves with exactly the §8.1 Kind/Role/FanIn/Shape/Sharder/packs.
func TestResolveEachPreset(t *testing.T) {
	for name, want := range wantPresets {
		t.Run(name, func(t *testing.T) {
			p, reg, err := Resolve(name)
			if err != nil {
				t.Fatalf("Resolve(%q) returned error: %v", name, err)
			}
			if reg == nil {
				t.Fatalf("Resolve(%q) returned a nil registry", name)
			}
			if p.Name != name {
				t.Errorf("Name = %q, want %q", p.Name, name)
			}
			if p.Kind != want.kind {
				t.Errorf("Kind = %q, want %q", p.Kind, want.kind)
			}
			if p.Role != want.role {
				t.Errorf("Role = %q, want %q", p.Role, want.role)
			}
			if p.FanIn != want.fanIn {
				t.Errorf("FanIn = %q, want %q", p.FanIn, want.fanIn)
			}
			if p.Shape != want.shape {
				t.Errorf("Shape = %q, want %q", p.Shape, want.shape)
			}
			if p.Sharder != want.sharder {
				t.Errorf("Sharder = %q, want %q", p.Sharder, want.sharder)
			}
			if !equalStrings(p.VerifyPacks, want.packs) {
				t.Errorf("VerifyPacks = %v, want %v", p.VerifyPacks, want.packs)
			}
			// The resolved Profile must be non-empty (a System prompt + a tool registry),
			// proving Resolve actually built it rather than leaving the catalog's zero.
			if p.Profile.System == "" {
				t.Error("resolved Profile has an empty System prompt")
			}
			if p.Profile.Tools == nil {
				t.Error("resolved Profile has a nil tool registry")
			}
		})
	}
}

// The catalog has exactly the documented presets, no more, no fewer.
func TestNamesAreTheDocumentedPresets(t *testing.T) {
	got := Names()
	want := []string{"audit", "benchmark", "code", "fix", "research", "ui"} // Names() is sorted
	if !equalStrings(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

// Lookup is the inspection path: it returns the bare catalog entry (Profile/Egress still
// zero) and reports unknown names as not-ok. Name normalization makes " Research " resolve.
func TestLookup(t *testing.T) {
	if _, ok := Lookup("research"); !ok {
		t.Error("Lookup(research) reported not-ok")
	}
	if _, ok := Lookup("  RESEARCH  "); !ok {
		t.Error("Lookup is not normalizing case/whitespace")
	}
	if _, ok := Lookup("garbage"); ok {
		t.Error("Lookup(garbage) reported ok for an unknown preset")
	}
	// The bare Lookup leaves the derived fields zero (Resolve fills them).
	p, _ := Lookup("research")
	if len(p.Egress) != 0 {
		t.Errorf("bare Lookup carried a derived Egress %v (should be filled only by Resolve)", p.Egress)
	}
	if p.Profile.System != "" {
		t.Error("bare Lookup carried a built Profile (should be filled only by Resolve)")
	}
}

// equalStrings reports element-wise slice equality (order-sensitive).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
