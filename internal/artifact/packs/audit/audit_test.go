package audit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in (mirrors the software/ui pack fakes):
// it records EVERY command it is asked to run and returns a canned Result/error so the
// sed/grep branches of each CheckFunc are driven with no real sed/grep on disk. calls
// counts invocations so a test can assert NO box reach happened on a path-escape.
type fakeBox struct {
	cmds []string // every command, in order
	exec func(cmd string) (sandbox.Result, error)
}

func (b *fakeBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.cmds = append(b.cmds, cmd)
	if b.exec != nil {
		return b.exec(cmd)
	}
	return sandbox.Result{}, nil
}
func (b *fakeBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *fakeBox) Workdir() string { return "/work" }

func (b *fakeBox) calls() int {
	return len(b.cmds)
}
func (b *fakeBox) lastCmd() string {
	if len(b.cmds) == 0 {
		return ""
	}
	return b.cmds[len(b.cmds)-1]
}

// boxReturning is a fakeBox whose every Exec returns the given canned stdout/exit.
func boxReturning(stdout string, exit int) *fakeBox {
	return &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{Stdout: stdout, ExitCode: exit}, nil
	}}
}

// claim builds a claim with the locator in SourceURL, the asserted text in Value, and an
// optional claimed-count Statement (used only by the reproduce check).
func claim(verifier, locator, val, statement string) artifact.Claim {
	return artifact.Claim{
		ID:        "c1",
		Field:     "f1",
		Statement: statement,
		Evidence: artifact.Evidence{
			SourceURL: locator,
			Value:     val,
			Verifier:  verifier,
		},
	}
}

func TestRegisterAll(t *testing.T) {
	want := []string{IDFileLineExists, IDPatternMatches, IDFindingReproduces}

	r := evverify.New()
	for _, id := range want {
		if _, ok := r.Lookup(id); ok {
			t.Fatalf("id %q resolvable before RegisterAll", id)
		}
	}
	RegisterAll(r)
	for _, id := range want {
		if _, ok := r.Lookup(id); !ok {
			t.Fatalf("id %q not resolvable after RegisterAll", id)
		}
	}
	// Does not leak a foreign id.
	if _, ok := r.Lookup("software.npm_version_exists"); ok {
		t.Fatalf("audit pack leaked a non-pack id")
	}
}

func TestHostsIsNil(t *testing.T) {
	if h := Hosts(); h != nil {
		t.Fatalf("Hosts() = %v, want nil (audit reads only local files, no egress)", h)
	}
}

// --- audit.file_line_exists --------------------------------------------------

func TestFileLineExists(t *testing.T) {
	ctx := context.Background()

	t.Run("present non-empty line => Pass", func(t *testing.T) {
		box := boxReturning("func main() {\n", 0)
		st, _ := checkFileLineExists(ctx, box, claim(IDFileLineExists, "cmd/main.go:42", "", ""))
		if st != artifact.StatusPass {
			t.Fatalf("present line = %q, want pass", st)
		}
	})

	t.Run("command is single-quoted around the path", func(t *testing.T) {
		box := boxReturning("x\n", 0)
		_, _ = checkFileLineExists(ctx, box, claim(IDFileLineExists, "internal/a/b.go:7", "", ""))
		cmd := box.lastCmd()
		// sed -n '7p' 'internal/a/b.go'
		if !strings.Contains(cmd, "sed -n '7p'") {
			t.Fatalf("cmd %q missing single-quoted line number", cmd)
		}
		if !strings.Contains(cmd, "'internal/a/b.go'") {
			t.Fatalf("cmd %q does not single-quote the path", cmd)
		}
	})

	t.Run("empty body (exit 0) => Fail (no such line)", func(t *testing.T) {
		box := boxReturning("", 0)
		st, _ := checkFileLineExists(ctx, box, claim(IDFileLineExists, "cmd/main.go:9999", "", ""))
		if st != artifact.StatusFail {
			t.Fatalf("empty line = %q, want fail", st)
		}
	})

	t.Run("whitespace-only body (exit 0) => Fail", func(t *testing.T) {
		box := boxReturning("   \n", 0)
		st, _ := checkFileLineExists(ctx, box, claim(IDFileLineExists, "cmd/main.go:3", "", ""))
		if st != artifact.StatusFail {
			t.Fatalf("blank line = %q, want fail", st)
		}
	})

	t.Run("non-zero exit (file absent) => Fail", func(t *testing.T) {
		box := boxReturning("", 2)
		st, _ := checkFileLineExists(ctx, box, claim(IDFileLineExists, "no/such/file.go:1", "", ""))
		if st != artifact.StatusFail {
			t.Fatalf("missing file = %q, want fail", st)
		}
	})

	t.Run("sandbox-level error => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("box exploded")
		}}
		st, _ := checkFileLineExists(ctx, box, claim(IDFileLineExists, "cmd/main.go:1", "", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("sandbox error = %q, want unverifiable", st)
		}
	})

	t.Run("nil box => Unverifiable, no host-side read", func(t *testing.T) {
		st, d := checkFileLineExists(ctx, nil, claim(IDFileLineExists, "cmd/main.go:1", "", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("nil box = %q, want unverifiable", st)
		}
		if !strings.Contains(d, "no sandbox") {
			t.Fatalf("nil box detail = %q, want a no-sandbox reason", d)
		}
	})

	t.Run("exactly one box.Exec per check", func(t *testing.T) {
		box := boxReturning("line\n", 0)
		_, _ = checkFileLineExists(ctx, box, claim(IDFileLineExists, "a.go:1", "", ""))
		if box.calls() != 1 {
			t.Fatalf("made %d box.Exec calls, want exactly 1", box.calls())
		}
	})
}

