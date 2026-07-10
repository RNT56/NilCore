package main

import (
	"testing"

	"nilcore/internal/policy"
	"nilcore/internal/roster"
	"nilcore/internal/swarm/preset"
)

func has(e policy.Egress, host string) bool {
	for _, h := range e.Allowed {
		if h == host {
			return true
		}
	}
	return false
}

// The egress a shard box actually gets must be the role-INTERSECTED set, never the
// wider tree. buildSwarm stands up ONE allowlist proxy per run and points every shard
// box at it, so the proxy must enforce the narrow set — enforcing the union would
// over-permit a role whose own allowlist is narrower than the operator's tree.
func TestShardEgressIsRoleNarrowedNotTheUnion(t *testing.T) {
	pre, _, err := preset.Resolve("research")
	if err != nil {
		t.Skipf("research preset unavailable: %v", err)
	}
	if len(pre.Profile.Egress.Allowed) == 0 {
		t.Skip("research profile is deny-all in this build; nothing to narrow")
	}

	// The operator widens the tree with a host the ROLE does not allow.
	tree := shardEgress(pre, []string{"evil.example.com"})
	if !has(tree, "evil.example.com") {
		t.Fatal("test premise: --egress-allow must land in the tree")
	}

	got := roster.EgressFor(pre.Profile, tree)

	if has(got, "evil.example.com") {
		t.Error("the shard allowlist admitted a host outside the role's own allowlist — EgressFor must only narrow")
	}
	for _, h := range got.Allowed {
		if !has(pre.Profile.Egress, h) {
			t.Errorf("shard allowlist host %q is not in the role's allowlist", h)
		}
		if !has(tree, h) {
			t.Errorf("shard allowlist host %q is not in the operator's tree", h)
		}
	}
}

// A deny-all preset stays deny-all no matter how wide the operator's tree is; with an
// empty allowlist buildSwarm starts no proxy and every shard box keeps --network none.
func TestDenyAllRoleStaysDenyAllRegardlessOfTree(t *testing.T) {
	pre, _, err := preset.Resolve("audit") // audit declares no egress
	if err != nil {
		t.Skipf("audit preset unavailable: %v", err)
	}
	if !pre.Profile.Egress.Empty() {
		t.Skipf("audit profile is not deny-all in this build (%v)", pre.Profile.Egress.Allowed)
	}

	// Even a wide operator tree cannot grant a deny-all role any host: EgressFor
	// intersects, and an empty role side is deny-all.
	tree := shardEgress(pre, []string{"example.com", "pypi.org"})
	got := roster.EgressFor(pre.Profile, tree)
	if !got.Empty() {
		t.Fatalf("a deny-all role must intersect to empty (got %v) — the shard box must stay --network none", got.Allowed)
	}
}

// Applying the derived egress changed a real default: code/fix/research shards used to
// run --network none because the value was computed and dropped. Now they reach exactly
// the hosts their preset declares — no more, no less. This pins that surface so a future
// preset edit cannot silently widen what a shard may touch.
func TestPresetShardEgressSurface(t *testing.T) {
	want := map[string][]string{
		"audit": {},
		"ui":    {},
		"code":  {"api.github.com", "crates.io", "pypi.org", "registry.npmjs.org"},
		"fix":   {"api.github.com", "crates.io", "pypi.org", "registry.npmjs.org"},
		"research": {
			"api.stlouisfed.org", "api.worldbank.org", "data.sec.gov",
			"financialmodelingprep.com", "www.imf.org",
		},
	}
	for name, hosts := range want {
		pre, _, err := preset.Resolve(name)
		if err != nil {
			t.Errorf("preset %q: %v", name, err)
			continue
		}
		got := roster.EgressFor(pre.Profile, shardEgress(pre, nil)) // no --egress-allow
		if len(got.Allowed) != len(hosts) {
			t.Errorf("preset %q shard egress = %v, want %v", name, got.Allowed, hosts)
			continue
		}
		for _, h := range hosts {
			if !has(got, h) {
				t.Errorf("preset %q shard egress %v missing declared host %q", name, got.Allowed, h)
			}
		}
	}
}
