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
	"nilcore/internal/verify/vcache"
)

// fakeVerifierBox is a hermetic sandbox.Sandbox stand-in for the wiring tests. Its
// Workdir() points at a real temp worktree (so artifactFiles can discover the files
// the test wrote), and Exec returns a canned exit code so web.url_resolves resolves
// to Pass (exit 0 ⇒ HTTP 2xx) or Unverifiable (non-zero) deterministically with NO
// network. It never reaches the host network — the whole point of I4 under test.
type fakeVerifierBox struct {
	dir      string
	exit     int
	stdout   string            // returned as Result.Stdout (e.g. a fetched body for web.quote_exists)
	envSeen  map[string]string // last env passed to ExecWithEnv (secret-leak assertions)
	cmdsSeen []string
}

func (b *fakeVerifierBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.cmdsSeen = append(b.cmdsSeen, cmd)
	return sandbox.Result{ExitCode: b.exit, Stdout: b.stdout}, nil
}

func (b *fakeVerifierBox) ExecWithEnv(_ context.Context, cmd string, env map[string]string) (sandbox.Result, error) {
	b.cmdsSeen = append(b.cmdsSeen, cmd)
	b.envSeen = env
	return sandbox.Result{ExitCode: b.exit, Stdout: b.stdout}, nil
}

func (b *fakeVerifierBox) Workdir() string { return b.dir }

// setVerifyFlags pins all three decorator escape hatches for a subtest so no
// ambient env leaks into the chain-shape assertions.
func setVerifyFlags(t *testing.T, vcacheV, flakeV, tieredV string) {
	t.Helper()
	t.Setenv("NILCORE_VCACHE", vcacheV)
	t.Setenv("NILCORE_FLAKEPROBE", flakeV)
	t.Setenv("NILCORE_TIERED_VERIFY", tieredV)
}

