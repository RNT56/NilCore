package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/backend"
	"nilcore/internal/memory"
	"nilcore/internal/onboard"
	"nilcore/internal/sandbox"
	"nilcore/internal/session"
	"nilcore/internal/verify"
)

// TestNativeFrontDoorsShareRuntimeCapabilities is the constructor-level parity gate.
// It drives the real run, chat/TUI, and serve builders and asserts every one receives
// the same cross-cutting runtime seams. Before configureNativeRuntime existed, omitting
// one of these assignments on a single front door compiled and its isolated tests stayed
// green; this test makes that class of built-but-inert regression discriminating.
func TestNativeFrontDoorsShareRuntimeCapabilities(t *testing.T) {
	t.Setenv("NILCORE_LIVE_INDEX", "0")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "NILCORE.md"), []byte("shared steering marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	steps := 8
	prov := &fakeProvider{id: "main"}
	advProv := &fakeProvider{id: "advisor"}
	adv := advisorCfg{prov: advProv, maxCalls: 2, escalateAfter: 3}
	mem := memory.New(nil) // construction-only: the callback is asserted, never invoked
	box := sandbox.NewContainer("podman", "img", dir)

	run, ok := buildBackend("native", prov, func(string) string { return "" }, adv, box,
		verify.Pass{}, nil, steps, mem, dir, onboard.Config{}).(*backend.Native)
	if !ok {
		t.Fatal("buildBackend(native) did not return *backend.Native")
	}

	chat := chatNativeBackend(chatDeps{
		flags: newChatFlags(dir), provider: prov, baseRepo: dir, mem: mem,
	}, prov, adv, box, verify.Pass{}, session.NativeRun{Mode: session.ModeExecute})

	serve := serveNativeBackend(serveDeps{
		flags: commonFlags{maxSteps: &steps}, provider: prov, baseRepo: dir, mem: mem,
	}, prov, adv, box, verify.Pass{}, session.NativeRun{Mode: session.ModeExecute}, nil)

	for name, n := range map[string]*backend.Native{"run": run, "chat/tui": chat, "serve": serve} {
		t.Run(name, func(t *testing.T) {
			if n.RepoContext == nil {
				t.Fatal("RepoContext is not wired")
			}
			if n.CtxWindow == nil {
				t.Fatal("CtxWindow is not wired")
			}
			if n.Advisor == nil || n.EscalateAfter != adv.escalateAfter {
				t.Fatalf("advisor parity lost: advisor=%v escalate_after=%d", n.Advisor, n.EscalateAfter)
			}
			if n.MemoryContext == nil {
				t.Fatal("MemoryContext is not wired")
			}
			if n.SteeringContext == nil || !strings.Contains(n.SteeringContext(), "shared steering marker") {
				t.Fatal("SteeringContext is not wired from the principal repo")
			}
			if n.LiveSession != nil {
				t.Fatal("NILCORE_LIVE_INDEX=0 must keep LiveSession disabled")
			}
		})
	}

	// The opt-in live-index seam must also reach every constructor through the same
	// configurator. This is a second, positive control; merely asserting the default-off
	// path would not catch a regression that dropped LiveSession from the helper itself.
	t.Setenv("NILCORE_LIVE_INDEX", "1")
	liveRun := buildBackend("native", prov, func(string) string { return "" }, adv, box,
		verify.Pass{}, nil, steps, mem, dir, onboard.Config{}).(*backend.Native)
	liveChat := chatNativeBackend(chatDeps{
		flags: newChatFlags(dir), provider: prov, baseRepo: dir, mem: mem,
	}, prov, adv, box, verify.Pass{}, session.NativeRun{Mode: session.ModeExecute})
	liveServe := serveNativeBackend(serveDeps{
		flags: commonFlags{maxSteps: &steps}, provider: prov, baseRepo: dir, mem: mem,
	}, prov, adv, box, verify.Pass{}, session.NativeRun{Mode: session.ModeExecute}, nil)
	for name, n := range map[string]*backend.Native{"run": liveRun, "chat/tui": liveChat, "serve": liveServe} {
		if n.LiveSession == nil {
			t.Errorf("%s: NILCORE_LIVE_INDEX=1 did not wire LiveSession", name)
		}
	}
}
