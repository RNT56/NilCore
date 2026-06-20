// Package egressprofile owns the named, opt-in research egress presets (Pillar 5
// of the Phase-11 artifact-factory roadmap). A preset is a labeled
// policy.Egress: it WIDENS the sandbox's deny-all tree from nothing to a
// sanctioned, auditable host set. It never narrows a role — roster.EgressFor
// still intersects each role's own allowlist against the resolved tree
// (narrow-only, R9), so a deny-all role stays `--network none` under any
// profile. This package is a stdlib-first leaf: it imports only internal/policy
// (for the policy.Egress type the presets construct) and the standard library.
//
// WHY literal hosts: roster.intersectEgress is intentionally conservative — a
// wildcard preset entry only survives intersection if the role side carries the
// IDENTICAL wildcard, whereas a literal host survives whenever the role allows
// that exact host. Keeping the presets literal therefore lets the role-side
// allowlists keep working without each role having to mirror our wildcards. The
// few wildcards we do ship are documented as requiring matching role-side
// wildcards.
//
// I3: an allowlist holds hostnames ONLY — never a secret, query string, or key.
// Keyed sources (e.g. a FRED/market-data API key) keep their key in the
// SecretStore and inject it via box.ExecWithEnv at the wiring layer; the host
// listed here is always the canonical key-free public host.
package egressprofile

import (
	"sort"

	"nilcore/internal/policy"
)

// Preset names. This is the CLOSED set surfaced by Names(); the front-door
// wiring (P11-T28) and onboard validation (P11-T27) reject anything else.
const (
	ProfileFinance     = "finance"
	ProfileDocs        = "docs"
	ProfileWebResearch = "web-research"
)

// presets maps each preset name to its host set. Hosts are co-designed with the
// Pillar-2 verifier packs: the finance preset is the superset of every host the
// finance pack reaches (P11-T35 cross-checks this), and likewise docs/web-research
// cover the software/web packs. Entries are literal hosts unless a comment marks a
// host as a documented wildcard (requiring a matching role-side wildcard).
var presets = map[string][]string{
	// finance — sanctioned market/economic data sources for the finance pack
	// (finance.sec_fact, finance.fred_series, finance.worldbank_indicator,
	// finance.imf_series, finance.market_quote). All literal, all key-free hosts;
	// keyed sources inject $NAME via ExecWithEnv, never a query string here.
	ProfileFinance: {
		"data.sec.gov",              // SEC companyfacts (finance.sec_fact)
		"www.sec.gov",               // SEC filings/index
		"api.stlouisfed.org",        // FRED series (finance.fred_series, keyed)
		"api.worldbank.org",         // World Bank indicators (finance.worldbank_indicator)
		"www.imf.org",               // IMF data portal (finance.imf_series)
		"financialmodelingprep.com", // market quotes (finance.market_quote, keyed)
	},
	// docs — package registries + source hosts for the software-research pack
	// (npm/pypi/crates/github version + release checks).
	ProfileDocs: {
		"registry.npmjs.org",        // npm_version_exists
		"pypi.org",                  // pypi_version_exists
		"files.pythonhosted.org",    // pypi artifacts
		"crates.io",                 // crate_version_exists
		"static.crates.io",          // crate artifacts
		"api.github.com",            // github_release_exists / github_tag_exists
		"github.com",                // github source / license
		"raw.githubusercontent.com", // license_matches (raw file fetch)
	},
	// web-research — general web verification for the web pack (url_resolves,
	// quote_exists, date_matches, not_stale). Two documented wildcards: they
	// require a matching role-side wildcard to survive intersection.
	ProfileWebResearch: {
		"en.wikipedia.org", // canonical reference source
		"www.wikidata.org", // structured facts
		"api.crossref.org", // DOI / publication metadata
		"archive.org",      // snapshots / freshness
		"web.archive.org",  // wayback snapshots
		"*.wikipedia.org",  // wildcard: requires matching role-side wildcard
		"*.gov",            // wildcard: requires matching role-side wildcard
	},
}

// Named returns the policy.Egress for a preset and ok=false for an unknown name.
// The returned Egress is a fresh copy — callers may not mutate the package's
// backing slice.
func Named(name string) (policy.Egress, bool) {
	hosts, ok := presets[name]
	if !ok {
		return policy.Egress{}, false
	}
	return policy.Egress{Allowed: append([]string(nil), hosts...)}, true
}

// Names returns the closed set of preset names, sorted for determinism.
func Names() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Resolve unions a named preset (when profileName != "") with a project-local
// allowlist file (when filePath != "") into one tree allowlist, deduping hosts
// while preserving first-seen order (preset hosts first, then file hosts). It
// returns the merged tree, the provenance of each contributing source
// ("profile:<name>" and/or "file:<path>") for the metadata-only egress_profile
// event, and an error if either side fails to resolve.
//
// Resolve("","") is the byte-identical default: an empty policy.Egress (deny-all)
// and no sources. An unknown profileName is a fail-closed error — never a silent
// widen. A file that fails to load is also an error (the caller fails closed to
// deny-all, never fail-open).
func Resolve(profileName, filePath string) (tree policy.Egress, sources []string, err error) {
	var hosts []string
	seen := map[string]bool{}
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		hosts = append(hosts, h)
	}

	if profileName != "" {
		preset, ok := Named(profileName)
		if !ok {
			return policy.Egress{}, nil, &UnknownProfileError{Name: profileName}
		}
		for _, h := range preset.Allowed {
			add(h)
		}
		sources = append(sources, "profile:"+profileName)
	}

	if filePath != "" {
		fileEgress, ferr := LoadFile(filePath)
		if ferr != nil {
			return policy.Egress{}, nil, ferr
		}
		for _, h := range fileEgress.Allowed {
			add(h)
		}
		sources = append(sources, "file:"+filePath)
	}

	return policy.Egress{Allowed: hosts}, sources, nil
}