// --- locator-escape: NO box call --------------------------------------------

func TestLocatorEscapeIsUnverifiableWithNoBoxCall(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		locator string
	}{
		{"dotdot segment", "../etc/passwd:1"},
		{"nested dotdot", "internal/../../secret:3"},
		{"leading slash", "/etc/passwd:1"},
		{"leading dash path (sed/grep option)", "-rfoo.go:1"},
		{"leading double-dash option-like path", "--version:1"},
		{"single quote in path", "a'b.go:1"},
		{"whitespace in path", "a b.go:1"},
		{"control byte in path", "a\tb.go:1"},
		{"no line suffix", "cmd/main.go"},
		{"non-integer line", "cmd/main.go:notanumber"},
		{"zero line", "cmd/main.go:0"},
		{"negative line", "cmd/main.go:-4"},
		{"empty locator", ""},
		{"trailing dotdot segment", "a/.."},
	}
	// Every check that takes a locator must refuse an escape BEFORE any reach.
	checks := map[string]evverify.CheckFunc{
		IDFileLineExists:    checkFileLineExists,
		IDPatternMatches:    checkPatternMatches,
		IDFindingReproduces: checkFindingReproduces,
	}
	for _, tc := range cases {
		for id, fn := range checks {
			box := boxReturning("anything\n", 0)
			// pattern/count are present so the ONLY rejection reason is the locator.
			st, _ := fn(ctx, box, claim(id, tc.locator, "x", "1"))
			if st != artifact.StatusUnverifiable {
				t.Fatalf("%s on %q (%s) = %q, want unverifiable", id, tc.locator, tc.name, st)
			}
			if box.calls() != 0 {
				t.Fatalf("%s on %q (%s) made %d box.Exec calls, want 0 (escape must not reach the box)", id, tc.locator, tc.name, box.calls())
			}
		}
	}
}

// a legitimate file named with embedded dots (not a ".." segment) must still pass
// validation and reach the box.
func TestDottyFilenameIsNotAnEscape(t *testing.T) {
	ctx := context.Background()
	box := boxReturning("content\n", 0)
	st, _ := checkFileLineExists(ctx, box, claim(IDFileLineExists, "pkg/a..b.go:1", "", ""))
	if st != artifact.StatusPass {
		t.Fatalf("a..b.go = %q, want pass (embedded dots are not a traversal)", st)
	}
	if box.calls() != 1 {
		t.Fatalf("legit dotty filename made %d calls, want 1", box.calls())
	}
}

// --- audit.pattern_matches ---------------------------------------------------

func TestPatternMatches(t *testing.T) {
	ctx := context.Background()

	t.Run("Value is a substring of the cited line => Pass", func(t *testing.T) {
		box := boxReturning("    return fmt.Errorf(\"boom\")\n", 0)
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:5", "fmt.Errorf", ""))
		if st != artifact.StatusPass {
			t.Fatalf("substring present = %q, want pass", st)
		}
	})

	t.Run("whitespace-normalized match => Pass", func(t *testing.T) {
		// Source line has collapsed/odd spacing vs the asserted value.
		box := boxReturning("foo    bar     baz\n", 0)
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:1", "bar baz", ""))
		if st != artifact.StatusPass {
			t.Fatalf("normalized substring = %q, want pass", st)
		}
	})

	t.Run("Value not on the cited line => Fail", func(t *testing.T) {
		box := boxReturning("a wholly different line\n", 0)
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:1", "panic(", ""))
		if st != artifact.StatusFail {
			t.Fatalf("absent substring = %q, want fail", st)
		}
	})

	t.Run("empty cited line => Fail", func(t *testing.T) {
		box := boxReturning("", 0)
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:99", "anything", ""))
		if st != artifact.StatusFail {
			t.Fatalf("empty line = %q, want fail", st)
		}
	})

	t.Run("file absent (non-zero exit) => Fail", func(t *testing.T) {
		box := boxReturning("", 2)
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "no.go:1", "x", ""))
		if st != artifact.StatusFail {
			t.Fatalf("missing file = %q, want fail", st)
		}
	})

	t.Run("empty Value => Unverifiable (no vacuous match)", func(t *testing.T) {
		box := boxReturning("some line\n", 0)
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:1", "", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("empty value = %q, want unverifiable", st)
		}
	})

	t.Run("the raw cited line is NOT echoed into Detail (I7)", func(t *testing.T) {
		secret := "TOTALLY_SECRET_TOKEN_abc123"
		box := boxReturning(secret+" on this line\n", 0)
		_, d := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:1", "not-present", ""))
		if strings.Contains(d, secret) {
			t.Fatalf("detail %q echoed the raw cited line content (I7 violation)", d)
		}
	})

	t.Run("sandbox-level error => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("kaboom")
		}}
		st, _ := checkPatternMatches(ctx, box, claim(IDPatternMatches, "x.go:1", "y", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("sandbox error = %q, want unverifiable", st)
		}
	})
}

