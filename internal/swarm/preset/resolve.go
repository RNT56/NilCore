package preset

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/packs"
	"nilcore/internal/roster"
)

// ErrUnknownPreset is returned by Resolve for a name not in the catalog. It is the
// fail-closed signal the cmd layer (SW-T17) turns into a FATAL at startup BEFORE any
// shard runs — there is no fallback to a permissive default, because a typo must never
// silently downgrade the run to an unverified shape. (It inverts verify.Detect's
// best-effort default: here an unknown selection refuses to start rather than guessing.)
var ErrUnknownPreset = errors.New("preset: unknown preset name")

// Resolve turns a preset name into a runnable bundle: the completed Preset (Profile +
// DERIVED Egress) and the verifier Registry whose checks decide GREEN. It fails closed —
// an unknown name returns ErrUnknownPreset and a nil registry, never a permissive
// fallback. The registry it returns is evverify.Default() (which registers NO always-pass
// or noop verifier) plus EXACTLY the preset's verify packs, so a claim can only become
// green via an affirmative pack check (I2). The two derivations are the whole point of
// resolving rather than hand-writing a Preset:
//
//   - Egress is computed as the UNION of the selected packs' packs.HostsFor — never
//     hand-typed — so the network surface always matches the packs actually wired. A pack
//     with a nil host-set (audit/benchmark/code/ui/web reach a per-claim or in-box target)
//     contributes nothing, so e.g. the benchmark preset derives an EMPTY egress
//     (--network none after intersection), which is correct: its check re-runs in-box.
//   - Profile is built from the role via roster.PresetProfile with that derived egress, so
//     the write/read capability comes from the role's WRITE nature (ReadOnly:false for the
//     four write roles), NOT the hardcoded Role.ReadOnly() helper (the SW-T15 gotcha).
//
// The returned Preset is a copy; the package catalog is never mutated. The Profile's Model
// is left nil — the cmd layer attaches the live worker provider before building the worker.
func Resolve(name string) (Preset, *evverify.Registry, error) {
	p, ok := Lookup(name)
	if !ok {
		return Preset{}, nil, fmt.Errorf("%w: %q (known: %s)", ErrUnknownPreset, strings.TrimSpace(name), strings.Join(Names(), ", "))
	}

	// Build the verifier registry: the fail-closed default (web.url_resolves only, no
	// always-pass verifier) plus exactly this preset's packs. Select is atomic — an
	// unknown pack name aborts before registering anything — but the catalog only names
	// shipped packs, so a Select error here is a wiring bug, surfaced (not swallowed).
	reg := evverify.Default()
	if err := packs.Select(p.VerifyPacks, reg); err != nil {
		return Preset{}, nil, fmt.Errorf("preset %q: selecting verify packs: %w", p.Name, err)
	}

	// Derive the egress as the union of the selected packs' documented host-sets. This is
	// the single source of truth (packs.HostsFor) so the network surface can never drift
	// from the packs actually wired into the registry above.
	egress := egressUnion(p.VerifyPacks)
	p.Egress = egress

	// Build the role's Profile with the derived egress. PresetProfile fails closed for an
	// unknown role; every catalog role is one of the four it handles, so a false here is a
	// wiring bug we surface rather than ship a surprise default.
	prof, ok := roster.PresetProfile(p.Role, egress)
	if !ok {
		return Preset{}, nil, fmt.Errorf("preset %q: no profile for role %q", p.Name, p.Role)
	}
	p.Profile = prof

	return p, reg, nil
}

// egressUnion returns the sorted, de-duplicated union of the named packs' egress host-sets
// (packs.HostsFor). A pack whose target is per-claim or in-box (nil host-set) contributes
// nothing, so a preset built only of such packs derives an empty allowlist — correct, since
// its checks reach no fixed host. The result is sorted for a deterministic egress surface
// (stable across runs and easy to assert in tests).
func egressUnion(packNames []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range packNames {
		for _, host := range packs.HostsFor(name) {
			if !seen[host] {
				seen[host] = true
				out = append(out, host)
			}
		}
	}
	sort.Strings(out)
	return out
}

// normalize lower-cases and trims a preset name for canonical lookup (matching the packs
// aggregator's normalization, so " Research " resolves the same as "research").
func normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// sortStrings sorts a slice in place (a tiny stdlib wrapper kept local so preset.go does
// not import sort directly — the catalog file stays dependency-free).
func sortStrings(xs []string) {
	sort.Strings(xs)
}
