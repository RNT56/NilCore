package code

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// fakeBox is a STUB sandbox: it records the exact command strings it is asked to run
// and returns a canned Result (or error) so the tests are hermetic — no real go/npm/
// pytest, no network. Workdir is configurable so a test can point verify.Detect at a
// real temp dir holding a marker file.
type fakeBox struct {
	workdir string
	calls   []string
	exec    func(cmd string) (sandbox.Result, error)
}

func (b *fakeBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.calls = append(b.calls, cmd)
	if b.exec != nil {
		return b.exec(cmd)
	}
	return sandbox.Result{}, nil
}

func (b *fakeBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}

func (b *fakeBox) Workdir() string { return b.workdir }

// exit makes a stub that returns a fixed exit code (+ optional stderr) for every Exec.
func exit(code int, stderr string) func(string) (sandbox.Result, error) {
	return func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: code, Stderr: stderr}, nil
	}
}

// claim builds a Claim carrying the model-authored fields a check reads.
func claim(verifier, extraction, value string) artifact.Claim {
	return artifact.Claim{
		ID:    "c1",
		Field: "build",
		Evidence: artifact.Evidence{
			Verifier:         verifier,
			ExtractionMethod: extraction,
			Value:            value,
		},
	}
}

// --- RegisterAll / Hosts ------------------------------------------------------

func TestRegisterAllRegistersBothIDs(t *testing.T) {
	r := evverify.New()
	RegisterAll(r)
	for _, id := range []string{IDBuildPasses, IDTestPasses} {
		if _, ok := r.Lookup(id); !ok {
			t.Fatalf("expected %q registered after RegisterAll", id)
		}
	}
	// Fail-closed: an id this pack does not own stays unregistered.
	if _, ok := r.Lookup("code.does_not_exist"); ok {
		t.Fatalf("unexpected registration of an unknown id")
	}
}

func TestHostsIsNil(t *testing.T) {
	if h := Hosts(); h != nil {
		t.Fatalf("Hosts() = %v, want nil (the pack reaches no remote host)", h)
	}
}

// --- verify.Detect REUSE ------------------------------------------------------

// TestDetectIsReusedForGoModule proves the pack does NOT re-implement detection: a temp
// worktree containing go.mod must yield the go build/test command verify.Detect emits,
// and checkBuildPasses must run exactly that command in the box.
func TestDetectIsReusedForGoModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	want := verify.Detect(dir)
	if !strings.HasPrefix(want, "go ") {
		t.Fatalf("verify.Detect over a go.mod dir = %q, expected a go command (test setup invalid)", want)
	}

	box := &fakeBox{workdir: dir, exec: exit(0, "")}
	status, _ := checkBuildPasses(context.Background(), box, claim(IDBuildPasses, "", ""))
	if status != artifact.StatusPass {
		t.Fatalf("status = %q, want pass", status)
	}
	if len(box.calls) != 1 {
		t.Fatalf("expected exactly one box.Exec, got %d: %v", len(box.calls), box.calls)
	}
	if box.calls[0] != want {
		t.Fatalf("ran %q, want the verify.Detect command %q (Detect must be REUSED)", box.calls[0], want)
	}
}

// TestDetectFallbackUnknownLayout shows an undetectable worktree falls through to
// verify.Detect's safe no-op "true" rather than a spurious red — the pack inherits the
// ladder's conservatism.
func TestDetectFallbackUnknownLayout(t *testing.T) {
	dir := t.TempDir() // no markers
	box := &fakeBox{workdir: dir, exec: exit(0, "")}
	if _, err := checkBuildPasses(context.Background(), box, claim(IDBuildPasses, "", "")); false {
		_ = err
	}
	if got := verify.Detect(dir); got != "true" {
		t.Fatalf("verify.Detect over an empty dir = %q, want \"true\"", got)
	}
	if len(box.calls) != 1 || box.calls[0] != "true" {
		t.Fatalf("expected the no-op 'true' to run, got %v", box.calls)
	}
}

// --- checkBuildPasses verdicts ------------------------------------------------

func TestCheckBuildPassesVerdicts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	tests := []struct {
		name string
		box  *fakeBox
		want artifact.Status
	}{
		{"exit-0-pass", &fakeBox{workdir: dir, exec: exit(0, "")}, artifact.StatusPass},
		{"non-zero-fail", &fakeBox{workdir: dir, exec: exit(2, "build failed")}, artifact.StatusFail},
		{"sandbox-error-unverifiable", &fakeBox{workdir: dir, exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("box exploded")
		}}, artifact.StatusUnverifiable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _ := checkBuildPasses(context.Background(), tt.box, claim(IDBuildPasses, "", ""))
			if status != tt.want {
				t.Fatalf("status = %q, want %q", status, tt.want)
			}
		})
	}
}

func TestCheckBuildPassesNilBox(t *testing.T) {
	status, msg := checkBuildPasses(context.Background(), nil, claim(IDBuildPasses, "", ""))
	if status != artifact.StatusUnverifiable {
		t.Fatalf("nil box: status = %q, want unverifiable", status)
	}
	if !strings.Contains(msg, "no sandbox") {
		t.Fatalf("nil box: detail = %q, want a 'no sandbox' note", msg)
	}
}

// --- build command override (allowlist) ---------------------------------------

