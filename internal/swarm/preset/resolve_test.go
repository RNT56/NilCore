package preset

import (
	"context"
	"errors"
	"sort"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/packs"
	"nilcore/internal/roster"
)

// Resolve is fail-closed: an unknown name returns ErrUnknownPreset and a nil registry —
// the cmd layer turns this into a startup FATAL, so a typo never downgrades the run to an
// unverified shape. Both the bare garbage name and a whitespace-only name fail.
func TestResolveUnknownIsFailClosed(t *testing.T) {
	for _, bad := range []string{"garbage", "Research-dossier", "  ", ""} {
		p, reg, err := Resolve(bad)
		if !errors.Is(err, ErrUnknownPreset) {
			t.Errorf("Resolve(%q) error = %v, want ErrUnknownPreset", bad, err)
		}
		if reg != nil {
			t.Errorf("Resolve(%q) returned a non-nil registry on failure", bad)
		}
		if p.Name != "" {
			t.Errorf("Resolve(%q) returned a populated Preset on failure: %+v", bad, p)
		}
	}
}

// The returned registry must contain NO always-pass / noop verifier (I2): a claim whose
// verifier-id is not bound to a real check resolves Unverifiable, never Pass. We probe with
// a claim naming an id that no pack registers — it must NOT come back green.
func TestResolvedRegistryHasNoAlwaysPassVerifier(t *testing.T) {
	for _, name := range Names() {
		_, reg, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		// An id nothing registers must be unresolvable (the absence of a check is never a pass).
		if _, ok := reg.Lookup("preset.test.never-registered"); ok {
			t.Errorf("preset %q registry resolved an unregistered verifier-id — an always-pass leak", name)
		}
		// And Resolve over a claim with that bogus verifier-id yields a non-pass status.
		claim := artifact.Claim{
			ID:    "c1",
			Field: "x",
			Evidence: artifact.Evidence{
				Value:    "anything",
				Verifier: "preset.test.never-registered",
			},
		}
		// A nil box is fine: an unregistered id never reaches the box; it fails closed.
		status, _ := reg.Resolve(context.Background(), nil, claim)
		if status == artifact.StatusPass {
			t.Errorf("preset %q: an unregistered claim resolved to Pass — always-pass leak (I2)", name)
		}
		if status != artifact.StatusUnverifiable {
			t.Errorf("preset %q: unregistered claim status = %q, want unverifiable", name, status)
		}
	}
}

// Egress is DERIVED as the sorted union of the selected packs' packs.HostsFor — never
// hand-typed — so it always matches the packs actually wired. We recompute the expected
// union independently and compare.
func TestResolvedEgressIsPackHostsUnion(t *testing.T) {
	for _, name := range Names() {
		p, _, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		want := independentUnion(p.VerifyPacks)
		if !equalStrings(p.Egress, want) {
			t.Errorf("preset %q egress = %v, want pack HostsFor union %v", name, p.Egress, want)
		}
		// The Profile the role got must carry the SAME derived egress (Resolve feeds it in).
		if !equalStrings(p.Profile.Egress.Allowed, want) {
			t.Errorf("preset %q Profile.Egress = %v, want %v", name, p.Profile.Egress.Allowed, want)
		}
	}
}

// Spot-check the two non-trivial unions against the shipped pack host catalogs so a future
// host change in finance/software is caught here, not just by the self-consistent union.
func TestResolvedEgressSpotChecks(t *testing.T) {
	research, _, err := Resolve("research")
	if err != nil {
		t.Fatal(err)
	}
	// research = web (nil hosts) + finance (5 hosts) ⇒ exactly the finance set.
	if !contains(research.Egress, "data.sec.gov") || !contains(research.Egress, "financialmodelingprep.com") {
		t.Errorf("research egress missing a finance host: %v", research.Egress)
	}

	// benchmark = benchmark (nil hosts) ⇒ empty (--network none after intersection).
	bench, _, err := Resolve("benchmark")
	if err != nil {
		t.Fatal(err)
	}
	if len(bench.Egress) != 0 {
		t.Errorf("benchmark egress should be empty (in-box pack), got %v", bench.Egress)
	}

	// code = software (4 hosts) + code (nil) ⇒ the software set, no in-box-pack contribution.
	code, _, err := Resolve("code")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(code.Egress, "registry.npmjs.org") || !contains(code.Egress, "api.github.com") {
		t.Errorf("code egress missing a software host: %v", code.Egress)
	}
}

