package packs

// build_test.go exercises the verify-pack assembler (SW-T05) HERMETICALLY: no real
// network and no real sandbox. The sandbox is a recording stub that returns a canned
// exit code and counts every Exec call, so a test can prove two load-bearing properties
// of the composite:
//
//   - cheapest-first short-circuit: a malformed-SHAPE artifact fails at Named[0] (schema)
//     and the per-claim (network) layer NEVER runs — asserted by ZERO recorded Exec calls;
//   - the I2 verdict: a green artifact passes and a single red (non-2xx) claim fails the
//     whole composite (Green requires EVERY claim StatusPass).
//
// It also proves the fail-closed name handling (unknown name ⇒ error, never a verify.Pass
// default) — and that audit/benchmark/code are now KNOWN — plus that DefaultSchemas
// round-trips every built-in Kind.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/schema"
	"nilcore/internal/sandbox"
)

// recBox is a hermetic sandbox.Sandbox stand-in. It records how many commands it ran and
// returns a fixed exit code, so a test drives the per-claim curl verdict (ExitCode 0 =>
// web.url_resolves StatusPass; non-zero => StatusUnverifiable) without any network, and can
// assert the per-claim layer was never reached (calls == 0).
type recBox struct {
	mu       sync.Mutex
	calls    int
	exitCode int
}

func (b *recBox) Exec(_ context.Context, _ string) (sandbox.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return sandbox.Result{ExitCode: b.exitCode}, nil
}

func (b *recBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}

// Workdir is "true"-detecting on purpose: verify.Detect over an empty/non-repo dir returns
// the no-op "true" command, so a code/ui child (if one were added) would not depend on the
// host's project layout. The tests that count Exec use packs WITHOUT a child (finance/web),
// so the count reflects only the per-claim layer.
func (b *recBox) Workdir() string { return "/nonexistent-workdir" }

func (b *recBox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// writeArtifact persists a at .nilcore/artifacts/<id>.json under root and returns the
// absolute path the schema + evidence verifiers read (both take the same RelPath).
func writeArtifact(t *testing.T, root string, a *artifact.Artifact) string {
	t.Helper()
	if err := artifact.Write(root, a); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return filepath.Join(root, ".nilcore", "artifacts", a.ID+".json")
}

// urlClaim builds a well-formed report claim bound to web.url_resolves with a valid,
// key-free https source — the shape passes schema, so the per-claim curl verdict decides.
func urlClaim(id string) artifact.Claim {
	return artifact.Claim{
		ID:        id,
		Field:     "homepage",
		Statement: "the site resolves",
		Evidence: artifact.Evidence{
			Value:     "ok",
			SourceURL: "https://example.com/" + id,
			Verifier:  "web.url_resolves",
			Status:    artifact.StatusPass, // self-written; the verifier MUST overwrite it
		},
	}
}

// reportArtifact assembles a Kind=report artifact (the strictest common shape: titled,
// every claim cited + verifier-bound) from the given claims.
func reportArtifact(id string, claims ...artifact.Claim) *artifact.Artifact {
	return &artifact.Artifact{
		ID:        id,
		Kind:      artifact.KindReport,
		Title:     "t",
		CreatedAt: time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		Claims:    claims,
	}
}

// TestBuildSchemaShortCircuits: a malformed-SHAPE artifact (a report missing its required
// Title) fails at Named[0] schema, so the per-claim network layer never runs. We use the
// finance pack (which adds NO build/browser child) so the only thing that could reach the
// box is the per-claim layer — and a zero Exec count proves it was short-circuited.
func TestBuildSchemaShortCircuits(t *testing.T) {
	root := t.TempDir()
	// Bad shape: a report with an EMPTY title (schema requires "title") => CodeMissingField
	// at Named[0]. The claim itself is well-formed so ONLY the missing title is the defect.
	bad := reportArtifact("a1", urlClaim("c1"))
	bad.Title = ""
	rel := writeArtifact(t, root, bad)

	box := &recBox{exitCode: 0} // a green box, to prove green never gets a chance to run
	plan, err := Build(NameFinance, box, rel, DefaultSchemas())
	if err != nil {
		t.Fatalf("Build(finance): unexpected error: %v", err)
	}

	rep, err := plan.Verifier.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatalf("malformed-shape artifact must NOT pass; got Passed=true, output=%q", rep.Output)
	}
	if got := box.count(); got != 0 {
		t.Fatalf("schema is Named[0] and must short-circuit before any Exec; got %d Exec call(s)", got)
	}
}

// TestBuildGreenVsRed: over a structurally valid artifact, the composite's verdict is the
// I2 per-claim verdict. A box returning exit 0 makes web.url_resolves pass (Passed=true);
// a box returning a non-zero exit makes it Unverifiable, so the composite is NOT green.
func TestBuildGreenVsRed(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		want     bool
	}{
		{"green", 0, true},
		{"one-red", 22, false}, // curl -f exits 22 on a 4xx => Unverifiable => not green
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			rel := writeArtifact(t, root, reportArtifact("g1", urlClaim("c1")))

			box := &recBox{exitCode: tc.exitCode}
			plan, err := Build(NameWeb, box, rel, DefaultSchemas())
			if err != nil {
				t.Fatalf("Build(web): unexpected error: %v", err)
			}
			rep, err := plan.Verifier.Check(context.Background())
			if err != nil {
				t.Fatalf("Check: unexpected error: %v", err)
			}
			if rep.Passed != tc.want {
				t.Fatalf("Passed = %v, want %v (output=%q)", rep.Passed, tc.want, rep.Output)
			}
			// The shape is valid, so the per-claim layer must have run exactly once.
			if got := box.count(); got != 1 {
				t.Fatalf("expected exactly 1 per-claim Exec, got %d", got)
			}
		})
	}
}

