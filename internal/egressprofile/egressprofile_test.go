package egressprofile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestPresets asserts each named preset resolves, is non-empty, contains its
// co-designed hosts, and that every literal preset host passes its own
// policy.Egress.Allow — the property that lets it survive roster.intersectEgress.
func TestPresets(t *testing.T) {
	t.Run("known and unknown names", func(t *testing.T) {
		for _, name := range []string{ProfileFinance, ProfileDocs, ProfileWebResearch} {
			eg, ok := Named(name)
			if !ok {
				t.Fatalf("Named(%q) ok=false, want true", name)
			}
			if len(eg.Allowed) == 0 {
				t.Fatalf("Named(%q) returned empty allowlist", name)
			}
		}
		if eg, ok := Named("bogus"); ok || len(eg.Allowed) != 0 {
			t.Fatalf("Named(bogus) = (%v,%v), want (zero,false)", eg, ok)
		}
	})

	t.Run("finance contains co-designed hosts", func(t *testing.T) {
		eg, _ := Named(ProfileFinance)
		for _, want := range []string{"data.sec.gov", "api.stlouisfed.org", "api.worldbank.org", "www.imf.org"} {
			if !contains(eg.Allowed, want) {
				t.Errorf("finance preset missing %q (have %v)", want, eg.Allowed)
			}
		}
	})

	t.Run("every preset host allows itself", func(t *testing.T) {
		// A literal host must Allow itself so it survives intersection against a
		// role allowlist that lists that exact host. A wildcard must Allow a host
		// it covers.
		for _, name := range Names() {
			eg, _ := Named(name)
			for _, h := range eg.Allowed {
				probe := h
				if strings.HasPrefix(h, "*.") {
					probe = "sub" + h[1:] // "*.wikipedia.org" -> "sub.wikipedia.org"
				}
				if !eg.Allow(probe) {
					t.Errorf("preset %q: host %q does not Allow probe %q", name, h, probe)
				}
			}
		}
	})

	t.Run("Named returns a defensive copy", func(t *testing.T) {
		eg, _ := Named(ProfileFinance)
		if len(eg.Allowed) > 0 {
			eg.Allowed[0] = "evil.example.com"
		}
		again, _ := Named(ProfileFinance)
		if contains(again.Allowed, "evil.example.com") {
			t.Fatal("mutating a Named() result leaked into the package presets")
		}
	})
}

// TestNames asserts the closed, sorted set.
func TestNames(t *testing.T) {
	got := Names()
	want := []string{"browse", "docs", "finance", "web-research"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("Names() not sorted: %v", got)
	}
}

// TestResolve covers the four Resolve combinations from the spec.
func TestResolve(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, "egress.json")
	writeFile(t, fpath, FileSpec{SchemaVersion: 1, Allow: []string{"example.com", "data.sec.gov"}})

	t.Run("profile only", func(t *testing.T) {
		tree, sources, err := Resolve(ProfileFinance, "")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		fin, _ := Named(ProfileFinance)
		if !reflect.DeepEqual(tree.Allowed, fin.Allowed) {
			t.Fatalf("tree = %v, want finance hosts %v", tree.Allowed, fin.Allowed)
		}
		if !reflect.DeepEqual(sources, []string{"profile:finance"}) {
			t.Fatalf("sources = %v, want [profile:finance]", sources)
		}
	})

	t.Run("file only", func(t *testing.T) {
		tree, sources, err := Resolve("", fpath)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !reflect.DeepEqual(tree.Allowed, []string{"example.com", "data.sec.gov"}) {
			t.Fatalf("tree = %v, want file hosts", tree.Allowed)
		}
		if !reflect.DeepEqual(sources, []string{"file:" + fpath}) {
			t.Fatalf("sources = %v, want [file:%s]", sources, fpath)
		}
	})

	t.Run("profile and file deduped union with both sources", func(t *testing.T) {
		tree, sources, err := Resolve(ProfileFinance, fpath)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		// data.sec.gov appears in both preset and file — must appear exactly once.
		if n := count(tree.Allowed, "data.sec.gov"); n != 1 {
			t.Fatalf("data.sec.gov appears %d times, want 1 (deduped)", n)
		}
		if !contains(tree.Allowed, "example.com") {
			t.Fatalf("union missing file-only host example.com: %v", tree.Allowed)
		}
		fin, _ := Named(ProfileFinance)
		for _, h := range fin.Allowed {
			if !contains(tree.Allowed, h) {
				t.Fatalf("union missing preset host %q", h)
			}
		}
		if !reflect.DeepEqual(sources, []string{"profile:finance", "file:" + fpath}) {
			t.Fatalf("sources = %v, want both", sources)
		}
	})

	t.Run("preset hosts come before file-only hosts", func(t *testing.T) {
		tree, _, err := Resolve(ProfileFinance, fpath)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		exampleIdx := indexOf(tree.Allowed, "example.com")
		secIdx := indexOf(tree.Allowed, "data.sec.gov")
		if secIdx == -1 || exampleIdx == -1 || secIdx >= exampleIdx {
			t.Fatalf("expected preset host before file-only host, got %v", tree.Allowed)
		}
	})

	t.Run("empty empty is the byte-identical default", func(t *testing.T) {
		tree, sources, err := Resolve("", "")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !tree.Empty() {
			t.Fatalf("Resolve(\"\",\"\") tree = %v, want empty deny-all", tree.Allowed)
		}
		if len(sources) != 0 {
			t.Fatalf("Resolve(\"\",\"\") sources = %v, want none", sources)
		}
	})

	t.Run("unknown profile fails closed", func(t *testing.T) {
		tree, sources, err := Resolve("bogus", "")
		var upe *UnknownProfileError
		if !errors.As(err, &upe) {
			t.Fatalf("err = %v, want *UnknownProfileError", err)
		}
		if !tree.Empty() || sources != nil {
			t.Fatalf("on error want empty tree + nil sources, got %v / %v", tree, sources)
		}
		if !strings.Contains(err.Error(), "finance") {
			t.Fatalf("error should list valid names: %q", err.Error())
		}
	})

	t.Run("unreadable file fails closed", func(t *testing.T) {
		tree, sources, err := Resolve("", filepath.Join(dir, "missing.json"))
		if err == nil {
			t.Fatal("Resolve with missing file returned nil error, want fail-closed")
		}
		if !errors.Is(err, ErrFileNotFound) {
			t.Fatalf("err = %v, want ErrFileNotFound", err)
		}
		if !tree.Empty() || sources != nil {
			t.Fatalf("on error want empty tree + nil sources, got %v / %v", tree, sources)
		}
	})
}

