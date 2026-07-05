package verify

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// scriptedVerifier replays a fixed sequence of reports/errors, one per Check call,
// and counts calls — the hermetic stand-in for the real inner verifier.
type scriptedVerifier struct {
	reports []Report
	errs    []error
	calls   int
}

func (s *scriptedVerifier) Check(context.Context) (Report, error) {
	i := s.calls
	s.calls++
	if i >= len(s.reports) {
		return Report{}, fmt.Errorf("scripted verifier: unexpected call %d", i+1)
	}
	var err error
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return s.reports[i], err
}

// Canned reports whose Output classifies structurally (enrich.go FailClass).
var (
	repPass     = Report{Passed: true, Output: "ok"}
	repTestRed  = Report{Passed: false, Output: "go test ./...\nFAIL\tnilcore/internal/x"}
	repBuildRed = Report{Passed: false, Output: "go build ./...\n./x.go:1: syntax error"}
)

// scriptedHash returns one hash (or error) per call.
func scriptedHash(vals []string, errs []error) func(context.Context) (string, error) {
	i := 0
	return func(context.Context) (string, error) {
		v, e := "", error(nil)
		if i < len(vals) {
			v = vals[i]
		}
		if i < len(errs) {
			e = errs[i]
		}
		i++
		return v, e
	}
}

// TestFlakeProbeNoProbe tables every condition under which the probe must NOT
// fire: the inner verifier is called exactly once per Check and the original
// verdict stands untouched.
func TestFlakeProbeNoProbe(t *testing.T) {
	tests := []struct {
		name    string
		reports []Report // one per Check call; NO probe entry — a probe would over-run the script
		hashes  []string
		hashErr []error
	}{
		{
			name:    "first check has no preceding hash",
			reports: []Report{repTestRed},
			hashes:  []string{"h"},
		},
		{
			name:    "different content hash between checks",
			reports: []Report{repTestRed, repTestRed},
			hashes:  []string{"h1", "h2"},
		},
		{
			name:    "non-test fail class (build red is deterministic)",
			reports: []Report{repBuildRed, repBuildRed},
			hashes:  []string{"h", "h"},
		},
		{
			name:    "passing runs never probe",
			reports: []Report{repPass, repPass},
			hashes:  []string{"h", "h"},
		},
		{
			name:    "hash error on the failing check disables probing",
			reports: []Report{repTestRed, repTestRed},
			hashes:  []string{"h", ""},
			hashErr: []error{nil, errors.New("walk failed")},
		},
		{
			name:    "hash error invalidates memory for the NEXT check too",
			reports: []Report{repPass, repTestRed, repTestRed},
			hashes:  []string{"h", "", "h"},
			hashErr: []error{nil, errors.New("walk failed"), nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner := &scriptedVerifier{reports: tc.reports}
			flaky := 0
			p := &FlakeProbe{
				Inner:   inner,
				Hash:    scriptedHash(tc.hashes, tc.hashErr),
				OnFlaky: func(string, string) { flaky++ },
			}
			var last Report
			for i := range tc.reports {
				rep, err := p.Check(context.Background())
				if err != nil {
					t.Fatalf("Check %d: %v", i+1, err)
				}
				last = rep
			}
			if inner.calls != len(tc.reports) {
				t.Fatalf("inner called %d times, want %d (no probe)", inner.calls, len(tc.reports))
			}
			if want := tc.reports[len(tc.reports)-1]; last != want {
				t.Fatalf("verdict mutated: got %+v want %+v", last, want)
			}
			if flaky != 0 {
				t.Fatalf("OnFlaky fired %d times, want 0", flaky)
			}
		})
	}
}

