package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/onboard"
)

// TestMain unsets NILCORE_MODEL so availableBackends' native probe — which now
// resolves the SAME spec as resolveProvider("native") (modelSpec(NILCORE_MODEL,
// cfg.Executor)) — is deterministic regardless of the host environment. Tests that
// need a specific model set it explicitly via t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("NILCORE_MODEL")
	os.Exit(m.Run())
}

// credFor returns a fake SecretStore-backed resolver that yields "present" only for
// the named env vars (any other lookup ⇒ "", i.e. unavailable). It never touches a
// real store or the process environment, so availability tests are hermetic.
func credFor(present ...string) func(string) string {
	set := map[string]bool{}
	for _, e := range present {
		set[e] = true
	}
	return func(env string) string {
		if set[env] {
			return "present"
		}
		return ""
	}
}

// fakeCLIsOnPath writes empty executable stub files named bins into a fresh temp dir
// and prepends it to PATH for the duration of the test, so onboard.OnPath("codex")
// / OnPath("claude") resolve without any real CLI installed. PATH is restored on
// cleanup. Returns nothing — the effect is the mutated PATH.
func fakeCLIsOnPath(t *testing.T, bins ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, b := range bins {
		p := filepath.Join(dir, b)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", b, err)
		}
	}
	old := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+old)
}

// emptyPath points PATH at a binary-free temp dir so codex/claude are guaranteed
// ABSENT regardless of the host, isolating native-only availability tests.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func nativeCfg() onboard.Config {
	return onboard.Config{Executor: "anthropic:claude-sonnet-4-6"}
}

// TestAvailableBackendsNativeOnly: with no delegated CLIs on PATH and only the
// native executor+key present, availableBackends reports exactly [native].
func TestAvailableBackendsNativeOnly(t *testing.T) {
	emptyPath(t) // codex/claude absent
	got := availableBackends(nativeCfg(), credFor("ANTHROPIC_API_KEY"))
	if want := []string{"native"}; !reflect.DeepEqual(got, want) {
		t.Errorf("availableBackends = %v, want %v", got, want)
	}
}

// TestAvailableBackendsNoneWhenNothingConfigured: no executor, no keys, no CLIs ⇒
// the empty set (the caller treats empty as "nothing to run").
func TestAvailableBackendsNoneWhenNothingConfigured(t *testing.T) {
	emptyPath(t)
	got := availableBackends(onboard.Config{}, credFor())
	if len(got) != 0 {
		t.Errorf("availableBackends = %v, want empty", got)
	}
}

// TestAvailableBackendsIncludesDelegated: with the codex+claude stubs on PATH and
// every key present, availableBackends reports all three in canonical order.
func TestAvailableBackendsIncludesDelegated(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	got := availableBackends(nativeCfg(), credFor("ANTHROPIC_API_KEY", "CODEX_API_KEY"))
	if want := []string{"native", "codex", "claude-code"}; !reflect.DeepEqual(got, want) {
		t.Errorf("availableBackends = %v, want %v", got, want)
	}
}

// TestAvailableBackendsCLIWithoutKeyExcluded: a CLI on PATH but no key ⇒ excluded
// (both halves of the predicate must hold), so codex drops out while claude stays.
func TestAvailableBackendsCLIWithoutKeyExcluded(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	got := availableBackends(nativeCfg(), credFor("ANTHROPIC_API_KEY")) // no CODEX_API_KEY
	if want := []string{"native", "claude-code"}; !reflect.DeepEqual(got, want) {
		t.Errorf("availableBackends = %v, want %v", got, want)
	}
}

// TestAvailableBackendsNativeViaDefaultModel is the regression for the
// auto-excludes-usable-native bug: with NO config executor but the default model's
// provider key present, native is STILL available — availableBackends mirrors
// resolveProvider("native"), which resolves the built-in default model
// ("claude-sonnet-4-6" ⇒ anthropic). The old check keyed only off cfg.Executor and
// wrongly excluded native here.
func TestAvailableBackendsNativeViaDefaultModel(t *testing.T) {
	emptyPath(t) // codex/claude absent; NILCORE_MODEL unset by TestMain
	got := availableBackends(onboard.Config{}, credFor("ANTHROPIC_API_KEY"))
	if want := []string{"native"}; !reflect.DeepEqual(got, want) {
		t.Errorf("availableBackends = %v, want %v (default model + key ⇒ native usable)", got, want)
	}
}

// TestAvailableBackendsNativeViaEnvModel: a NILCORE_MODEL override (no config
// executor) plus its provider key ⇒ native available, matching resolveProvider's
// NILCORE_MODEL-wins spec resolution.
func TestAvailableBackendsNativeViaEnvModel(t *testing.T) {
	emptyPath(t)
	t.Setenv("NILCORE_MODEL", "anthropic:claude-opus-4-8")
	got := availableBackends(onboard.Config{}, credFor("ANTHROPIC_API_KEY"))
	if want := []string{"native"}; !reflect.DeepEqual(got, want) {
		t.Errorf("availableBackends = %v, want %v (NILCORE_MODEL + key ⇒ native usable)", got, want)
	}
}