// --- audit.finding_reproduces ------------------------------------------------

func TestFindingReproduces(t *testing.T) {
	ctx := context.Background()

	t.Run("anchored grep count matches the claim => Pass", func(t *testing.T) {
		// Cited line is 3; matches at 2,3,4 are all within reproWindow (±2) of it.
		box := boxReturning("2:TODO a\n3:TODO b\n4:TODO c\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:3", "TODO", "3"))
		if st != artifact.StatusPass {
			t.Fatalf("anchored count match = %q, want pass", st)
		}
	})

	t.Run("grep is fixed-string line-numbered, single-quoted pattern and path", func(t *testing.T) {
		box := boxReturning("1:needle\n", 0)
		_, _ = checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "pkg/a.go:1", "needle", "1"))
		cmd := box.lastCmd()
		if !strings.Contains(cmd, "grep -n -F 'needle' 'pkg/a.go'") {
			t.Fatalf("cmd %q is not a single-quoted fixed-string line-numbered grep", cmd)
		}
		// -c (count-only) must be gone: we need per-match line numbers to anchor.
		if strings.Contains(cmd, "-c") {
			t.Fatalf("cmd %q still uses -c; anchoring needs line numbers, not a bare count", cmd)
		}
	})

	t.Run("grep count mismatch within window => Fail", func(t *testing.T) {
		// Cited line 1, window matches at 1,2,3 => 3 in-window, but claim says 5.
		box := boxReturning("1:x\n2:x\n3:x\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "5"))
		if st != artifact.StatusFail {
			t.Fatalf("count mismatch = %q, want fail", st)
		}
	})

	// THE discriminating test for MINOR #14: the SAME pattern present in the file, at the
	// SAME claimed count, but FAR from the cited line, must NOT pass. The old whole-file
	// grep accepted this; the anchored check must reject it as Fail (the finding does not
	// reproduce at the citation).
	t.Run("match far from the cited line => Fail (anchoring guard)", func(t *testing.T) {
		// One match in the file, but at line 200 while the finding cites line 5.
		box := boxReturning("200:TODO over here\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:5", "TODO", "1"))
		if st != artifact.StatusFail {
			t.Fatalf("far-away match = %q, want fail (must not reproduce at the citation)", st)
		}
	})

	// Same match-set, same claimed count, but moving the CITATION on/off the match flips the
	// verdict — proving the cited line is load-bearing (not ignored as before).
	t.Run("citation on the match passes; citation off it fails (same grep output)", func(t *testing.T) {
		grep := "100:TODO\n"
		onSt, _ := checkFindingReproduces(ctx, boxReturning(grep, 0), claim(IDFindingReproduces, "x.go:100", "TODO", "1"))
		if onSt != artifact.StatusPass {
			t.Fatalf("citation ON the match = %q, want pass", onSt)
		}
		offSt, _ := checkFindingReproduces(ctx, boxReturning(grep, 0), claim(IDFindingReproduces, "x.go:50", "TODO", "1"))
		if offSt != artifact.StatusFail {
			t.Fatalf("citation OFF the match = %q, want fail (same grep output, only the cited line moved)", offSt)
		}
	})

	// A match just outside the window must not count; just inside must.
	t.Run("window boundary: ±2 counts, ±3 does not", func(t *testing.T) {
		// Cited 10: a match at 12 (in, |Δ|=2) and one at 13 (out, |Δ|=3). Claim 1 in-window.
		box := boxReturning("12:TODO\n13:TODO\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:10", "TODO", "1"))
		if st != artifact.StatusPass {
			t.Fatalf("window boundary = %q, want pass (only the ±2 match counts)", st)
		}
	})

	t.Run("grep exit 1 (no match) with claim 0 => Pass", func(t *testing.T) {
		box := boxReturning("0\n", 1)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "neverthere", "0"))
		if st != artifact.StatusPass {
			t.Fatalf("no-match claim-0 = %q, want pass", st)
		}
	})

	t.Run("grep exit 1 (no match) with positive claim => Fail", func(t *testing.T) {
		box := boxReturning("0\n", 1)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "neverthere", "2"))
		if st != artifact.StatusFail {
			t.Fatalf("no-match claim-2 = %q, want fail", st)
		}
	})

	t.Run("grep exit >1 (error) => Unverifiable", func(t *testing.T) {
		box := boxReturning("", 2)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("grep error = %q, want unverifiable", st)
		}
	})

	t.Run("malformed grep -n output (no line-number prefix) => Unverifiable", func(t *testing.T) {
		// A line with no ':' separator is not valid `grep -n` output; it must not be coerced
		// into a count.
		box := boxReturning("no-colon-here\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("malformed -n output = %q, want unverifiable", st)
		}
	})

	t.Run("non-integer line-number prefix => Unverifiable", func(t *testing.T) {
		box := boxReturning("NaN:TODO\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("non-integer line number = %q, want unverifiable", st)
		}
	})

	t.Run("missing claimed count => Unverifiable", func(t *testing.T) {
		box := boxReturning("1\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("missing count = %q, want unverifiable", st)
		}
	})

	t.Run("non-integer claimed count => Unverifiable", func(t *testing.T) {
		box := boxReturning("1\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "lots"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("bad count statement = %q, want unverifiable", st)
		}
	})

	t.Run("empty pattern => Unverifiable", func(t *testing.T) {
		box := boxReturning("1\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("empty pattern = %q, want unverifiable", st)
		}
	})

	t.Run("pattern with a single quote => Unverifiable, no box call", func(t *testing.T) {
		box := boxReturning("1\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "it's", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("quoted pattern = %q, want unverifiable", st)
		}
		if box.calls() != 0 {
			t.Fatalf("quoted pattern made %d box.Exec calls, want 0", box.calls())
		}
	})

	// A leading '-' pattern is a grep OPTION-injection ("-rfoo" ⇒ -r -f oo). It must be
	// refused BEFORE the grep runs — the fake box fails the test if Exec is reached.
	t.Run("pattern with a leading dash => Unverifiable, no box call", func(t *testing.T) {
		for _, pat := range []string{"-rfoo", "--include=*.go", "-n"} {
			box := boxReturning("1:x\n", 0)
			st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", pat, "1"))
			if st != artifact.StatusUnverifiable {
				t.Fatalf("leading-dash pattern %q = %q, want unverifiable", pat, st)
			}
			if box.calls() != 0 {
				t.Fatalf("leading-dash pattern %q made %d box.Exec calls, want 0 (must refuse before grep)", pat, box.calls())
			}
		}
		// Control: a pattern with an INNER dash (not leading) is fine and reaches the box.
		box := boxReturning("1:a-b\n", 0)
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "a-b", "1"))
		if st != artifact.StatusPass {
			t.Fatalf("inner-dash pattern = %q, want pass (only a LEADING dash is refused)", st)
		}
	})

	t.Run("nil box => Unverifiable", func(t *testing.T) {
		st, _ := checkFindingReproduces(ctx, nil, claim(IDFindingReproduces, "x.go:1", "TODO", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("nil box = %q, want unverifiable", st)
		}
	})

	t.Run("sandbox-level error => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("boom")
		}}
		st, _ := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("sandbox error = %q, want unverifiable", st)
		}
	})

	t.Run("exactly one box.Exec per check", func(t *testing.T) {
		box := boxReturning("1:TODO\n2:TODO\n", 0)
		_, _ = checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "2"))
		if box.calls() != 1 {
			t.Fatalf("made %d box.Exec calls, want exactly 1", box.calls())
		}
	})

	t.Run("matched line text is never echoed into Detail (I7)", func(t *testing.T) {
		secret := "TOTALLY_SECRET_TOKEN_xyz789"
		// One match far from the citation => Fail; the matched line text must not leak.
		box := boxReturning("200:"+secret+"\n", 0)
		_, d := checkFindingReproduces(ctx, box, claim(IDFindingReproduces, "x.go:1", "TODO", "1"))
		if strings.Contains(d, secret) {
			t.Fatalf("detail %q echoed the matched line text (I7 violation)", d)
		}
	})
}

// detail must never exceed the bound, regardless of input.
func TestDetailBounded(t *testing.T) {
	long := strings.Repeat("z", maxDetail*3)
	if got := detail(long); len(got) > maxDetail {
		t.Fatalf("detail length = %d, want <= %d", len(got), maxDetail)
	}
}
