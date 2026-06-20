package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// fakeVerifierBox is a hermetic sandbox.Sandbox stand-in for the wiring tests. Its
// Workdir() points at a real temp worktree (so artifactFiles can discover the files
// the test wrote), and Exec returns a canned exit code so web.url_resolves resolves
// to Pass (exit 0 ⇒ HTTP 2xx) or Unverifiable (non-zero) deterministically with NO
// network. It never reaches the host network — the whole point of I4 under test.
type fakeVerifierBox struct {
	dir      string
	exit     int
	envSeen  map[string]string // last env passed to ExecWithEnv (secret-leak assertions)
	cmdsSeen []string
}

func (b *fakeVerifierBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.cmdsSeen = append(b.cmdsSeen, cmd)
	return sandbox.Result{ExitCode: b.exit}, nil
}

func (b *fakeVerifierBox) ExecWithEnv(_ context.Context, cmd string, env map[string]string) (sandbox.Result, error) {
	b.cmdsSeen = append(b.cmdsSeen, cmd)
	b.envSeen = env
	return sandbox.Result{ExitCode: b.exit}, nil
}

func (b *fakeVerifierBox) Workdir() string { return b.dir }

// writeURLArtifact writes an artifact whose single claim uses the generic
// web.url_resolves verifier (the only id evverify.Default registers), so the wired
// verifier exercises the real default registry path. Returns the artifact id.
func writeURLArtifact(t *testing.T, root, id, sourceURL string) string {
	t.Helper()
	a := &artifact.Artifact{
		ID:        id,
		Kind:      artifact.KindReport,
		Title:     "wiring",
		CreatedAt: time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		Claims: []artifact.Claim{{
			ID:    id + "-c1",
			Field: "f1",
			Evidence: artifact.Evidence{
				Value:     "v1",
				SourceURL: sourceURL,
				Verifier:  "web.url_resolves",
				Status:    artifact.StatusPass, // self-written; the verifier must overwrite it
			},
		}},
	}
	if err := artifact.Write(root, a); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return id
}

func readReport(t *testing.T, v verify.Verifier) verify.Report {
	t.Helper()
	rep, err := v.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return rep
}

// TestEvidenceVerifierWiring is the P11-T05 gate: behavioralVerifier wires an
// evverify.ArtifactVerifier behind NILCORE_EVIDENCE_VERIFY, after the build verifier,
// only when an artifact file is present; unset is byte-identical.
func TestEvidenceVerifierWiring(t *testing.T) {
	t.Run("env unset => bare verify.New (byte-identical)", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}
		// An artifact is present, but with the flag unset it must be IGNORED: the
		// returned verifier must be exactly the bare project verifier, not a Composite.
		writeURLArtifact(t, box.dir, "rep", "https://example.com")

		v := behavioralVerifier(box, "true")
		if _, ok := v.(verify.Composite); ok {
			t.Fatalf("flag unset must return the bare verifier, got a Composite")
		}
		// Byte-identical means structurally the SAME verifier today's code returns:
		// a *CommandVerifier wrapping the same box+command, never a Composite.
		got, ok := v.(*verify.CommandVerifier)
		if !ok {
			t.Fatalf("flag unset must return *verify.CommandVerifier, got %T", v)
		}
		want := verify.New(box, "true")
		if *got != *want {
			t.Fatalf("flag unset must return exactly verify.New, got %#v want %#v", *got, *want)
		}
	})

	t.Run("set + artifact present + pass => build first, evidence appended, green", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0} // exit 0 ⇒ url_resolves Pass
		writeURLArtifact(t, box.dir, "rep", "https://example.com")

		v := behavioralVerifier(box, "true")
		comp, ok := v.(verify.Composite)
		if !ok {
			t.Fatalf("flag set + artifact present must return a Composite, got %#v", v)
		}
		if len(comp.Named) < 2 {
			t.Fatalf("Composite must have build + evidence, got %d verifiers", len(comp.Named))
		}
		if comp.Named[0].Name != "checks" {
			t.Fatalf("Named[0] must be the build verifier, got %q", comp.Named[0].Name)
		}
		if !strings.HasPrefix(comp.Named[1].Name, "evidence") {
			t.Fatalf("evidence verifier must be appended after the build verifier, got %q", comp.Named[1].Name)
		}
		if rep := readReport(t, v); !rep.Passed {
			t.Fatalf("all-pass artifact + green build must be green, got: %s", rep.Output)
		}
	})

	t.Run("set + artifact present + red claim => Composite red", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 22} // non-2xx ⇒ Unverifiable
		writeURLArtifact(t, box.dir, "rep", "https://example.com")

		v := behavioralVerifier(box, "true")
		if rep := readReport(t, v); rep.Passed {
			t.Fatalf("a non-pass claim must redden the whole verdict; got Passed=true: %s", rep.Output)
		}
	})

	t.Run("set + NO artifact => evidence omitted, green build greens", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0} // empty worktree, no artifacts

		v := behavioralVerifier(box, "true")
		if _, ok := v.(verify.Composite); ok {
			t.Fatalf("no artifact present must omit the evidence verifier (bare verifier), got a Composite")
		}
		if rep := readReport(t, v); !rep.Passed {
			t.Fatalf("green build with no artifact must stay green, got: %s", rep.Output)
		}
	})
}