// autoFlags parses run-style args into a commonFlags so resolveAutoBackend reads the
// real flag surface (-prefer-backend, -log).
func autoFlags(t *testing.T, args []string) commonFlags {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := registerCommon(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return c
}

func openLogAt(t *testing.T, path string) *eventlog.Log {
	t.Helper()
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log %s: %v", path, err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// TestResolveAutoColdStartHonorsPreference: all three available, empty ledger,
// preferred=claude-code ⇒ claude-code is chosen (preference seeds the cold start),
// and a metadata-only backend_auto event is recorded with names only.
func TestResolveAutoColdStartHonorsPreference(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	b := boot{cfg: nativeCfg(), cred: credFor("ANTHROPIC_API_KEY", "CODEX_API_KEY")}
	c := autoFlags(t, []string{"-log", logPath, "-prefer-backend", "claude-code"})
	log := openLogAt(t, logPath)

	got := resolveAutoBackend(c, b, log)
	if got != "claude-code" {
		t.Errorf("resolveAutoBackend = %q, want claude-code (preferred cold start)", got)
	}
	// Replay over a missing/empty log SUCCEEDS (a clean empty ledger), so trust_ordered
	// is true even though Order is a no-op among unproven backends — preference still
	// wins the cold start.
	assertBackendAutoLogged(t, logPath, "claude-code", true)
}

// TestResolveAutoConfigPreferenceWhenNoFlag: with no -prefer-backend flag, the
// durable config.preferred_backend seeds the cold start.
func TestResolveAutoConfigPreferenceWhenNoFlag(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	cfg := nativeCfg()
	cfg.PreferredBackend = "codex"
	b := boot{cfg: cfg, cred: credFor("ANTHROPIC_API_KEY", "CODEX_API_KEY")}
	c := autoFlags(t, []string{"-log", logPath})

	if got := resolveAutoBackend(c, b, openLogAt(t, logPath)); got != "codex" {
		t.Errorf("resolveAutoBackend = %q, want codex (config preference)", got)
	}
}

// TestResolveAutoDefaultsToNativeNoPreference: nothing preferred ⇒ native (the
// canonical first), matching today's default behavior.
func TestResolveAutoDefaultsToNativeNoPreference(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	b := boot{cfg: nativeCfg(), cred: credFor("ANTHROPIC_API_KEY", "CODEX_API_KEY")}
	c := autoFlags(t, []string{"-log", logPath})

	if got := resolveAutoBackend(c, b, openLogAt(t, logPath)); got != "native" {
		t.Errorf("resolveAutoBackend = %q, want native (no preference)", got)
	}
}

// TestResolveAutoLedgerOvertakesPreference: codex earns verifier-judged wins in the
// log; even with claude-code preferred, the ledger ranks the proven codex first.
// This is the "earn it from evidence" path — preference seeds, evidence overtakes.
func TestResolveAutoLedgerOvertakesPreference(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	seedRaceWins(t, logPath, "codex", 5) // codex: 5/5 verifier-judged wins

	b := boot{cfg: nativeCfg(), cred: credFor("ANTHROPIC_API_KEY", "CODEX_API_KEY")}
	c := autoFlags(t, []string{"-log", logPath, "-prefer-backend", "claude-code"})

	got := resolveAutoBackend(c, b, openLogAt(t, logPath))
	if got != "codex" {
		t.Errorf("resolveAutoBackend = %q, want codex (earned ledger lead overtakes preference)", got)
	}
	assertBackendAutoLogged(t, logPath, "codex", true)
}

// TestResolveAutoNativeOnly: only native available ⇒ native, regardless of a
// preference naming an unavailable backend (preference never conjures a backend).
func TestResolveAutoNativeOnly(t *testing.T) {
	emptyPath(t) // codex/claude absent
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	b := boot{cfg: nativeCfg(), cred: credFor("ANTHROPIC_API_KEY")}
	c := autoFlags(t, []string{"-log", logPath, "-prefer-backend", "claude-code"})

	if got := resolveAutoBackend(c, b, openLogAt(t, logPath)); got != "native" {
		t.Errorf("resolveAutoBackend = %q, want native (only native available)", got)
	}
}

// TestResolveAutoNoneAvailableIsFatal re-execs the test binary so resolveAutoBackend's
// os.Exit(1) FATAL (no usable backend) is observed without killing the test process.
// The child has an empty PATH and an empty cred ⇒ availableBackends is empty.
func TestResolveAutoNoneAvailableIsFatal(t *testing.T) {
	if os.Getenv("NILCORE_TEST_AUTO_NONE_FATAL") == "1" {
		dir := t.TempDir()
		logPath := filepath.Join(dir, "events.jsonl")
		b := boot{cfg: onboard.Config{}, cred: credFor()} // no executor, no keys
		c := autoFlags(t, []string{"-log", logPath})
		resolveAutoBackend(c, b, openLogAt(t, logPath)) // must fatal → os.Exit(1)
		return                                          // unreachable if it behaves
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestResolveAutoNoneAvailableIsFatal")
	cmd.Env = append(os.Environ(), "NILCORE_TEST_AUTO_NONE_FATAL=1", "PATH="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit when no backend is available; got success.\n%s", out)
	}
	if !strings.Contains(string(out), "no usable backend") {
		t.Errorf("expected a 'no usable backend' message, got:\n%s", out)
	}
}

// TestExpandAutoBackends covers the -backends auto expansion contract: a plain auto
// expands to availableBackends; auto mixed with explicit names unions them
// (dedup, explicit positions kept); no auto token is returned unchanged.
func TestExpandAutoBackends(t *testing.T) {
	fakeCLIsOnPath(t, "codex", "claude")
	cfg := nativeCfg()
	cred := credFor("ANTHROPIC_API_KEY", "CODEX_API_KEY") // all three available

	cases := []struct {
		name   string
		tokens []string
		want   []string
	}{
		{"plain auto ⇒ all available", []string{"auto"}, []string{"native", "codex", "claude-code"}},
		{"no auto ⇒ unchanged", []string{"native", "codex"}, []string{"native", "codex"}},
		{"auto union explicit dedup", []string{"claude-code", "auto"}, []string{"claude-code", "native", "codex"}},
	}
	for _, c := range cases {
		got := expandAutoBackends(c.tokens, cfg, cred)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: expandAutoBackends(%v) = %v, want %v", c.name, c.tokens, got, c.want)
		}
	}
}

// TestExpandAutoBackendsNativeOnlyCollapses: `-backends auto` on a native-only host
// collapses to [native] ⇒ wireMultiBackend reads len<=1 as the single path.
func TestExpandAutoBackendsNativeOnlyCollapses(t *testing.T) {
	emptyPath(t)
	got := expandAutoBackends([]string{"auto"}, nativeCfg(), credFor("ANTHROPIC_API_KEY"))
	if want := []string{"native"}; !reflect.DeepEqual(got, want) {
		t.Errorf("expandAutoBackends([auto]) = %v, want %v", got, want)
	}
}

// TestRunDefaultNoAutoIsByteIdentical is the default-off proof: with no -backend auto
// anywhere (and no -backends auto), the backend name stays "native" untouched and NO
// backend_auto event is written — the resolveAutoBackend branch never runs.
func TestRunDefaultNoAutoIsByteIdentical(t *testing.T) {
	emptyPath(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	c := autoFlags(t, []string{"-log", logPath})
	log := openLogAt(t, logPath)

	// Mirror runMain's guard: only "auto" triggers resolveAutoBackend.
	if *c.backendName == "auto" {
		t.Fatal("default -backend should be native, not auto")
	}
	if *c.backendName != "native" {
		t.Errorf("default backend = %q, want native", *c.backendName)
	}
	_ = log
	// No resolveAutoBackend call ⇒ no backend_auto event on disk.
	if countEvents(t, logPath, "backend_auto") != 0 {
		t.Error("default path must not write a backend_auto event")
	}
}

// --- log helpers --------------------------------------------------------------

// seedRaceWins appends n verifier-judged race_outcome wins for backend, through the
// real append-only Log so the hash chain trust.Replay verifies is valid.
func seedRaceWins(t *testing.T, logPath, backend string, n int) {
	t.Helper()
	log := openLogAt(t, logPath)
	for i := 0; i < n; i++ {
		log.Append(eventlog.Event{Kind: "race_outcome", Backend: backend, Detail: map[string]any{"passed": true}})
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close seed log: %v", err)
	}
}

// assertBackendAutoLogged checks that exactly one backend_auto event was written with
// the expected chosen backend and trust_ordered flag (metadata only — no secrets).
func assertBackendAutoLogged(t *testing.T, logPath, chosen string, trustOrdered bool) {
	t.Helper()
	evs := readEvents(t, logPath, "backend_auto")
	if len(evs) != 1 {
		t.Fatalf("backend_auto events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e.Backend != chosen {
		t.Errorf("backend_auto.Backend = %q, want %q", e.Backend, chosen)
	}
	if to, _ := e.Detail["trust_ordered"].(bool); to != trustOrdered {
		t.Errorf("backend_auto.trust_ordered = %v, want %v", to, trustOrdered)
	}
	// Secret-free audit: the only Detail keys are names/flags.
	for k := range e.Detail {
		switch k {
		case "preferred", "available", "candidates", "trust_ordered":
		default:
			t.Errorf("unexpected backend_auto detail key %q (audit must carry names only)", k)
		}
	}
}

// readEvents reads the JSONL log at logPath and returns every event of the given
// kind. The Log handle must be CLOSED first (callers open via openLogAt with a
// Cleanup close; here the file content is read directly off disk).
func readEvents(t *testing.T, logPath, kind string) []eventlog.Event {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log %s: %v", logPath, err)
	}
	var out []eventlog.Event
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e eventlog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func countEvents(t *testing.T, logPath, kind string) int {
	t.Helper()
	return len(readEvents(t, logPath, kind))
}
