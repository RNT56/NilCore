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

// TestUndetectableBuildIsUnverifiable pins the fix: an unrecognized worktree layout (where
// verify.Detect yields the no-op "true") must NOT green a typed build claim. Running "true"
// always exits 0 — a Pass with ZERO checking (I2). The pack inverts Detect's permissiveness:
// an undetectable build is Unverifiable, reached BEFORE any box call (no no-op is run).
func TestUndetectableBuildIsUnverifiable(t *testing.T) {
	dir := t.TempDir() // no markers ⇒ verify.Detect == "true"
	if got := verify.Detect(dir); got != "true" {
		t.Fatalf("verify.Detect over an empty dir = %q, want \"true\" (test setup invalid)", got)
	}
	box := &fakeBox{workdir: dir, exec: exit(0, "")}
	status, msg := checkBuildPasses(context.Background(), box, claim(IDBuildPasses, "", ""))
	if status != artifact.StatusUnverifiable {
		t.Fatalf("status = %q, want Unverifiable for an undetectable build layout (never a no-op green)", status)
	}
	if len(box.calls) != 0 {
		t.Fatalf("an undetectable build must not run the no-op 'true' in the box; calls = %v", box.calls)
	}
	if !strings.Contains(msg, "no build system") {
		t.Fatalf("detail = %q, want a 'no build system detected' reason", msg)
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
			// EMPTY selector ⇒ the WHOLE suite. For go this MUST recurse ("./..."):
			// a bare `go test` tests only the current dir and would green with the
			// suite never running when tests live in subpackages (the I2 laundering
			// vector). See TestGoEmptySelectorRunsWholeSuiteRecursively below.
			name: "go-whole-suite-pass", extract: "go", value: "",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusPass,
			wantCmd: "go test ./...",
		},
		{
			name: "npm-selector-pass", extract: "npm", value: "unit",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusPass,
			wantCmd: "npm test -- 'unit'",
		},
		{
			name: "pytest-fail", extract: "pytest", value: "tests/test_x.py",
			box: &fakeBox{exec: exit(1, "1 failed")}, want: artifact.StatusFail,
			wantCmd: "pytest -- 'tests/test_x.py'",
		},
		{
			name: "sandbox-error-unverifiable", extract: "go", value: "",
			box: &fakeBox{exec: func(string) (sandbox.Result, error) {
				return sandbox.Result{}, errors.New("box exploded")
			}}, want: artifact.StatusUnverifiable, wantCmd: "go test ./...",
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
		// A leading-dash selector is the I2 laundering vector: "go test -run=^$" selects
		// zero tests, exits 0, and would forge a green verdict. validateSelector must
		// reject it BEFORE any box.Exec — the fake box fails the test if Exec is reached.
		{
			name: "selector-run-empty-unverifiable-no-box", extract: "go", value: "-run=^$",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusUnverifiable, wantNoBox: true,
		},
		{
			name: "selector-count-zero-unverifiable-no-box", extract: "go", value: "-count=0",
			box: &fakeBox{exec: exit(0, "")}, want: artifact.StatusUnverifiable, wantNoBox: true,
		},
		{
			name: "selector-list-flag-unverifiable-no-box", extract: "go", value: "--list",
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

// TestTestCommandIsSingleQuoted asserts a benign selector is single-quoted into each
// runner's fixed shape so it stays DATA (the command-injection boundary). For go the
// selector is a BARE positional package path (no "--", which would skip the package); for
// npm/pytest it sits after a literal "--" so it can never be read as a runner flag.
func TestTestCommandIsSingleQuoted(t *testing.T) {
	// go: bare single-quoted package path, NO "--" (a "--" would forge a green — I2).
	goCmd, err := buildTestCommand(claim(IDTestPasses, "go", "./internal/foo"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if goCmd != "go test './internal/foo'" {
		t.Fatalf("cmd = %q, want the go selector single-quoted as a bare package path", goCmd)
	}
	if strings.Contains(goCmd, " -- ") {
		t.Fatalf("cmd = %q, the go runner must NOT insert a '--' (post-'--' args skip the selected package — I2)", goCmd)
	}
	// npm/pytest: single-quoted selector behind a literal "--".
	npmCmd, err := buildTestCommand(claim(IDTestPasses, "npm", "unit"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if npmCmd != "npm test -- 'unit'" {
		t.Fatalf("cmd = %q, want the npm selector single-quoted behind '--'", npmCmd)
	}
}

// TestGoRunnerSelectorIsPackagePath documents (and pins) the corrected contract for the go
// runner: the selector is emitted as a BARE positional argument (never after "--"), so for
// `go test` it selects a PACKAGE PATH, never a `-run` test-name filter. `go test -- <x>`
// would hand <x> to the current-dir test binary and skip the selected package — the false
// green this fix removes.
func TestGoRunnerSelectorIsPackagePath(t *testing.T) {
	pkgCmd, err := buildTestCommand(claim(IDTestPasses, "go", "./internal/foo"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pkgCmd != "go test './internal/foo'" {
		t.Fatalf("cmd = %q, want a bare positional package path (no '--')", pkgCmd)
	}
	if strings.Contains(pkgCmd, "--") {
		t.Fatalf("cmd = %q, the go runner must never emit a '--' (post-'--' args skip the selected package — I2)", pkgCmd)
	}
	// A test-name-looking value is emitted positionally too — it can NEVER become
	// `go test -run '...'` (the leading-dash rule forbids ever producing a flag form).
	nameCmd, err := buildTestCommand(claim(IDTestPasses, "go", "TestFoo"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(nameCmd, "-run") {
		t.Fatalf("cmd = %q, the go runner must never emit a -run flag for a selector", nameCmd)
	}
}

// TestFailingSelectedSubpackageIsFailNotPass is the discriminating end-to-end proof of the
// `--` fix. The stub models a red test in the SELECTED sub-package: the fixed command
// `go test './sub'` exits 1 (Fail), while the PRE-FIX buggy `go test -- './sub'` (which runs
// only the current-dir package) would have exited 0 and forged a green (I2). Because the
// fixed pack emits the bare package-path form, the claim REDDENS.
func TestFailingSelectedSubpackageIsFailNotPass(t *testing.T) {
	box := &fakeBox{exec: func(cmd string) (sandbox.Result, error) {
		if cmd == "go test './sub'" {
			return sandbox.Result{ExitCode: 1, Stderr: "--- FAIL: TestSub"}, nil
		}
		// The pre-fix post-'--' form ran only the current dir (which passes) — a spurious green.
		return sandbox.Result{ExitCode: 0, Stderr: "ok (current dir only)"}, nil
	}}
	status, _ := checkTestPasses(context.Background(), box, claim(IDTestPasses, "go", "./sub"))
	if status != artifact.StatusFail {
		t.Fatalf("status = %q, want Fail — a failing SELECTED sub-package must redden the claim (I2)", status)
	}
	if len(box.calls) != 1 || box.calls[0] != "go test './sub'" {
		t.Fatalf("ran %v, want exactly the bare package-path form (no '--' prefix)", box.calls)
	}
}

// TestGoNoPackagesIsUnverifiableNotFail pins the secondary false-RED fix: `go test ./...` in
// a worktree with no Go packages exits 1 with "matched no packages" — nothing was tested, so
// the verdict must be Unverifiable (no decisive result), never a false Fail. The marker never
// appears in a genuine `--- FAIL:` failure, so this can only downgrade, never mask a red.
func TestGoNoPackagesIsUnverifiableNotFail(t *testing.T) {
	box := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 1, Stderr: "go: warning: \"./...\" matched no packages\nno packages to test"}, nil
	}}
	status, _ := checkTestPasses(context.Background(), box, claim(IDTestPasses, "go", ""))
	if status != artifact.StatusUnverifiable {
		t.Fatalf("status = %q, want Unverifiable (no packages to test is not a decisive fail)", status)
	}
	// And a REAL failure with no such marker still reds (guard against over-broad downgrading).
	box2 := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 1, Stderr: "--- FAIL: TestReal"}, nil
	}}
	if status2, _ := checkTestPasses(context.Background(), box2, claim(IDTestPasses, "go", "")); status2 != artifact.StatusFail {
		t.Fatalf("status = %q, want Fail for a genuine test failure (downgrade must not mask a real red)", status2)
	}
}

// TestGoEmptySelectorRunsWholeSuiteRecursively pins the fix for the empty-selector
// laundering vector: an empty go selector MUST recurse over the whole module ("./..."),
// never emit a bare `go test`. A bare `go test` tests only the current directory's
// package; in a worktree whose tests live in subpackages it compiles/runs nothing,
// prints "[no test files]", and exits 0 — a green with the suite never running (I2).
func TestGoEmptySelectorRunsWholeSuiteRecursively(t *testing.T) {
	cmd, err := buildTestCommand(claim(IDTestPasses, "go", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "go test ./..." {
		t.Fatalf("cmd = %q, want the recursive whole-suite form %q (a bare 'go test' launders a green)", cmd, "go test ./...")
	}
	// Guard the exact laundering shape explicitly: the empty selector must NEVER be the
	// current-dir-only bare command.
	if cmd == "go test" {
		t.Fatalf("cmd = %q — a bare 'go test' tests only the current dir and launders a green (I2)", cmd)
	}
}

// TestEmptySelectorWholeSuiteFormsPerRunner documents the whole-suite command each
// allowlisted runner emits for an empty selector. Each MUST exercise the whole module,
// never a no-op that could exit 0 without running the suite.
func TestEmptySelectorWholeSuiteFormsPerRunner(t *testing.T) {
	want := map[string]string{
		"go":     "go test ./...",
		"npm":    "npm test",
		"pytest": "pytest",
	}
	for token, wantCmd := range want {
		cmd, err := buildTestCommand(claim(IDTestPasses, token, ""))
		if err != nil {
			t.Fatalf("runner %q empty selector: unexpected error: %v", token, err)
		}
		if cmd != wantCmd {
			t.Fatalf("runner %q empty selector: cmd = %q, want %q (whole-suite form)", token, cmd, wantCmd)
		}
	}
}

// TestFailingSubpackageReddensEmptyGoSelector is the discriminating end-to-end proof of
// the fix. The stub box returns a failing exit for the recursive whole-suite command
// ("go test ./...") — modeling a red test in a SUBPACKAGE — but exit 0 for a bare
// `go test` (current-dir only, "[no test files]"). Because the fixed pack emits the
// recursive form, the claim now REDDENS (Fail); the pre-fix bare command would have run
// the current dir, found no tests, exited 0, and forged a green (StatusPass).
func TestFailingSubpackageReddensEmptyGoSelector(t *testing.T) {
	box := &fakeBox{exec: func(cmd string) (sandbox.Result, error) {
		if cmd == "go test ./..." {
			// A subpackage test fails: the whole suite is red.
			return sandbox.Result{ExitCode: 1, Stderr: "--- FAIL: TestSub"}, nil
		}
		// The pre-fix bare `go test`: current dir has no tests ⇒ a spurious green.
		return sandbox.Result{ExitCode: 0, Stderr: "?   x  [no test files]"}, nil
	}}
	status, _ := checkTestPasses(context.Background(), box, claim(IDTestPasses, "go", ""))
	if status != artifact.StatusFail {
		t.Fatalf("status = %q, want Fail — a failing subpackage test must redden the whole-suite claim (I2)", status)
	}
	if len(box.calls) != 1 || box.calls[0] != "go test ./..." {
		t.Fatalf("ran %v, want exactly the recursive whole-suite command 'go test ./...'", box.calls)
	}
}

// TestUnverifiableRunnerWithNoWholeSuiteForm proves the fail-closed branch: if a runner
// is registered with no whole-suite form, an empty selector must yield Unverifiable (an
// error before any box call), never a bare Pass — so a no-op can never launder a green.
// It exercises the branch directly via a temporary registry entry so the guard is
// covered even while every shipped runner defines a whole-suite form.
func TestUnverifiableRunnerWithNoWholeSuiteForm(t *testing.T) {
	const token = "noWholeSuite"
	testRunners[token] = testRunner{selected: "fake test %s"} // wholeSuite left empty
	defer delete(testRunners, token)

	if _, err := buildTestCommand(claim(IDTestPasses, token, "")); err == nil {
		t.Fatalf("empty selector on a runner with no whole-suite form = nil error, want an error (fail closed)")
	}
	// End-to-end: the check must be Unverifiable and reach no box.
	box := &fakeBox{exec: exit(0, "")}
	status, _ := checkTestPasses(context.Background(), box, claim(IDTestPasses, token, ""))
	if status != artifact.StatusUnverifiable {
		t.Fatalf("status = %q, want Unverifiable for an empty selector with no whole-suite form", status)
	}
	if len(box.calls) != 0 {
		t.Fatalf("expected no box call (fail-closed before exec), got %v", box.calls)
	}
	// A NON-empty selector on the same runner still works (only the empty case is gated).
	if _, err := buildTestCommand(claim(IDTestPasses, token, "pkg")); err != nil {
		t.Fatalf("non-empty selector on the same runner: unexpected error %v", err)
	}
}

// TestValidateSelectorRejectsLeadingDash is the discriminating guard for the I2
// laundering vector: a selector that begins with '-' (e.g. "-run=^$", which selects zero
// tests, exits 0, and forges a green verdict) MUST be rejected by validateSelector — and
// a benign selector with the same flag-name shape but no leading dash MUST pass, so the
// test proves it is the leading dash (not the substring) that is caught.
func TestValidateSelectorRejectsLeadingDash(t *testing.T) {
	reject := []string{"-run=^$", "-count=0", "--list", "-v", "  -run=Foo"}
	for _, sel := range reject {
		if err := validateSelector(sel); err == nil {
			t.Fatalf("validateSelector(%q) = nil, want an error for a leading-dash selector", sel)
		}
	}
	// Control side: an inner dash, or a flag-looking name without the leading dash, is
	// fine — the rule must discriminate on POSITION, not merely contain "-".
	accept := []string{"TestRun", "run=Foo", "./pkg/-internal", "Test-Name"}
	for _, sel := range accept {
		if err := validateSelector(sel); err != nil {
			t.Fatalf("validateSelector(%q) = %v, want nil (a non-leading dash is allowed)", sel, err)
		}
	}
}

// Compile-time guard mirrored as a runtime no-op so the test file also fails loudly if
// the CheckFunc signature drifts.
var (
	_ evverify.CheckFunc = checkBuildPasses
	_ evverify.CheckFunc = checkTestPasses
)