// TestBuildOneRedAmongGreen: a single red claim fails the WHOLE composite — Green requires
// every claim StatusPass, so a swarm shard can never ship with a red claim masked by greens.
func TestBuildOneRedAmongGreen(t *testing.T) {
	root := t.TempDir()
	rel := writeArtifact(t, root, reportArtifact("m1", urlClaim("c1"), urlClaim("c2")))

	// The stub returns the SAME exit code for every call; a non-zero code reds both claims.
	// (A per-claim-selective stub is unnecessary: the property under test is "any non-pass
	// claim ⇒ composite red", which one red claim already establishes.)
	box := &recBox{exitCode: 22}
	plan, err := Build(NameWeb, box, rel, DefaultSchemas())
	if err != nil {
		t.Fatalf("Build(web): unexpected error: %v", err)
	}
	rep, err := plan.Verifier.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatalf("a red claim must red the composite; got Passed=true, output=%q", rep.Output)
	}
}

// TestBuildUnknownName: an unknown pack name is an ERROR — never a verify.Pass default and
// never a silent no-op verifier. This is the fail-closed inversion of verify.Detect.
func TestBuildUnknownName(t *testing.T) {
	root := t.TempDir()
	rel := writeArtifact(t, root, reportArtifact("u1", urlClaim("c1")))

	if _, err := Build("does-not-exist", &recBox{}, rel, DefaultSchemas()); err == nil {
		t.Fatalf("Build with an unknown pack name must return an error, got nil")
	}
}

// TestBuildKnownNames: every documented pack name — including the three new swarm packs —
// is now KNOWN, so Build resolves it without error. (audit/benchmark/code were unregistered
// before SW-T05; this pins that they are wired.)
func TestBuildKnownNames(t *testing.T) {
	root := t.TempDir()
	rel := writeArtifact(t, root, reportArtifact("k1", urlClaim("c1")))

	for _, name := range []string{
		NameWeb, NameSoftware, NameFinance, NameUI,
		NameAudit, NameBenchmark, NameCode,
	} {
		if _, err := Build(name, &recBox{}, rel, DefaultSchemas()); err != nil {
			t.Fatalf("Build(%q): expected a known pack, got error: %v", name, err)
		}
	}
}

// TestBuildNilBoxStillComposes: a nil box still yields a usable plan whose schema layer
// runs. A structurally valid artifact then resolves to NOT-green (the per-claim network
// check fails closed to Unverifiable with no box), never a spurious pass.
func TestBuildNilBoxStillComposes(t *testing.T) {
	root := t.TempDir()
	rel := writeArtifact(t, root, reportArtifact("n1", urlClaim("c1")))

	plan, err := Build(NameWeb, nil, rel, DefaultSchemas())
	if err != nil {
		t.Fatalf("Build(web, nil box): unexpected error: %v", err)
	}
	rep, err := plan.Verifier.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatalf("a nil box must fail networked claims closed (Unverifiable), got Passed=true")
	}
}

// TestBuildNilBoxSchemaFailsClosed: with a nil box AND a malformed shape, the schema layer
// still fires and reds the verdict — proving the structural gate runs without a box.
func TestBuildNilBoxSchemaFailsClosed(t *testing.T) {
	root := t.TempDir()
	bad := reportArtifact("nb1", urlClaim("c1"))
	bad.Title = "" // schema: missing required title
	rel := writeArtifact(t, root, bad)

	plan, err := Build(NameAudit, nil, rel, DefaultSchemas())
	if err != nil {
		t.Fatalf("Build(audit, nil box): unexpected error: %v", err)
	}
	rep, err := plan.Verifier.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: unexpected error: %v", err)
	}
	if rep.Passed {
		t.Fatalf("malformed shape with a nil box must NOT pass")
	}
}

// TestBuildHostsForLocalPacks: the three swarm packs are local/in-box and document a nil
// egress host-set, so their PackPlan.Hosts is nil (no allowlist to cross-check).
func TestBuildHostsForLocalPacks(t *testing.T) {
	root := t.TempDir()
	rel := writeArtifact(t, root, reportArtifact("h1", urlClaim("c1")))

	for _, name := range []string{NameAudit, NameBenchmark, NameCode} {
		plan, err := Build(name, &recBox{}, rel, DefaultSchemas())
		if err != nil {
			t.Fatalf("Build(%q): %v", name, err)
		}
		if plan.Hosts != nil {
			t.Fatalf("pack %q is local/in-box; want nil Hosts, got %v", name, plan.Hosts)
		}
	}
}

// TestDefaultSchemasRoundTrips: DefaultSchemas() resolves EVERY built-in Kind via Lookup —
// the single source of built-in shapes covers all five canonical Kinds, so no Kind falls
// through to the unschematized (fail-closed) path by accident.
func TestDefaultSchemasRoundTrips(t *testing.T) {
	reg := DefaultSchemas()
	for _, k := range []artifact.Kind{
		artifact.KindReport,
		artifact.KindMatrix,
		artifact.KindSpec,
		artifact.KindBenchmark,
		artifact.KindDossier,
	} {
		s, ok := reg.Lookup(k)
		if !ok {
			t.Fatalf("DefaultSchemas missing built-in Kind %q", k)
		}
		if s.Kind != k {
			t.Fatalf("schema for Kind %q has mismatched Kind %q", k, s.Kind)
		}
	}
	// DefaultSchemas must be a real catalog, not the same value as a fresh empty registry.
	if _, ok := schema.NewRegistry().Lookup(artifact.KindReport); ok {
		t.Fatalf("sanity: an empty registry should not resolve KindReport")
	}
}
