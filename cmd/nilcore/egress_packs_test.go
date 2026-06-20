package main

// P11-T35 — the one machine-checkable proof that "Pillar 5 unlocks Pillar 2":
// every host a domain verifier pack reaches MUST be permitted by the egress
// profile that governs that domain. If the two catalogs drift (a pack adds an
// endpoint the profile does not sanction), a research swarm running under the
// profile would hit a denied host and the claim would fail closed — so we catch
// the drift HERE, in the cmd layer that imports both leaves, keeping the two
// packages decoupled (neither pack imports egressprofile, nor vice versa).

import (
	"testing"

	"nilcore/internal/artifact/packs"
	"nilcore/internal/egressprofile"
)

// packProfile maps each verifier pack with a FIXED egress host-set to the egress
// profile that must sanction it. The web and ui packs are intentionally omitted:
// they reach a per-claim Evidence.SourceURL host (packs.HostsFor returns nil), so
// there is no fixed set to subset — the operator's profile/file must already
// permit the specific target.
var packProfile = map[string]string{
	packs.NameFinance:  egressprofile.ProfileFinance,
	packs.NameSoftware: egressprofile.ProfileDocs,
}

func TestEgressPackHostConsistency(t *testing.T) {
	for pack, profile := range packProfile {
		hosts := packs.HostsFor(pack)
		if len(hosts) == 0 {
			t.Errorf("pack %q has no fixed egress hosts; expected a non-empty catalog to cross-check against profile %q", pack, profile)
			continue
		}
		egr, ok := egressprofile.Named(profile)
		if !ok {
			t.Errorf("egress profile %q (governing pack %q) is not a known preset", profile, pack)
			continue
		}
		for _, h := range hosts {
			if !egr.Allow(h) {
				t.Errorf("pack %q host %q is NOT permitted by egress profile %q — packs.HostsFor and "+
					"egressprofile.Named have drifted; add %q to the %q preset", pack, h, profile, h, profile)
			}
		}
	}

	// The host-per-claim packs must keep returning nil (no fixed allowlist): that
	// contract is what lets a web/ui claim target whatever site the operator's
	// profile/file already permits, rather than a baked-in host set.
	for _, pack := range []string{packs.NameWeb, packs.NameUI} {
		if hosts := packs.HostsFor(pack); hosts != nil {
			t.Errorf("pack %q is host-per-claim and must return nil from HostsFor, got %v", pack, hosts)
		}
	}
}