// TestFlakeProbeConfirmedFlake is the happy path: a test-class red on content
// identical to the preceding Check re-runs the real verifier once; the re-run's
// pass IS the verdict and OnFlaky fires with (fail_class, content_hash).
func TestFlakeProbeConfirmedFlake(t *testing.T) {
	inner := &scriptedVerifier{reports: []Report{repTestRed, repTestRed, repPass}}
	var gotClass, gotHash string
	fired := 0
	p := &FlakeProbe{
		Inner: inner,
		Hash:  scriptedHash([]string{"h", "h"}, nil),
		OnFlaky: func(class, hash string) {
			fired++
			gotClass, gotHash = class, hash
		},
	}

	// Check 1: red, but no preceding hash — no probe, red stands.
	if rep, err := p.Check(context.Background()); err != nil || rep.Passed {
		t.Fatalf("check 1: want unprobed red, got %+v err=%v", rep, err)
	}
	// Check 2: same hash + test red ⇒ probe once ⇒ the probe's pass is the verdict.
	rep, err := p.Check(context.Background())
	if err != nil {
		t.Fatalf("check 2: %v", err)
	}
	if !rep.Passed {
		t.Fatalf("probe pass must be the verdict, got %+v", rep)
	}
	if inner.calls != 3 {
		t.Fatalf("inner called %d times, want 3 (1 + fail + one probe)", inner.calls)
	}
	if fired != 1 || gotClass != FailClassTest || gotHash != "h" {
		t.Fatalf("OnFlaky: fired=%d class=%q hash=%q, want 1/%q/%q", fired, gotClass, gotHash, FailClassTest, "h")
	}
}

// TestFlakeProbeProbeStillRed proves the one-probe bound and that a failing probe
// leaves the ORIGINAL failure as the verdict.
func TestFlakeProbeProbeStillRed(t *testing.T) {
	original := Report{Passed: false, Output: "go test ./...\nFAIL original"}
	probeRed := Report{Passed: false, Output: "go test ./...\nFAIL probe"}
	inner := &scriptedVerifier{reports: []Report{original, original, probeRed}}
	fired := 0
	p := &FlakeProbe{
		Inner:   inner,
		Hash:    scriptedHash([]string{"h", "h"}, nil),
		OnFlaky: func(string, string) { fired++ },
	}

	if _, err := p.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep, err := p.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep != original {
		t.Fatalf("the ORIGINAL fail must stand, got %+v", rep)
	}
	// 1 (first check) + 1 (second check) + exactly ONE probe — never a retry loop.
	if inner.calls != 3 {
		t.Fatalf("inner called %d times, want 3 (one probe per Check, bounded)", inner.calls)
	}
	if fired != 0 {
		t.Fatal("OnFlaky must not fire for an unconfirmed flake")
	}
}

// TestFlakeProbeProbeError proves an erroring probe never worsens the outcome:
// the original red is returned with a nil error.
func TestFlakeProbeProbeError(t *testing.T) {
	inner := &scriptedVerifier{
		reports: []Report{repTestRed, repTestRed, {}},
		errs:    []error{nil, nil, errors.New("sandbox died")},
	}
	p := &FlakeProbe{Inner: inner, Hash: scriptedHash([]string{"h", "h"}, nil)}
	if _, err := p.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep, err := p.Check(context.Background())
	if err != nil {
		t.Fatalf("probe error must not surface: %v", err)
	}
	if rep != repTestRed {
		t.Fatalf("original red must stand on probe error, got %+v", rep)
	}
}

