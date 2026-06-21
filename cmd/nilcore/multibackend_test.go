package main

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/eventlog"
	"nilcore/internal/onboard"
)

// buildCommon parses args into a commonFlags exactly as the run-style commands do, so
// the multi-backend wiring is exercised through the real flag surface (no hand-built
// pointers that could drift from registerCommon's defaults).
func buildCommon(t *testing.T, args []string) commonFlags {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := registerCommon(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return c
}

// logAt opens an append-only event log at the given path (a distinct helper from
// build_test.go's openTestLog, which picks its own temp path — here the path must
// match the -log flag so wireMultiBackend's trust.Replay reads the same file).
func logAt(t *testing.T, path string) *eventlog.Log {
	t.Helper()
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log %s: %v", path, err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

func fakeBoot() boot {
	// A cred that supplies a key for every backend's provider/CLI, so resolveProvider
	// ("native" → anthropic) and the delegated backends construct offline (no network,
	// no real key) — buildBackend only stores the value; nothing is dialed.
	return boot{cfg: onboard.Config{}, cred: func(string) string { return "test-key" }}
}

// TestParseBackends covers the flag → name-list contract: empty ⇒ nil (single path),
// order is preserved, duplicates collapse, whitespace is trimmed, and a single name
// stays a one-element slice (the orchestrator reads len<=1 as the single path).
func TestParseBackends(t *testing.T) {
	cases := []struct {
		spec string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"native", []string{"native"}},
		{"native,codex", []string{"native", "codex"}},
		{" native , codex , claude-code ", []string{"native", "codex", "claude-code"}},
		{"codex,native,codex", []string{"codex", "native"}}, // dedup, order of first sight
		{"native,,codex", []string{"native", "codex"}},      // empty entries dropped
	}
	for _, c := range cases {
		got := parseBackends(c.spec)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseBackends(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

// TestWireMultiBackendDefaultOff is the byte-identical proof: with no -backends, or a
// single -backends name, wireMultiBackend leaves Backends/NewEnvFor/Selector UNSET, so
// the orchestrator stays on the existing single-backend path. Only two or more DISTINCT
// names activate the multi fields.
func TestWireMultiBackendDefaultOff(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl") // missing ⇒ trust.Replay yields an empty ledger
	b := fakeBoot()

	off := func(name string, args []string) {
		t.Run(name, func(t *testing.T) {
			c := buildCommon(t, append(args, "-log", logPath))
			o := &agent.Orchestrator{}
			wireMultiBackend(o, c, b, logAt(t, logPath), nil, dir)
			if o.Backends != nil {
				t.Errorf("Backends should be nil on the single path, got %v", o.Backends)
			}
			if o.NewEnvFor != nil {
				t.Error("NewEnvFor should be nil on the single path")
			}
			if o.Selector != nil {
				t.Error("Selector should be nil on the single path")
			}
		})
	}
	off("no -backends", nil)
	off("single -backends", []string{"-backends", "native"})
	off("single -backends with dup", []string{"-backends", "codex,codex"})
}

// TestWireMultiBackendActivates proves the multi path: two or more DISTINCT names set
// all three fields. The Selector is non-nil (a trust.Selector over the missing/empty
// log ⇒ configured order until evidence accrues — never an error), and NewEnvFor
// resolves each backend FRESH by name.
func TestWireMultiBackendActivates(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	b := fakeBoot()

	c := buildCommon(t, []string{"-backends", "native,codex,claude-code", "-log", logPath})
	o := &agent.Orchestrator{}
	wireMultiBackend(o, c, b, logAt(t, logPath), nil, dir)

	if want := []string{"native", "codex", "claude-code"}; !reflect.DeepEqual(o.Backends, want) {
		t.Errorf("Backends = %v, want %v", o.Backends, want)
	}
	if o.NewEnvFor == nil {
		t.Fatal("NewEnvFor must be set on the multi path")
	}
	if o.Selector == nil {
		t.Error("Selector must be set on the multi path (empty ledger over a missing log is still a valid Selector)")
	}
}

// TestMultiEnvFactorySwapsBackendByName proves NewEnvFor builds the SAME per-worktree
// env (a verifier is always present) but swaps the BACKEND by name — native, codex,
// and claude-code each construct the matching backend, each resolving its own creds.
func TestMultiEnvFactorySwapsBackendByName(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	b := fakeBoot()
	c := buildCommon(t, []string{"-log", logPath})
	newEnvFor := multiEnvFactory(c, b, logAt(t, logPath), nil, dir)

	for _, name := range []string{"native", "codex", "claude-code"} {
		env := newEnvFor(dir, name)
		if env.Backend == nil {
			t.Fatalf("%s: env.Backend is nil", name)
		}
		if env.Backend.Name() != name {
			t.Errorf("NewEnvFor(_, %q).Backend.Name() = %q, want %q", name, env.Backend.Name(), name)
		}
		if env.Verifier == nil {
			t.Errorf("%s: env.Verifier should always be present (backend-independent)", name)
		}
	}
}

// TestParseBackendsUnknownIsFatal re-execs the test binary so the os.Exit(1) FATAL of
// an unknown -backends name is observed without taking down the test process — the
// standard Go idiom for asserting a fatal exit. The child runs parseBackends under the
// guard env var and must exit non-zero with the unknown-backend message.
func TestParseBackendsUnknownIsFatal(t *testing.T) {
	if os.Getenv("NILCORE_TEST_PARSE_BACKENDS_FATAL") == "1" {
		parseBackends("native,foo") // must call fatal(...) → os.Exit(1)
		return                      // unreachable if parseBackends behaves
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestParseBackendsUnknownIsFatal")
	cmd.Env = append(os.Environ(), "NILCORE_TEST_PARSE_BACKENDS_FATAL=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit on an unknown -backends name; got success.\n%s", out)
	}
	if !strings.Contains(string(out), "unknown backend") {
		t.Errorf("expected an 'unknown backend' message, got:\n%s", out)
	}
}
