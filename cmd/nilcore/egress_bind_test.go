package main

import (
	"testing"

	"nilcore/internal/sandbox"
)

// proxyBindAddr binds the egress proxy to loopback for backends that never expose it
// to a bridged container, and to all interfaces only when a container will consume it.
func TestProxyBindAddr(t *testing.T) {
	t.Setenv("NILCORE_SANDBOX", "") // don't let the host env leak into the explicit cases

	if got := proxyBindAddr("namespace", ""); got != "127.0.0.1:0" {
		t.Errorf("namespace ⇒ %q, want 127.0.0.1:0 (proxy unused, no LAN exposure)", got)
	}
	if got := proxyBindAddr("container", ""); got != "0.0.0.0:0" {
		t.Errorf("container ⇒ %q, want 0.0.0.0:0 (reachable across the runtime bridge)", got)
	}

	// NILCORE_SANDBOX overrides an auto/empty preference (mirrors selectSandbox).
	t.Setenv("NILCORE_SANDBOX", "container")
	if got := proxyBindAddr("auto", ""); got != "0.0.0.0:0" {
		t.Errorf("auto + NILCORE_SANDBOX=container ⇒ %q, want 0.0.0.0:0", got)
	}
	t.Setenv("NILCORE_SANDBOX", "namespace")
	if got := proxyBindAddr("", ""); got != "127.0.0.1:0" {
		t.Errorf("empty + NILCORE_SANDBOX=namespace ⇒ %q, want 127.0.0.1:0", got)
	}

	// Bare auto with no override resolves consistently with the host's actual backend
	// availability — container (0.0.0.0) only when the namespace backend is unavailable.
	t.Setenv("NILCORE_SANDBOX", "")
	ns, _, _ := sandbox.Available("")
	want := "0.0.0.0:0"
	if ns {
		want = "127.0.0.1:0"
	}
	if got := proxyBindAddr("auto", ""); got != want {
		t.Errorf("auto ⇒ %q, want %q (namespace-available=%v)", got, want, ns)
	}
}