// TestFlakeProbeOpaqueRecipeIsProbed proves the broadened detection: a test flake
// hidden behind an OPAQUE recipe (make/wrapper), whose first output line is the make
// wrapper rather than `go test`, is still recognized as a test-class failure and
// probed. First-line command sniffing (FailClass) returns "other" here, so without
// the full-output scan this flake would never get its one probe.
func TestFlakeProbeOpaqueRecipeIsProbed(t *testing.T) {
	// First line is the make wrapper (FailClass -> "other"); the go test banner is
	// buried below. FailClassUnknown + a test-runner signature ⇒ probe.
	opaqueRed := Report{
		Passed: false,
		Output: "make: *** [Makefile:12: test] Error 1\n--- FAIL: TestFlaky (0.01s)\n    x_test.go:9: timing",
	}
	// Sanity: first-line sniffing does NOT see this as a test class.
	if got := FailClass(opaqueRed); got == FailClassTest {
		t.Fatalf("precondition: FailClass should not already classify the opaque recipe as test, got %q", got)
	}

	inner := &scriptedVerifier{reports: []Report{opaqueRed, opaqueRed, repPass}}
	fired := 0
	p := &FlakeProbe{
		Inner:   inner,
		Hash:    scriptedHash([]string{"h", "h"}, nil),
		OnFlaky: func(string, string) { fired++ },
	}

	// Check 1: red, no preceding hash ⇒ no probe.
	if rep, err := p.Check(context.Background()); err != nil || rep.Passed {
		t.Fatalf("check 1: want unprobed red, got %+v err=%v", rep, err)
	}
	// Check 2: same hash + opaque-recipe test red ⇒ probe ⇒ probe pass is the verdict.
	rep, err := p.Check(context.Background())
	if err != nil {
		t.Fatalf("check 2: %v", err)
	}
	if !rep.Passed {
		t.Fatalf("opaque-recipe flake must be probed and the probe pass returned, got %+v", rep)
	}
	if inner.calls != 3 {
		t.Fatalf("inner called %d times, want 3 (1 + fail + one probe)", inner.calls)
	}
	if fired != 1 {
		t.Fatalf("OnFlaky must fire once for the confirmed opaque-recipe flake, fired=%d", fired)
	}
}

// TestFlakeProbeOpaqueBuildRedNotProbed guards the other direction: a DETERMINISTIC
// build red must never be probed even if its output mentions the word "test". A
// structurally-classified build/lint/browser red is a compiler/analyzer fact, not a
// flake.
func TestFlakeProbeOpaqueBuildRedNotProbed(t *testing.T) {
	// FailClass classifies this as build (first token "go", subcommand "build"); the
	// stray "test" word must not upgrade it to a probe candidate.
	buildRed := Report{Passed: false, Output: "go build ./...\n./test_helpers.go:3: undefined: Foo"}
	if got := FailClass(buildRed); got != FailClassBuild {
		t.Fatalf("precondition: want build class, got %q", got)
	}
	inner := &scriptedVerifier{reports: []Report{buildRed, buildRed}}
	p := &FlakeProbe{Inner: inner, Hash: scriptedHash([]string{"h", "h"}, nil)}
	for i := 0; i < 2; i++ {
		if _, err := p.Check(context.Background()); err != nil {
			t.Fatalf("check %d: %v", i+1, err)
		}
	}
	if inner.calls != 2 {
		t.Fatalf("a deterministic build red must not be probed; inner ran %d, want 2", inner.calls)
	}
}

// TestFlakeProbeGuards covers the structural guards: nil Inner errors, nil Hash
// disables probing, and an inner error propagates unchanged.
func TestFlakeProbeGuards(t *testing.T) {
	t.Run("nil inner", func(t *testing.T) {
		if _, err := (&FlakeProbe{}).Check(context.Background()); err == nil {
			t.Fatal("nil Inner must error")
		}
	})
	t.Run("nil hash never probes", func(t *testing.T) {
		inner := &scriptedVerifier{reports: []Report{repTestRed, repTestRed}}
		p := &FlakeProbe{Inner: inner}
		for i := 0; i < 2; i++ {
			if rep, err := p.Check(context.Background()); err != nil || rep.Passed {
				t.Fatalf("check %d: %+v err=%v", i+1, rep, err)
			}
		}
		if inner.calls != 2 {
			t.Fatalf("inner called %d times, want 2 (nil Hash disables probing)", inner.calls)
		}
	})
	t.Run("inner error propagates", func(t *testing.T) {
		boom := errors.New("boom")
		inner := &scriptedVerifier{reports: []Report{{}}, errs: []error{boom}}
		p := &FlakeProbe{Inner: inner, Hash: scriptedHash([]string{"h"}, nil)}
		if _, err := p.Check(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("want inner error, got %v", err)
		}
	})
}
