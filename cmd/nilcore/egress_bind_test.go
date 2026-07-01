package main

import (
	"testing"

	"nilcore/internal/sandbox"
)

// proxyBindAddr binds the egress proxy to loopback for backends that never expose it
// to a bridged container, and to all interfaces only when a container will consume it.
func TestProxyBindAddr(t *testing.T) {
	t.Setenv("NILCORE_SANDBOX", "") // don't let the host env leak into the explicit cases

	// The namespace backend never uses the proxy, so an explicit/auto namespace request
	// binds loopback — UNLESS the namespace backend is unavailable, where selectSandbox
	// DEGRADES to a *sandbox.Container that needs 0.0.0.0 across the bridge. nsWant tracks
	// that degradation (on a non-Linux host like macOS, ns is false ⇒ 0.0.0.0). This is
	// the silent-egress-failure regression the test guards.
	ns, _, _ := sandbox.Available("")
	nsWant := "0.0.0.0:0"
	if ns {
		nsWant = "127.0.0.1:0"
	}

	// Explicit container always binds all-interfaces (deterministic).
	if got := proxyBindAddr("container", ""); got != "0.0.0.0:0" {
		t.Errorf("container ⇒ %q, want 0.0.0.0:0 (reachable across the runtime bridge)", got)
	}
	// Explicit namespace tracks availability (degrades to a container when unavailable).
	if got := proxyBindAddr("namespace", ""); got != nsWant {
		t.Errorf("namespace ⇒ %q, want %q (namespace-available=%v; degrades to container otherwise)", got, nsWant, ns)
	}
	// Bare auto / empty also tracks availability.
	if got := proxyBindAddr("auto", ""); got != nsWant {
		t.Errorf("auto ⇒ %q, want %q (namespace-available=%v)", got, nsWant, ns)
	}

	// NILCORE_SANDBOX overrides an auto/empty preference (mirrors selectSandbox).
	t.Setenv("NILCORE_SANDBOX", "container")
	if got := proxyBindAddr("auto", ""); got != "0.0.0.0:0" {
		t.Errorf("auto + NILCORE_SANDBOX=container ⇒ %q, want 0.0.0.0:0", got)
	}
	t.Setenv("NILCORE_SANDBOX", "namespace")
	if got := proxyBindAddr("", ""); got != nsWant {
		t.Errorf("empty + NILCORE_SANDBOX=namespace ⇒ %q, want %q", got, nsWant)
	}
}