// SW-T15 + B4-swarm.5, exercised directly: every WRITE preset role takes its write
// capability from the STRUCTURAL Profile.ReadOnly==false field NewWorker reads — never a
// role-name lookup. The Role.ReadOnly() helper used to DIVERGE for the two new roles
// (auditor, ui), reporting them read-only; that latent footgun is now closed — the helper
// AGREES with the Profile for every write role. This test pins both: Profile.ReadOnly==false
// (the source of truth) AND Role.ReadOnly()==false (the helper now agrees), so neither a
// regression of the helper NOR a flip of the structural field can pass silently.
func TestWriteRolesRelyOnProfileNotHelper(t *testing.T) {
	cases := []struct {
		preset string
		role   roster.Role
	}{
		{"audit", roster.RoleAuditor},          // formerly the divergent (gotcha) role — now agrees
		{"ui", roster.RoleUI},                  // formerly the divergent (gotcha) role — now agrees
		{"code", roster.RoleImplementer},       // pre-existing write role
		{"research", roster.RoleTypedResearch}, // pre-existing write role
	}
	for _, tc := range cases {
		t.Run(tc.preset, func(t *testing.T) {
			p, _, err := Resolve(tc.preset)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.preset, err)
			}
			if p.Role != tc.role {
				t.Fatalf("preset %q role = %q, want %q", tc.preset, p.Role, tc.role)
			}
			// The capability source of truth: the Profile says writable.
			if p.Profile.ReadOnly {
				t.Errorf("preset %q: Profile.ReadOnly = true — the write role cannot emit its artifact", tc.preset)
			}
			// The helper now AGREES (no more divergence): a write role reports !ReadOnly.
			if tc.role.ReadOnly() {
				t.Errorf("preset %q: Role.ReadOnly() = true — helper disagrees with the write Profile (footgun regressed)", tc.preset)
			}
		})
	}
}

// The two new roster roles must round-trip through roster.PresetProfile as write-capable,
// proving the seam preset uses (a model-free, policy-free Profile builder) sets ReadOnly
// false for them — the structural reason NewWorker hands them the write registry.
func TestPresetProfileSeamIsWriteCapable(t *testing.T) {
	for _, role := range []roster.Role{roster.RoleAuditor, roster.RoleUI, roster.RoleImplementer, roster.RoleTypedResearch} {
		prof, ok := roster.PresetProfile(role, nil)
		if !ok {
			t.Fatalf("PresetProfile(%q) reported not-ok", role)
		}
		if prof.ReadOnly {
			t.Errorf("PresetProfile(%q).ReadOnly = true — write role mis-wired", role)
		}
		if prof.System == "" {
			t.Errorf("PresetProfile(%q) has an empty System prompt", role)
		}
	}
	// An unknown role fails closed at the roster seam.
	if _, ok := roster.PresetProfile(roster.RoleReviewer, nil); ok {
		t.Error("PresetProfile(reviewer) should fail closed — reviewer is not a write preset role")
	}
}

// independentUnion recomputes the expected egress the same way Resolve should: the sorted,
// de-duplicated union of each pack's packs.HostsFor. Kept independent of resolve.go's helper
// so the test is a genuine cross-check, not a tautology.
func independentUnion(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		for _, h := range packs.HostsFor(n) {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

// contains reports whether xs holds s.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TestFixPresetSelectsFailureSharder is the B4-swarm.2 wire guard: the "fix" preset is
// the catalog entry that makes SharderFailure reachable (before this it was a defined-but-
// unselectable enum value). It must resolve to a runnable, write-capable bundle whose
// Sharder is SharderFailure and whose FanIn merges (fix shards make real edits that land
// as one verified tree), and it must be the ONLY preset selecting that sharder.
func TestFixPresetSelectsFailureSharder(t *testing.T) {
	p, reg, err := Resolve("fix")
	if err != nil {
		t.Fatalf("Resolve(fix): %v", err)
	}
	if reg == nil {
		t.Fatal("Resolve(fix) returned a nil registry")
	}
	if p.Sharder != SharderFailure {
		t.Errorf("fix preset Sharder = %q, want %q (the failure-driven flow)", p.Sharder, SharderFailure)
	}
	if p.FanIn != FanInMerge {
		t.Errorf("fix preset FanIn = %q, want merge (its fix branches must integrate)", p.FanIn)
	}
	if p.Role != roster.RoleImplementer {
		t.Errorf("fix preset Role = %q, want implementer (it edits code)", p.Role)
	}
	if p.Profile.ReadOnly {
		t.Error("fix preset Profile.ReadOnly = true — a fix worker cannot edit the tree")
	}
	// Exactly one preset selects SharderFailure; the enum is no longer dead.
	var failurePresets []string
	for _, name := range Names() {
		q, _, err := Resolve(name)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if q.Sharder == SharderFailure {
			failurePresets = append(failurePresets, name)
		}
	}
	if len(failurePresets) != 1 || failurePresets[0] != "fix" {
		t.Errorf("SharderFailure selected by %v, want exactly [fix]", failurePresets)
	}
}