// TestEvidenceVerifierEvents asserts the additive artifact_verify/claim_verify event
// kinds are appended through the eventlog ONLY when the flag is on and a log is
// supplied — and never when it is off (I5: new append-only kinds, gated).
func TestEvidenceVerifierEvents(t *testing.T) {
	newLog := func(t *testing.T) (*eventlog.Log, string) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "events.jsonl")
		log, err := eventlog.Open(p)
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		return log, p
	}

	t.Run("flag on + log => emits artifact_verify and claim_verify", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}
		writeURLArtifact(t, box.dir, "rep", "https://example.com")
		log, path := newLog(t)

		v := behavioralVerifierWithLog(box, "true", log)
		_ = readReport(t, v)

		body := readFile(t, path)
		for _, kind := range []string{"artifact_verify", "claim_verify"} {
			if !strings.Contains(body, kind) {
				t.Fatalf("expected %q event appended, log was:\n%s", kind, body)
			}
		}
	})

	t.Run("flag off => no evidence events", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}
		writeURLArtifact(t, box.dir, "rep", "https://example.com")
		log, path := newLog(t)

		v := behavioralVerifierWithLog(box, "true", log)
		_ = readReport(t, v)

		body := readFile(t, path)
		for _, kind := range []string{"artifact_verify", "claim_verify"} {
			if strings.Contains(body, kind) {
				t.Fatalf("flag off must emit no %q event, log was:\n%s", kind, body)
			}
		}
	})
}

// TestEvidenceVerifierNoSecretLeak asserts the wired evidence path never writes a
// secret into the persisted artifact JSON or an emitted event Detail (I3). The
// SourceURL stays key-free and the event copies only key-free, harness-trusted
// fields; the secret lives only in the box-injected env, never the command or the
// persisted/logged surface.
func TestEvidenceVerifierNoSecretLeak(t *testing.T) {
	const secret = "SUPER-SECRET-TOKEN-XYZ"
	t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
	t.Setenv("NILCORE_BROWSER_VERIFY", "")
	t.Setenv("NILCORE_TEST_SECRET", secret)

	box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}
	id := writeURLArtifact(t, box.dir, "rep", "https://example.com")

	p := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(p)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	v := behavioralVerifierWithLog(box, "true", log)
	_ = readReport(t, v)

	artJSON := readFile(t, filepath.Join(box.dir, ".nilcore", "artifacts", id+".json"))
	if strings.Contains(artJSON, secret) {
		t.Fatalf("secret leaked into the persisted artifact JSON:\n%s", artJSON)
	}
	if body := readFile(t, p); strings.Contains(body, secret) {
		t.Fatalf("secret leaked into an event Detail:\n%s", body)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
