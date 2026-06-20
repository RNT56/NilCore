package main

// P11-T28 — the egress-profile wiring across both front doors. These tests are
// hermetic (no network, no container): they exercise the pure resolution helpers,
// the resolveWeb composition, the build.go tree toggle via roster.EgressFor, the
// metadata-only event, and the namespace-degrade label. The golden byte-identical
// path (nothing opted in) is asserted first.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/egressprofile"
	"nilcore/internal/eventlog"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
	"nilcore/internal/roster"
	"nilcore/internal/tools"
)

func hasHost(allow []string, h string) bool {
	for _, a := range allow {
		if a == h {
			return true
		}
	}
	return false
}

// TestEgressProfileWiring is the umbrella the Verify line names. It bundles the
// resolution precedence, the resolveWeb composition, the build.go tree toggle, the
// metadata-only event, and the fail-closed paths.
func TestEgressProfileWiring(t *testing.T) {
	t.Run("default unset => byte-identical deny-all", func(t *testing.T) {
		// No flag, no env, no config ⇒ a zero profile: empty tree, not On(), and
		// resolveWeb returns exactly the deny-all (nil) it returned before T28.
		t.Setenv(egressProfileEnv, "")
		prof, err := resolveEgressProfile(onboard.Config{}, "")
		if err != nil {
			t.Fatalf("default resolve: unexpected error %v", err)
		}
		if prof.On() || len(prof.Tree.Allowed) != 0 || len(prof.Sources) != 0 {
			t.Fatalf("default profile must be empty: %+v", prof)
		}
		// resolveWeb with nil profile hosts and no config/flag ⇒ default-deny.
		if allow, b := resolveWeb(onboard.Config{}, prof.Tree.Allowed, "", ""); allow != nil || b != tools.SearchOff {
			t.Fatalf("default resolveWeb: allow=%v backend=%v, want nil/off", allow, b)
		}
		// build.go tree toggle: an empty tree intersects every role to deny-all.
		researcher, _ := roster.NewDefault(nil, nil, policy.Egress{}).Resolve(roster.RoleResearcher)
		if got := roster.EgressFor(researcher, prof.Tree); !got.Empty() {
			t.Fatalf("empty tree must intersect researcher to deny-all, got %v", got.Allowed)
		}
	})

	t.Run("flag finance => widen tree + resolveWeb base", func(t *testing.T) {
		t.Setenv(egressProfileEnv, "")
		prof, err := resolveEgressProfile(onboard.Config{}, "finance")
		if err != nil {
			t.Fatalf("finance resolve: %v", err)
		}
		if !prof.On() || prof.Profile != "finance" {
			t.Fatalf("expected finance profile On, got %+v", prof)
		}
		// The widen-tree carries the sanctioned finance hosts.
		for _, h := range []string{"data.sec.gov", "api.stlouisfed.org"} {
			if !hasHost(prof.Tree.Allowed, h) {
				t.Fatalf("finance tree missing %q: %v", h, prof.Tree.Allowed)
			}
		}
		if len(prof.Sources) != 1 || prof.Sources[0] != "profile:finance" {
			t.Fatalf("sources = %v, want [profile:finance]", prof.Sources)
		}
		// resolveWeb: profile hosts are the BASE, added before -allow-egress extras
		// and before the search host. ddg is the keyless default.
		allow, b := resolveWeb(onboard.Config{}, prof.Tree.Allowed, "extra.example.com", "")
		if b == tools.SearchOff {
			t.Fatalf("a profile alone must enable web (backend off)")
		}
		if !hasHost(allow, "data.sec.gov") || !hasHost(allow, "extra.example.com") {
			t.Fatalf("merged allow must contain profile base + flag extra: %v", allow)
		}
		// Order: a profile host precedes the flag-extra host (profile = base).
		if idxOf(allow, "data.sec.gov") >= idxOf(allow, "extra.example.com") {
			t.Fatalf("profile host must precede flag extra in %v", allow)
		}
	})

	t.Run("env overrides config, flag overrides env", func(t *testing.T) {
		cfg := onboard.Config{Web: onboard.WebConfig{Profile: "web-research"}}
		// config only.
		if prof, err := resolveEgressProfile(cfg, ""); err != nil || prof.Profile != "web-research" {
			t.Fatalf("config profile: %+v err=%v", prof, err)
		}
		// env beats config.
		t.Setenv(egressProfileEnv, "docs")
		if prof, err := resolveEgressProfile(cfg, ""); err != nil || prof.Profile != "docs" {
			t.Fatalf("env should override config: %+v err=%v", prof, err)
		}
		// flag beats env (and config).
		if prof, err := resolveEgressProfile(cfg, "finance"); err != nil || prof.Profile != "finance" {
			t.Fatalf("flag should override env+config: %+v err=%v", prof, err)
		}
	})

	t.Run("unknown profile => fail-closed error", func(t *testing.T) {
		t.Setenv(egressProfileEnv, "")
		_, err := resolveEgressProfile(onboard.Config{}, "bogus")
		if err == nil {
			t.Fatal("unknown profile must error (fail-closed), got nil")
		}
		// The error references the valid set so the operator can self-correct.
		for _, name := range egressprofile.Names() {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error should list valid names; missing %q in %q", name, err.Error())
			}
		}
	})

	t.Run("unparseable project-local file => fail-closed error", func(t *testing.T) {
		t.Setenv(egressProfileEnv, "")
		bad := filepath.Join(t.TempDir(), "egress.json")
		if err := os.WriteFile(bad, []byte("{ not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := onboard.Config{Web: onboard.WebConfig{ProfileFile: bad}}
		if _, err := resolveEgressProfile(cfg, ""); err == nil {
			t.Fatal("unparseable file must fail closed, got nil error")
		}
	})

	t.Run("resolved tree == EXACTLY preset ∪ file, nothing else", func(t *testing.T) {
		t.Setenv(egressProfileEnv, "")
		file := filepath.Join(t.TempDir(), "egress.json")
		spec := egressprofile.FileSpec{SchemaVersion: 1, Allow: []string{"my.internal.host", "data.sec.gov"}}
		blob, _ := json.Marshal(spec)
		if err := os.WriteFile(file, blob, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := onboard.Config{Web: onboard.WebConfig{ProfileFile: file}}
		prof, err := resolveEgressProfile(cfg, "finance")
		if err != nil {
			t.Fatalf("resolve union: %v", err)
		}
		// Expected = finance preset hosts ∪ file hosts, deduped (data.sec.gov appears
		// in both ⇒ once). Build the want-set and assert set-equality with the tree.
		preset, _ := egressprofile.Named("finance")
		want := map[string]bool{}
		for _, h := range preset.Allowed {
			want[h] = true
		}
		for _, h := range spec.Allow {
			want[h] = true
		}
		got := map[string]bool{}
		for _, h := range prof.Tree.Allowed {
			if got[h] {
				t.Fatalf("tree has a duplicate host %q: %v", h, prof.Tree.Allowed)
			}
			got[h] = true
		}
		if len(got) != len(want) {
			t.Fatalf("tree set size %d, want %d (tree=%v)", len(got), len(want), prof.Tree.Allowed)
		}
		for h := range want {
			if !got[h] {
				t.Fatalf("tree missing expected host %q: %v", h, prof.Tree.Allowed)
			}
		}
		// And the file host that is NOT in any preset still made it in.
		if !got["my.internal.host"] {
			t.Fatal("file-only host must be in the resolved tree")
		}
	})

	t.Run("build.go toggle: researcher intersects, deny-all role stays --network none", func(t *testing.T) {
		t.Setenv(egressProfileEnv, "")
		prof, err := resolveEgressProfile(onboard.Config{}, "finance")
		if err != nil {
			t.Fatal(err)
		}
		// roster.NewDefault wired with d.egress as the researcher's pre-intersection
		// allowlist (mirrors build.go:353); EgressFor narrows against the same tree
		// (mirrors build.go:700). The researcher yields the intersection (non-empty);
		// a deny-all role (planner: empty Profile.Egress) stays --network none.
		rost := roster.NewDefault(nil, nil, prof.Tree)
		researcherProf, _ := rost.Resolve(roster.RoleResearcher)
		researcher := roster.EgressFor(researcherProf, prof.Tree)
		if researcher.Empty() {
			t.Fatal("researcher under the finance tree must yield a non-empty intersection")
		}
		if !hasHost(researcher.Allowed, "data.sec.gov") {
			t.Fatalf("researcher intersection should include data.sec.gov: %v", researcher.Allowed)
		}
		plannerProf, _ := rost.Resolve(roster.RolePlanner)
		planner := roster.EgressFor(plannerProf, prof.Tree)
		if !planner.Empty() {
			t.Fatalf("a deny-all role must stay --network none under any profile, got %v", planner.Allowed)
		}
	})

	t.Run("namespace backend => inert + labelled", func(t *testing.T) {
		if got := egressBackendLabel("namespace"); got != "namespace" {
			t.Fatalf("namespace label = %q, want namespace", got)
		}
		if got := egressBackendLabel("auto"); got != "container" {
			t.Fatalf("auto label = %q, want container", got)
		}
		if got := egressBackendLabel("container"); got != "container" {
			t.Fatalf("container label = %q, want container", got)
		}
	})
}

// TestEgressProfileEvent asserts the metadata-only egress_profile event is emitted
// only when a profile is opted in, and carries {profile,file,host_count,sources,
// backend} with NO hostnames-with-query-strings and NO keys (I3).
func TestEgressProfileEvent(t *testing.T) {
	t.Setenv(egressProfileEnv, "")

	t.Run("off => no event", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		log, err := eventlog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		emitEgressProfile(log, egressProfile{}, "container")
		_ = log.Close()
		if body := readLogFile(t, path); strings.Contains(body, "egress_profile") {
			t.Fatalf("off path must emit no egress_profile event, log:\n%s", body)
		}
	})

	t.Run("on => one metadata-only event", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		log, err := eventlog.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		prof, err := resolveEgressProfile(onboard.Config{}, "finance")
		if err != nil {
			t.Fatal(err)
		}
		emitEgressProfile(log, prof, "container")
		_ = log.Close()

		body := readLogFile(t, path)
		if !strings.Contains(body, "egress_profile") {
			t.Fatalf("expected egress_profile event, log:\n%s", body)
		}
		// Parse the one event and assert the Detail shape.
		var ev struct {
			Kind   string         `json:"kind"`
			Detail map[string]any `json:"detail"`
		}
		line := lastLine(t, body, "egress_profile")
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal event: %v\n%s", err, line)
		}
		for _, k := range []string{"profile", "file", "host_count", "sources", "backend"} {
			if _, ok := ev.Detail[k]; !ok {
				t.Fatalf("egress_profile Detail missing %q: %v", k, ev.Detail)
			}
		}
		if ev.Detail["profile"] != "finance" || ev.Detail["backend"] != "container" {
			t.Fatalf("unexpected Detail values: %v", ev.Detail)
		}
		// I3: no query-string host, no api_key/token anywhere in the event line.
		for _, bad := range []string{"api_key", "?", "token=", "key="} {
			if strings.Contains(line, bad) {
				t.Fatalf("event must carry no keys/query strings; found %q in:\n%s", bad, line)
			}
		}
	})
}

// idxOf returns the index of h in allow, or len(allow) if absent.
func idxOf(allow []string, h string) int {
	for i, a := range allow {
		if a == h {
			return i
		}
	}
	return len(allow)
}

// readLogFile reads the whole event-log file for assertions.
func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log %s: %v", path, err)
	}
	return string(b)
}

// lastLine returns the last JSONL line containing needle.
func lastLine(t *testing.T, body, needle string) string {
	t.Helper()
	var last string
	for _, ln := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.Contains(ln, needle) {
			last = ln
		}
	}
	if last == "" {
		t.Fatalf("no line containing %q in:\n%s", needle, body)
	}
	return last
}