// TestVerifyFlagEnabled pins the default-on parse idiom (mirrors NILCORE_KERNEL):
// unset/anything ⇒ on; 0/off/false/no (any case/space) ⇒ off.
func TestVerifyFlagEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", true}, {"1", true}, {"yes", true}, {"on", true}, {"garbage", true},
		{"0", false}, {"off", false}, {"false", false}, {"no", false},
		{"OFF", false}, {" No ", false}, {"False", false},
	}
	for _, tc := range tests {
		t.Setenv("NILCORE_VCACHE", tc.val)
		if got := verifyFlagEnabled("NILCORE_VCACHE"); got != tc.want {
			t.Errorf("verifyFlagEnabled(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// TestVcacheDecorateGating proves the vcache stage is DEFAULT-ON (kernel
// precedent), =0 is the escape hatch, and a missing input skips the stage. The
// cache MECHANICS are tested in internal/verify/vcache; here we only assert the
// gate the cmd layer owns. The other two stages are pinned OFF so this test sees
// the vcache stage in isolation.
func TestVcacheDecorateGating(t *testing.T) {
	dir := t.TempDir()
	box := &fakeVerifierBox{dir: dir}
	base := verify.New(box, "true")
	logPath := filepath.Join(dir, "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	setVerifyFlags(t, "", "0", "0")
	// DEFAULT-ON: env unset with a full config ⇒ a wrapped cache verifier.
	if _, ok := vcacheDecorate(base, box, "true", log, logPath).(*vcache.Cache); !ok {
		t.Fatal("NILCORE_VCACHE unset must default ON (wrap in *vcache.Cache)")
	}
	// Legacy opt-in value keeps working.
	t.Setenv("NILCORE_VCACHE", "1")
	if _, ok := vcacheDecorate(base, box, "true", log, logPath).(*vcache.Cache); !ok {
		t.Fatal("NILCORE_VCACHE=1 must keep the cache on")
	}
	// The escape hatch: =0 ⇒ base unchanged, byte-identical.
	t.Setenv("NILCORE_VCACHE", "0")
	if got := vcacheDecorate(base, box, "true", log, logPath); got != verify.Verifier(base) {
		t.Fatal("NILCORE_VCACHE=0 must return the base verifier unchanged (byte-identical)")
	}
	// On but missing a required input ⇒ the stage is skipped (cannot record/verify safely).
	t.Setenv("NILCORE_VCACHE", "")
	if got := vcacheDecorate(base, box, "true", nil, logPath); got != verify.Verifier(base) {
		t.Fatal("a nil log must return base unchanged")
	}
	if got := vcacheDecorate(base, box, "true", log, ""); got != verify.Verifier(base) {
		t.Fatal("an empty log path must return base unchanged")
	}
	if got := vcacheDecorate(base, &fakeVerifierBox{dir: ""}, "true", log, logPath); got != verify.Verifier(base) {
		t.Fatal("an empty workdir must return base unchanged")
	}
}

// TestVerifyDecoratorChain pins the composed chain's shape and each stage's
// escape hatch: tiered OUTERMOST, then flakeprobe, then vcache hugging base
// (see vcacheDecorate for the order rationale), every stage default-on and
// individually disabled by =0.
func TestVerifyDecoratorChain(t *testing.T) {
	dir := t.TempDir()
	box := &fakeVerifierBox{dir: dir}
	// A SOUND go-test command so the tiered layer can arm when it is opted in; the
	// tiered flag is passed "1" in the subtests that expect the wrap (it is opt-in).
	const cmd = "go test ./..."
	base := verify.New(box, cmd)
	logPath := filepath.Join(dir, "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	t.Run("tiered opted in => tiered(flakeprobe(vcache(base)))", func(t *testing.T) {
		setVerifyFlags(t, "", "", "1")
		got := vcacheDecorate(base, box, cmd, log, logPath)
		tv, ok := got.(*verify.TieredVerifier)
		if !ok {
			t.Fatalf("outermost must be *verify.TieredVerifier, got %T", got)
		}
		if tv.ScopedRed == nil {
			t.Fatal("the tiered wrap must carry a ScopedRed seam")
		}
		fp, ok := tv.Full.(*verify.FlakeProbe)
		if !ok {
			t.Fatalf("tiered.Full must be *verify.FlakeProbe, got %T", tv.Full)
		}
		if fp.OnFlaky == nil {
			t.Fatal("a non-nil log must wire OnFlaky to the eventlog")
		}
		if _, ok := fp.Inner.(*vcache.Cache); !ok {
			t.Fatalf("flakeprobe.Inner must be *vcache.Cache (innermost), got %T", fp.Inner)
		}
		// The wired callback appends the additive verify_flaky kind (I5) with the
		// structural fail-class + content hash.
		fp.OnFlaky("test", "hash123abc")
		body := readFile(t, logPath)
		for _, want := range []string{"verify_flaky", "hash123abc", "fail_class"} {
			if !strings.Contains(body, want) {
				t.Fatalf("expected %q in the event log, got:\n%s", want, body)
			}
		}
	})
	t.Run("tiered default-off => probe outermost (no tiered layer)", func(t *testing.T) {
		setVerifyFlags(t, "", "", "")
		got := vcacheDecorate(base, box, cmd, log, logPath)
		if _, ok := got.(*verify.FlakeProbe); !ok {
			t.Fatalf("outermost must be *verify.FlakeProbe with tiered off by default, got %T", got)
		}
	})
	t.Run("NILCORE_FLAKEPROBE=0 strips only the probe layer", func(t *testing.T) {
		setVerifyFlags(t, "", "0", "1")
		got := vcacheDecorate(base, box, cmd, log, logPath)
		tv, ok := got.(*verify.TieredVerifier)
		if !ok {
			t.Fatalf("outermost must be *verify.TieredVerifier, got %T", got)
		}
		if _, ok := tv.Full.(*vcache.Cache); !ok {
			t.Fatalf("tiered.Full must be *vcache.Cache with the probe off, got %T", tv.Full)
		}
	})
	t.Run("all off => base unchanged (byte-identical escape hatch)", func(t *testing.T) {
		setVerifyFlags(t, "0", "0", "0")
		if got := vcacheDecorate(base, box, cmd, log, logPath); got != verify.Verifier(base) {
			t.Fatal("all flags off must return base unchanged")
		}
	})
	t.Run("nil log skips vcache + OnFlaky but keeps the probe", func(t *testing.T) {
		setVerifyFlags(t, "", "", "0")
		got := vcacheDecorate(base, box, cmd, nil, logPath)
		fp, ok := got.(*verify.FlakeProbe)
		if !ok {
			t.Fatalf("outermost must be *verify.FlakeProbe, got %T", got)
		}
		if fp.Inner != verify.Verifier(base) {
			t.Fatalf("with no log, flakeprobe must hug base directly, got %T", fp.Inner)
		}
		if fp.OnFlaky != nil {
			t.Fatal("with no log there is nothing to wire OnFlaky to")
		}
	})
}

// TestTieredSoundnessGate proves the hardened I2-soundness gate: the tiered wrap
// arms ONLY for a full-module `go test ./...` invocation — an opaque "make verify"
// (unknown recipe) and a single-package `go test` are NOT sound and stay unwrapped.
func TestTieredSoundnessGate(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"go build ./... && go test ./...", true}, // verify.Detect's go.mod fallback
		{"go test ./...", true},
		{"gofmt -l . && go test -race ./...", true},
		{"/usr/local/go/bin/go test ./...", true}, // path-invoked go is still go
		{"go test -short -tags integration ./...", true},
		{"make verify", false}, // opaque recipe: might run no tests / different flags ⇒ NOT sound
		{"  make verify  ", false},
		{"go test ./pkg", false}, // single-package run: scoped red need not be a subset of it
		{"go test ./internal/foo", false},
		{"true", false}, // verify.Detect's unknown-repo fallback
		{"npm test", false},
		{"cargo test ./...", false}, // "car|go test" — the go-test word-boundary check refuses it
		{"mongo test", false},
		{"pytest", false},
		{"make check", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := tieredSound(tc.cmd); got != tc.want {
			t.Errorf("tieredSound(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}

	// And the wiring honors it: an opaque "make verify" leaves the chain untiered
	// even with the tiered flag opted in.
	dir := t.TempDir()
	box := &fakeVerifierBox{dir: dir}
	base := verify.New(box, "make verify")
	setVerifyFlags(t, "0", "", "1")
	got := vcacheDecorate(base, box, "make verify", nil, "")
	if _, ok := got.(*verify.TieredVerifier); ok {
		t.Fatal("an opaque make-verify command must never be tiered-wrapped (soundness gate)")
	}
	if _, ok := got.(*verify.FlakeProbe); !ok {
		t.Fatalf("the probe (command-agnostic) must still wrap, got %T", got)
	}
}

// TestTieredVerifyOptIn pins the DEFAULT-OFF posture: the tiered layer wraps ONLY
// when NILCORE_TIERED_VERIFY=1 is set (a sound go-test command); unset/anything else
// leaves the chain untiered while vcache + flakeprobe stay default-on.
func TestTieredVerifyOptIn(t *testing.T) {
	dir := t.TempDir()
	box := &fakeVerifierBox{dir: dir}
	const cmd = "go test ./..." // a SOUND command, so only the opt-in flag decides
	base := verify.New(box, cmd)

	// Default (unset): NOT wrapped — the tiered fast path is opt-in.
	setVerifyFlags(t, "0", "0", "")
	if got := vcacheDecorate(base, box, cmd, nil, ""); base != got {
		t.Fatalf("NILCORE_TIERED_VERIFY unset must NOT tier-wrap (opt-in); got %T", got)
	}
	// A non-opt-in value ("on the fence") is still off — only 1/on/true/yes arm it.
	for _, off := range []string{"0", "off", "no", "garbage", "2"} {
		setVerifyFlags(t, "0", "0", off)
		if _, ok := vcacheDecorate(base, box, cmd, nil, "").(*verify.TieredVerifier); ok {
			t.Fatalf("NILCORE_TIERED_VERIFY=%q must NOT arm the tiered wrap", off)
		}
	}
	// The opt-in: =1 (and its synonyms) arm the tiered wrap over a sound command.
	for _, on := range []string{"1", "on", "true", "yes", "TRUE"} {
		setVerifyFlags(t, "0", "0", on)
		if _, ok := vcacheDecorate(base, box, cmd, nil, "").(*verify.TieredVerifier); !ok {
			t.Fatalf("NILCORE_TIERED_VERIFY=%q must arm the tiered wrap", on)
		}
	}
}

// scriptedExecBox replays one sandbox.Result (or error) per Exec call, recording
// the command lines — hermetic plumbing for the scopedRedFunc tests.
type scriptedExecBox struct {
	dir  string
	res  []sandbox.Result
	errs []error
	cmds []string
}

func (b *scriptedExecBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	i := len(b.cmds)
	b.cmds = append(b.cmds, cmd)
	var r sandbox.Result
	if i < len(b.res) {
		r = b.res[i]
	}
	var e error
	if i < len(b.errs) {
		e = b.errs[i]
	}
	return r, e
}

func (b *scriptedExecBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}

func (b *scriptedExecBox) Workdir() string { return b.dir }

// scopedTestTree creates a worktree root holding foo/ and bar/ Go packages plus a
// root-level main.go, so the host-side existence probe has something real to read.
func scopedTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"foo/a.go", "bar/b.go", "main.go"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestScopedRedFunc drives the wired ScopedRed seam end to end over a scripted
// sandbox: touched-package discovery, the targeted command shape (vet only for the
// make-verify family), and the fall-through semantics on every inconclusive path.
func TestScopedRedFunc(t *testing.T) {
	const goDefault = "go build ./... && go test ./..."

	t.Run("scoped red on a touched package", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\n"},
			{ExitCode: 1, Stdout: "--- FAIL: TestX\nFAIL\tnilcore/foo"},
		}}
		failed, out, err := scopedRedFunc(box, goDefault)(context.Background())
		if err != nil || !failed {
			t.Fatalf("want failed=true, got failed=%v err=%v", failed, err)
		}
		if !strings.Contains(out, "FAIL") {
			t.Fatalf("scoped output must carry the failure, got %q", out)
		}
		if want := "go test ./foo"; box.cmds[1] != want {
			t.Fatalf("scoped cmd = %q, want %q (no vet for a command that does not run vet)", box.cmds[1], want)
		}
	})
	t.Run("explicit go vet in the command folds vet in", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\nbar/b.go\nmain.go\n"},
			{ExitCode: 0},
		}}
		// A command that visibly runs vet AND is full-module go test.
		failed, _, err := scopedRedFunc(box, "go vet ./... && go test ./...")(context.Background())
		if err != nil || failed {
			t.Fatalf("green scoped run must report failed=false, got failed=%v err=%v", failed, err)
		}
		// Dirs deduped, sorted, root as "." — and vet prefixed since the command runs vet.
		if want := "go vet . ./bar ./foo && go test . ./bar ./foo"; box.cmds[1] != want {
			t.Fatalf("scoped cmd = %q, want %q", box.cmds[1], want)
		}
	})
	t.Run("go-test flags are replicated onto the scoped command", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\n"},
			{ExitCode: 0},
		}}
		cmd := "go test -short -race -tags integration -count 1 ./..."
		if _, _, err := scopedRedFunc(box, cmd)(context.Background()); err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		// The scoped run must test EXACTLY the subset the full command would (flags copied),
		// else it could red on a test/build the full command never executes.
		if want := "go test -short -race -tags integration -count 1 ./foo"; box.cmds[1] != want {
			t.Fatalf("scoped cmd = %q, want %q (flags must be replicated)", box.cmds[1], want)
		}
	})
	t.Run("a scoped package-LOAD error falls through to Full (not a subset red)", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\n"},
			// A nested go.mod / unresolved import: nonzero, but no package failed a gated check.
			{ExitCode: 1, Stderr: "no required module provides package nilcore/foo/bar; to add it:"},
		}}
		failed, _, err := scopedRedFunc(box, goDefault)(context.Background())
		if err != nil || failed {
			t.Fatalf("a package-load error must be inconclusive (fall through), got failed=%v err=%v", failed, err)
		}
	})
	t.Run("a bare 'go:' toolchain diagnostic falls through to Full", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\n"},
			{ExitCode: 1, Stderr: "go: updates to go.mod needed; to update it:\n\tgo mod tidy"},
		}}
		failed, _, err := scopedRedFunc(box, goDefault)(context.Background())
		if err != nil || failed {
			t.Fatalf("a toolchain diagnostic must fall through, got failed=%v err=%v", failed, err)
		}
	})
	t.Run("a genuine compile error IS a provable subset red", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\n"},
			{ExitCode: 1, Stderr: "# nilcore/foo\nfoo/a.go:3:1: syntax error: unexpected }"},
		}}
		failed, out, err := scopedRedFunc(box, goDefault)(context.Background())
		if err != nil || !failed {
			t.Fatalf("a compile error in a touched package must short-circuit, got failed=%v err=%v", failed, err)
		}
		if !strings.Contains(out, "syntax error") {
			t.Fatalf("scoped output must carry the compile error, got %q", out)
		}
	})
	t.Run("empty diff falls through without a second exec", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{{ExitCode: 0, Stdout: ""}}}
		failed, _, err := scopedRedFunc(box, goDefault)(context.Background())
		if err != nil || failed {
			t.Fatalf("empty diff must be inconclusive, got failed=%v err=%v", failed, err)
		}
		if len(box.cmds) != 1 {
			t.Fatalf("no scoped command may run on an empty diff, got %v", box.cmds)
		}
	})
	t.Run("non-Go touched file is unscopable", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{
			{ExitCode: 0, Stdout: "foo/a.go\ngo.mod\n"},
		}}
		failed, _, err := scopedRedFunc(box, goDefault)(context.Background())
		if err != nil || failed {
			t.Fatalf("a touched go.mod must fall through to Full, got failed=%v err=%v", failed, err)
		}
		if len(box.cmds) != 1 {
			t.Fatalf("unscopable change must not run a scoped command, got %v", box.cmds)
		}
	})
	t.Run("git faults surface as errors (decorator falls through)", func(t *testing.T) {
		box := &scriptedExecBox{dir: scopedTestTree(t), res: []sandbox.Result{{ExitCode: 128}}}
		if _, _, err := scopedRedFunc(box, goDefault)(context.Background()); err == nil {
			t.Fatal("a non-zero git exit must be an error, never a verdict")
		}
	})
}