// TestLoadFile covers the project-local allowlist loader provided here so
// Resolve's file path resolves (T26 owns its full surface).
func TestLoadFile(t *testing.T) {
	if DefaultFilePath != ".nilcore/egress.json" {
		t.Fatalf("DefaultFilePath = %q, want .nilcore/egress.json", DefaultFilePath)
	}
	dir := t.TempDir()

	t.Run("reads allow list preserving order and trimming", func(t *testing.T) {
		p := filepath.Join(dir, "ok.json")
		if err := os.WriteFile(p, []byte(`{"schema_version":1,"allow":["  a.com ","*.b.org",""]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		eg, err := LoadFile(p)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		if !reflect.DeepEqual(eg.Allowed, []string{"a.com", "*.b.org"}) {
			t.Fatalf("Allowed = %v, want [a.com *.b.org] (trimmed, empty dropped)", eg.Allowed)
		}
		// both feed Allow correctly
		if !eg.Allow("a.com") || !eg.Allow("x.b.org") {
			t.Fatalf("literal+wildcard host not matched: %v", eg.Allowed)
		}
	})

	t.Run("golden JSON shape includes schema_version and round-trips", func(t *testing.T) {
		// Golden: the canonical on-disk shape carries schema_version + allow[].
		const golden = `{"schema_version":1,"allow":["data.sec.gov","*.gov"]}`
		spec := FileSpec{SchemaVersion: fileSchemaVersion, Allow: []string{"data.sec.gov", "*.gov"}}
		data, err := json.Marshal(spec)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if string(data) != golden {
			t.Fatalf("serialized FileSpec = %s, want golden %s", data, golden)
		}
		// LoadFile of the golden bytes yields the same hosts (order preserved).
		p := filepath.Join(dir, "golden.json")
		if err := os.WriteFile(p, []byte(golden), 0o644); err != nil {
			t.Fatal(err)
		}
		eg, err := LoadFile(p)
		if err != nil {
			t.Fatalf("LoadFile: %v", err)
		}
		if !reflect.DeepEqual(eg.Allowed, []string{"data.sec.gov", "*.gov"}) {
			t.Fatalf("Allowed = %v, want golden hosts", eg.Allowed)
		}
	})

	t.Run("missing file is typed not-found, not a parse error", func(t *testing.T) {
		_, err := LoadFile(filepath.Join(dir, "nope.json"))
		if !errors.Is(err, ErrFileNotFound) {
			t.Fatalf("err = %v, want ErrFileNotFound", err)
		}
	})

	t.Run("malformed JSON is a parse error, never silent zero-value", func(t *testing.T) {
		p := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		eg, err := LoadFile(p)
		if err == nil {
			t.Fatalf("LoadFile of malformed JSON returned nil error, eg=%v", eg)
		}
		if errors.Is(err, ErrFileNotFound) {
			t.Fatalf("malformed JSON should not be ErrFileNotFound: %v", err)
		}
	})
}

func contains(s []string, v string) bool { return indexOf(s, v) >= 0 }

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func count(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

func writeFile(t *testing.T, path string, spec FileSpec) {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