func TestCheckBuildPassesAllowlistedOverride(t *testing.T) {
	dir := t.TempDir() // no markers ⇒ Detect would be "true"; override must win
	box := &fakeBox{workdir: dir, exec: exit(0, "")}
	status, _ := checkBuildPasses(context.Background(), box, claim(IDBuildPasses, "make verify", ""))
	if status != artifact.StatusPass {
		t.Fatalf("status = %q, want pass", status)
	}
	if len(box.calls) != 1 || box.calls[0] != "make verify" {
		t.Fatalf("ran %v, want the allowlisted override 'make verify'", box.calls)
	}
}

func TestCheckBuildPassesUnallowlistedOverrideFailsClosed(t *testing.T) {
	dir := t.TempDir()
	box := &fakeBox{workdir: dir, exec: exit(0, "")}
	// A free command (even a benign-looking one) is rejected before any box call.
	status, _ := checkBuildPasses(context.Background(), box, claim(IDBuildPasses, "rm -rf /", ""))
	if status != artifact.StatusUnverifiable {
		t.Fatalf("status = %q, want unverifiable for an unallowlisted command", status)
	}
	if len(box.calls) != 0 {
		t.Fatalf("an unallowlisted override must not reach the box; calls = %v", box.calls)
	}
}

// --- checkTestPasses ----------------------------------------------------------

func TestCheckTestPassesVerdicts(t *testing.T) {
	tests := []struct {
		name      string
		extract   string
		value     string
		box       *fakeBox
		want      artifact.Status
		wantCmd   string // expected command (empty ⇒ unchecked / no box call)
		wantNoBox bool
	}{
		{
			name: "go-selector-pass", extract: "go", value: "./internal/foo",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusPass,
			wantCmd: "go test './internal/foo'",
		},
		{
			name: "go-bare-suite-pass", extract: "go", value: "",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusPass,
			wantCmd: "go test",
		},
		{
			name: "npm-selector-pass", extract: "npm", value: "unit",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusPass,
			wantCmd: "npm test 'unit'",
		},
		{
			name: "pytest-fail", extract: "pytest", value: "tests/test_x.py",
			box: &fakeBox{exec: exit(1, "1 failed")}, want: artifact.StatusFail,
			wantCmd: "pytest 'tests/test_x.py'",
		},
		{
			name: "sandbox-error-unverifiable", extract: "go", value: "",
			box: &fakeBox{exec: func(string) (sandbox.Result, error) {
				return sandbox.Result{}, errors.New("box exploded")
			}}, want: artifact.StatusUnverifiable, wantCmd: "go test",
		},
		{
			name: "unknown-runner-unverifiable-no-box", extract: "rake", value: "x",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusUnverifiable, wantNoBox: true,
		},
		{
			name: "missing-runner-unverifiable-no-box", extract: "", value: "x",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusUnverifiable, wantNoBox: true,
		},
		{
			name: "selector-with-quote-unverifiable-no-box", extract: "go", value: "x'; rm -rf /",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusUnverifiable, wantNoBox: true,
		},
		{
			name: "selector-with-space-unverifiable-no-box", extract: "go", value: "a b",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusUnverifiable, wantNoBox: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _ := checkTestPasses(context.Background(), tt.box, claim(IDTestPasses, tt.extract, tt.value))
			if status != tt.want {
				t.Fatalf("status = %q, want %q", status, tt.want)
			}
			if tt.wantNoBox {
				if len(tt.box.calls) != 0 {
					t.Fatalf("expected no box call (fail-closed before exec), got %v", tt.box.calls)
				}
				return
			}
			if len(tt.box.calls) != 1 {
				t.Fatalf("expected exactly one box.Exec, got %d: %v", len(tt.box.calls), tt.box.calls)
			}
			if tt.wantCmd != "" && tt.box.calls[0] != tt.wantCmd {
				t.Fatalf("ran %q, want %q", tt.box.calls[0], tt.wantCmd)
			}
		})
	}
}

func TestCheckTestPassesNilBox(t *testing.T) {
	status, msg := checkTestPasses(context.Background(), nil, claim(IDTestPasses, "go", ""))
	if status != artifact.StatusUnverifiable {
		t.Fatalf("nil box: status = %q, want unverifiable", status)
	}
	if !strings.Contains(msg, "no sandbox") {
		t.Fatalf("nil box: detail = %q, want a 'no sandbox' note", msg)
	}
}

// --- detail tail does not echo raw output (I7) --------------------------------

// TestDetailBounded confirms the harness-authored detail tail is length-bounded, so a
// verifier note can never flood the artifact JSON or an event Detail (I7 hygiene).
func TestDetailBounded(t *testing.T) {
	long := strings.Repeat("x", maxDetail*2)
	if got := detail(long); len(got) > maxDetail {
		t.Fatalf("detail not bounded: len=%d, want <= %d", len(got), maxDetail)
	}
}

// --- single-quoting (I7) ------------------------------------------------------

// TestTestCommandIsSingleQuoted asserts a benign selector is single-quoted into the
// fixed shape, so it stays DATA (the command-injection boundary).
func TestTestCommandIsSingleQuoted(t *testing.T) {
	cmd, err := buildTestCommand(claim(IDTestPasses, "go", "TestFoo"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "go test 'TestFoo'" {
		t.Fatalf("cmd = %q, want the selector single-quoted into the go shape", cmd)
	}
}

// Compile-time guard mirrored as a runtime no-op so the test file also fails loudly if
// the CheckFunc signature drifts.
var (
	_ evverify.CheckFunc = checkBuildPasses
	_ evverify.CheckFunc = checkTestPasses
)
