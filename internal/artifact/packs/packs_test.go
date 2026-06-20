package packs

import (
	"testing"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/packs/finance"
	"nilcore/internal/artifact/packs/web"
)

// One representative verifier-id per pack — a non-Default id so its presence proves the
// pack's RegisterAll ran (web.url_resolves is in evverify.Default, so we probe a
// pack-only web id instead).
const (
	probeWeb      = web.IDQuoteExists
	probeSoftware = "software.npm_version_exists"
	probeFinance  = finance.IDSecFact
	probeUI       = "ui.flow_passes"
)

func has(r *evverify.Registry, id string) bool {
	_, ok := r.Lookup(id)
	return ok
}

func TestPacksSelect(t *testing.T) {
	t.Run("selects only the named packs", func(t *testing.T) {
		r := evverify.New()
		if err := Select([]string{"web", "software"}, r); err != nil {
			t.Fatalf("Select: %v", err)
		}
		if !has(r, probeWeb) {
			t.Errorf("web id %q not registered", probeWeb)
		}
		if !has(r, probeSoftware) {
			t.Errorf("software id %q not registered", probeSoftware)
		}
		if has(r, probeFinance) {
			t.Errorf("finance id %q registered but finance was not selected", probeFinance)
		}
		if has(r, probeUI) {
			t.Errorf("ui id %q registered but ui was not selected", probeUI)
		}
	})

	t.Run("unknown name is atomic — registers nothing", func(t *testing.T) {
		r := evverify.New()
		err := Select([]string{"web", "nope"}, r)
		if err == nil {
			t.Fatal("Select with unknown pack: want error, got nil")
		}
		// The valid "web" preceding the bad name must NOT have been registered:
		// validation happens before any RegisterAll runs.
		if has(r, probeWeb) {
			t.Errorf("registry mutated despite error: %q present", probeWeb)
		}
	})

	t.Run("nil and empty are no-ops", func(t *testing.T) {
		for _, names := range [][]string{nil, {}} {
			r := evverify.New()
			if err := Select(names, r); err != nil {
				t.Fatalf("Select(%v): unexpected error %v", names, err)
			}
			// Default-off path: registry untouched, so a pack id stays unresolvable.
			if has(r, probeWeb) || has(r, probeFinance) {
				t.Errorf("Select(%v) registered packs but should be a no-op", names)
			}
		}
	})

	t.Run("names are case-insensitive and space-trimmed", func(t *testing.T) {
		r := evverify.New()
		if err := Select([]string{" Web ", "FINANCE"}, r); err != nil {
			t.Fatalf("Select: %v", err)
		}
		if !has(r, probeWeb) {
			t.Errorf("' Web ' did not register the web pack")
		}
		if !has(r, probeFinance) {
			t.Errorf("'FINANCE' did not register the finance pack")
		}
	})

	t.Run("selecting all four registers each pack", func(t *testing.T) {
		r := evverify.New()
		if err := Select([]string{"web", "software", "finance", "ui"}, r); err != nil {
			t.Fatalf("Select: %v", err)
		}
		for _, id := range []string{probeWeb, probeSoftware, probeFinance, probeUI} {
			if !has(r, id) {
				t.Errorf("id %q not registered after selecting all packs", id)
			}
		}
	})
}

func TestPacksHostsFor(t *testing.T) {
	t.Run("finance host-set", func(t *testing.T) {
		got := HostsFor("finance")
		if len(got) == 0 {
			t.Fatal("HostsFor(finance) is empty")
		}
		for _, want := range []string{"data.sec.gov", "api.stlouisfed.org"} {
			if !contains(got, want) {
				t.Errorf("HostsFor(finance) missing %q (got %v)", want, got)
			}
		}
	})

	t.Run("unknown name returns nil", func(t *testing.T) {
		if got := HostsFor("nope"); got != nil {
			t.Errorf("HostsFor(unknown) = %v, want nil", got)
		}
	})

	t.Run("case-insensitive lookup", func(t *testing.T) {
		if got := HostsFor(" Finance "); len(got) == 0 {
			t.Errorf("HostsFor(' Finance ') = %v, want non-empty", got)
		}
	})

	t.Run("returned slice is a defensive copy", func(t *testing.T) {
		got := HostsFor("finance")
		got[0] = "MUTATED"
		again := HostsFor("finance")
		if contains(again, "MUTATED") {
			t.Error("HostsFor returns a shared slice; mutation leaked into the catalog")
		}
	})

	// Table over all four packs + an unknown: web and ui have a per-claim target host
	// (nil host-set); software and finance have fixed allowlists.
	t.Run("table all packs", func(t *testing.T) {
		cases := []struct {
			name      string
			wantEmpty bool
		}{
			{"web", true},
			{"software", false},
			{"finance", false},
			{"ui", true},
			{"unknown", true},
		}
		for _, tc := range cases {
			got := HostsFor(tc.name)
			if tc.wantEmpty && len(got) != 0 {
				t.Errorf("HostsFor(%q) = %v, want empty", tc.name, got)
			}
			if !tc.wantEmpty && len(got) == 0 {
				t.Errorf("HostsFor(%q) = empty, want non-empty", tc.name)
			}
		}
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