// TestGoTestFlags pins the flag replication: only flags that change WHICH tests/builds
// run are copied (so the scoped subset matches the full command), in both the
// space-separated and "=" value forms; unrelated tokens are dropped.
func TestGoTestFlags(t *testing.T) {
	tests := []struct {
		cmd  string
		want string
	}{
		{"go test ./...", ""},
		{"go test -short ./...", "-short"},
		{"go test -race ./...", "-race"},
		{"go test -short -race ./...", "-short -race"},
		{"go test -tags integration ./...", "-tags integration"},
		{"go test -tags=integration ./...", "-tags=integration"},
		{"go test -count 1 ./...", "-count 1"},
		{"go test -count=1 ./...", "-count=1"},
		{"go build ./... && go test -short -tags a,b -race ./...", "-short -tags a,b -race"},
		{"go test -v -json ./...", ""}, // -v/-json don't change the subset ⇒ not copied
	}
	for _, tc := range tests {
		if got := goTestFlags(tc.cmd); got != tc.want {
			t.Errorf("goTestFlags(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// TestScopedRedIsProvable tables the provable-red classifier: a genuine test/compile
// failure is a shippable subset red; a package-load/resolution error or any other
// ambiguous nonzero output is NOT (fall through to Full).
func TestScopedRedIsProvable(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"test failure", "--- FAIL: TestX (0.01s)\nFAIL\tnilcore/foo\t0.02s", true},
		{"bare FAIL line", "FAIL\tnilcore/foo", true},
		{"compile error", "# nilcore/foo\nfoo/a.go:3:1: syntax error", true},
		{"unresolved import", "no required module provides package nilcore/x; to add it:", false},
		{"cannot find package", "cannot find package nilcore/x", false},
		{"go.mod needs update", "go: updates to go.mod needed; run 'go mod tidy'", false},
		{"missing go.sum", "missing go.sum entry for module providing package x", false},
		{"bare go: diagnostic", "go: cannot find main module", false},
		{"opaque nonzero", "some unrecognized failure", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		if got := scopedRedIsProvable(tc.output); got != tc.want {
			t.Errorf("scopedRedIsProvable(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestTouchedGoPackageDirs tables the file→package mapping, its hygiene refusals,
// and the deleted-package skip.
func TestTouchedGoPackageDirs(t *testing.T) {
	root := scopedTestTree(t)
	tests := []struct {
		name  string
		lines string
		want  []string
		ok    bool
	}{
		{"dedup + sort + ./ prefix", "foo/a.go\nfoo/zz.go\nbar/b.go\n", []string{"./bar", "./foo"}, true},
		{"root package maps to .", "main.go\n", []string{"."}, true},
		{"blank lines ignored", "\nfoo/a.go\n\n", []string{"./foo"}, true},
		{".nilcore scratch ignored", ".nilcore/state.go\nfoo/a.go\n", []string{"./foo"}, true},
		{"deleted package dir skipped", "foo/a.go\ngone/z.go\n", []string{"./foo"}, true},
		{"nothing touched", "", nil, true},
		{"non-go file unscopable", "foo/a.go\nMakefile\n", nil, false},
		{"testdata input unscopable", "foo/testdata/in.json\n", nil, false},
		{"shell metacharacters refused", "foo/$(rm).go\n", nil, false},
		{"space refused", "foo/a b.go\n", nil, false},
		{"leading dash refused", "-flag/x.go\n", nil, false},
		{"dotdot refused", "foo/../bar/b.go\n", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := touchedGoPackageDirs(root, tc.lines)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("dirs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dirs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

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

// writeQuoteArtifact writes a KindReport artifact whose single claim ASSERTS a Value
// ("v1") verified by the VALUE-CHECKING web.quote_exists — the genuinely-verifiable
// green fixture. A KindReport claim must carry a Value (schema), and a value-bearing
// claim must name a verifier that inspects that Value (evverify's anti-hollow-green
// gate) — so url_resolves is inadequate here. Run with NILCORE_VERIFY_PACKS=web (so
// quote_exists resolves) and a fakeVerifierBox whose stdout contains the Value:
// quote_exists fetches the body through the box and checks the Value appears in it.
func writeQuoteArtifact(t *testing.T, root, id, sourceURL string) string {
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
				Verifier:  "web.quote_exists",
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
		t.Setenv("NILCORE_VERIFY_PACKS", "web")                          // register web.quote_exists (value-checking)
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0, stdout: "v1"} // 2xx + body contains the claimed Value
		writeQuoteArtifact(t, box.dir, "rep", "https://example.com")

		v := behavioralVerifier(box, "true")
		// Evidence legs are discovered at Check time (the artifact does not exist when
		// the verifier is constructed), so the ordering is asserted over the list the
		// composite builds per Check rather than over a frozen constructor result.
		bc, ok := v.(behavioralComposite)
		if !ok {
			t.Fatalf("flag set must return a behavioralComposite, got %#v", v)
		}
		named := bc.compose()
		if len(named) < 3 {
			t.Fatalf("Composite must have build + schema + evidence, got %d verifiers", len(named))
		}
		if named[0].Name != "checks" {
			t.Fatalf("Named[0] must be the build verifier, got %q", named[0].Name)
		}
		// The cheap structural shape gate runs before the per-claim evidence check, so a
		// shape defect short-circuits first — matching the swarm path's packs.Build order.
		if !strings.HasPrefix(named[1].Name, "schema") {
			t.Fatalf("schema gate must follow the build verifier, got %q", named[1].Name)
		}
		if !strings.HasPrefix(named[2].Name, "evidence") {
			t.Fatalf("evidence verifier must follow the schema gate, got %q", named[2].Name)
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
