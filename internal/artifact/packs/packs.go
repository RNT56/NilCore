// Package packs is the domain-verifier-pack aggregator (Phase 11, Pillar 2). The four
// leaf packs — web, software, finance, ui — each expose a RegisterAll that adds their
// namespaced verifier-ids to an evverify.Registry. This file is the single seam the
// wiring (P11-T12) uses to turn a model/operator-named list of packs into registrations,
// and the single place P11-T35 reads each pack's documented egress host-set from.
//
// Two narrow, auditable operations:
//
//   - Select(names, r) — register EXACTLY the named packs into r. Unknown names abort
//     the whole call BEFORE any RegisterAll runs, so a typo can never leave the registry
//     half-populated (atomic). nil/empty names is a no-op, so the default (packs off)
//     path leaves r byte-identical to Default() — any pack-claim then resolves
//     Unverifiable, never Pass (additive/opt-in).
//   - HostsFor(name) — the documented egress host-set a pack reaches, co-designed with
//     the egress profiles (P11-T25) and cross-checked by P11-T35 (every pack host must
//     be a subset of its profile). An unknown name returns nil.
//
// This file is a LEAF aggregator: it imports only the four sibling packs plus the
// standard library — never the orchestrator (super/roster/agent). Each pack already
// keeps its own LEAF discipline (artifact/evverify/sandbox/worktreefs only), so the
// aggregator transitively pulls nothing heavier.
package packs

import (
	"fmt"
	"sort"
	"strings"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/packs/audit"
	"nilcore/internal/artifact/packs/benchmark"
	"nilcore/internal/artifact/packs/code"
	"nilcore/internal/artifact/packs/finance"
	"nilcore/internal/artifact/packs/software"
	"nilcore/internal/artifact/packs/ui"
	"nilcore/internal/artifact/packs/web"
)

// Canonical pack names. Selection and HostsFor lookups are normalized to these
// (lower-cased, space-trimmed) so " Web, Finance " resolves the same as "web,finance".
const (
	NameWeb      = "web"
	NameSoftware = "software"
	NameFinance  = "finance"
	NameUI       = "ui"
	// NameAudit / NameBenchmark / NameCode are the Phase-12 (swarm) packs. All three
	// run ENTIRELY in-box against the local worktree (audit reproduces a file:line with
	// sed/grep, benchmark re-runs an allowlisted script, code re-runs the autodetected
	// build/test) — none reaches a fixed external host, so each carries a nil host-set
	// and HostsFor returns nil for them (no egress allowlist to cross-check, P11-T35).
	NameAudit     = "audit"
	NameBenchmark = "benchmark"
	NameCode      = "code"
)

// pack bundles a name's registration entrypoint with its documented egress host-set.
// registerAll mirrors each leaf's RegisterAll; hosts is the host catalog HostsFor
// returns (a fresh copy is handed out so a caller can never mutate the source).
type pack struct {
	registerAll func(*evverify.Registry)
	hosts       []string
}

// registry maps each canonical pack name to its registration + host-set. The host
// catalogs are sourced from the packs themselves where they export one (finance.Hosts,
// ui.Hosts()) so a single source of truth stays the cross-check input for P11-T35; for
// web and software — which the aggregator may not edit — the documented catalog lives
// here alongside the import:
//
//   - web reaches whatever URL the per-claim Evidence.SourceURL names (an arbitrary,
//     model-authored host validated and reached in-box), so it has NO fixed allowlist —
//     its host-set is nil and a UI/web profile must already permit the target.
//   - software reaches a fixed set of package-registry + VCS-metadata endpoints
//     (npm/PyPI/crates.io/GitHub API); those are listed here verbatim from the pack's
//     check endpoints so the P11-T35 cross-check has a definite answer.
var registry = map[string]pack{
	NameWeb: {
		registerAll: web.RegisterAll,
		hosts:       nil, // arbitrary per-claim SourceURL host — no fixed allowlist
	},
	NameSoftware: {
		registerAll: software.RegisterAll,
		hosts: []string{
			"registry.npmjs.org", // npm_version_exists
			"pypi.org",           // pypi_version_exists
			"crates.io",          // crate_version_exists
			"api.github.com",     // github_release_exists / github_tag_exists / license_matches
		},
	},
	NameFinance: {
		registerAll: finance.RegisterAll,
		hosts:       finance.Hosts, // co-designed with the finance egress profile
	},
	NameUI: {
		registerAll: ui.RegisterAll,
		hosts:       ui.Hosts(), // intentionally empty: the flow targets a per-claim site
	},
	// The three swarm packs are in-box/local by construction: their checks are pure
	// functions of files already on disk (audit), a re-run of an allowlisted script in
	// the worktree (benchmark), or a re-run of the autodetected build/test (code). None
	// reaches a fixed external host, so each documents a nil host-set. We still source
	// each from the pack's own Hosts() so the single-source-of-truth discipline (and the
	// P11-T35 cross-check input) is identical to the shipped packs.
	NameAudit: {
		registerAll: audit.RegisterAll,
		hosts:       audit.Hosts(), // nil: file:line reproduction is local-only
	},
	NameBenchmark: {
		registerAll: benchmark.RegisterAll,
		hosts:       benchmark.Hosts(), // nil: re-runs an in-box script, reaches no host
	},
	NameCode: {
		registerAll: code.RegisterAll,
		hosts:       code.Hosts(), // nil: re-runs the in-box build/test, reaches no host
	},
}

// normalize lower-cases and trims a single pack name for canonical lookup.
func normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Select registers exactly the named packs into r and nothing else. It is ATOMIC: it
// validates every name first and returns an error (registering nothing) if any is
// unknown, so a typo never leaves the registry half-populated. Names are
// case-insensitive and space-trimmed. A nil/empty list is a no-op with no error — the
// default (packs off) path, which leaves r equal to the caller's Default() so any
// pack-claim resolves Unverifiable rather than Pass.
func Select(names []string, r *evverify.Registry) error {
	if len(names) == 0 {
		return nil
	}
	// Resolve all names up front; abort before touching r on the first unknown.
	chosen := make([]pack, 0, len(names))
	for _, raw := range names {
		n := normalize(raw)
		p, ok := registry[n]
		if !ok {
			return fmt.Errorf("packs: unknown pack %q (known: %s)", strings.TrimSpace(raw), strings.Join(known(), ", "))
		}
		chosen = append(chosen, p)
	}
	for _, p := range chosen {
		p.registerAll(r)
	}
	return nil
}

// HostsFor returns a copy of the documented egress host-set the named pack reaches, or
// nil for an unknown name (and for packs whose target host is supplied per-claim, like
// web and ui). The returned slice is a fresh copy: mutating it cannot corrupt the
// canonical catalog a later HostsFor call or P11-T35 cross-check reads.
func HostsFor(name string) []string {
	p, ok := registry[normalize(name)]
	if !ok || len(p.hosts) == 0 {
		return nil
	}
	out := make([]string, len(p.hosts))
	copy(out, p.hosts)
	return out
}

// known is the sorted list of canonical pack names, used for a helpful error on an
// unknown Select name.
func known() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
