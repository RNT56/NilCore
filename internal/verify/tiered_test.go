package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubFull is a countable Full verifier with a fixed verdict.
type stubFull struct {
	rep   Report
	err   error
	calls int
}

func (s *stubFull) Check(context.Context) (Report, error) {
	s.calls++
	return s.rep, s.err
}

func TestTieredVerifier(t *testing.T) {
	fullPass := Report{Passed: true, Output: "full ok"}
	fullRed := Report{Passed: false, Output: "full red"}

	tests := []struct {
		name       string
		full       Report
		fullErr    error
		scoped     func(context.Context) (bool, string, error) // nil = passthrough
		wantPassed bool
		wantMarker bool // Output must start with ScopedRedMarker
		wantFull   int  // expected Full.Check call count
		wantOutput string
	}{
		{
			name:       "scoped red short-circuits with marker; Full never runs",
			full:       fullPass,
			scoped:     func(context.Context) (bool, string, error) { return true, "FAIL\tpkg", nil },
			wantPassed: false,
			wantMarker: true,
			wantFull:   0,
		},
		{
			name:       "scoped green falls through; only Full can PASS",
			full:       fullPass,
			scoped:     func(context.Context) (bool, string, error) { return false, "", nil },
			wantPassed: true,
			wantFull:   1,
			wantOutput: "full ok",
		},
		{
			name:       "scoped green + Full red stays red (scoped never greens anything)",
			full:       fullRed,
			scoped:     func(context.Context) (bool, string, error) { return false, "", nil },
			wantPassed: false,
			wantFull:   1,
			wantOutput: "full red",
		},
		{
			name: "scoped error falls through even when it also claims failed",
			full: fullPass,
			scoped: func(context.Context) (bool, string, error) {
				return true, "half-truth", errors.New("git unavailable")
			},
			wantPassed: true,
			wantFull:   1,
			wantOutput: "full ok",
		},
		{
			name:       "nil ScopedRed is byte-identical passthrough",
			full:       fullPass,
			scoped:     nil,
			wantPassed: true,
			wantFull:   1,
			wantOutput: "full ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			full := &stubFull{rep: tc.full, err: tc.fullErr}
			tv := &TieredVerifier{Full: full, ScopedRed: tc.scoped}
			rep, err := tv.Check(context.Background())
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if rep.Passed != tc.wantPassed {
				t.Fatalf("Passed=%v, want %v (output: %s)", rep.Passed, tc.wantPassed, rep.Output)
			}
			if full.calls != tc.wantFull {
				t.Fatalf("Full called %d times, want %d", full.calls, tc.wantFull)
			}
			if tc.wantMarker {
				if !strings.HasPrefix(rep.Output, ScopedRedMarker+"\n") {
					t.Fatalf("scoped red must be marker-prefixed, got: %q", rep.Output)
				}
			} else if tc.wantOutput != "" && rep.Output != tc.wantOutput {
				t.Fatalf("Output=%q, want %q (Full's verdict must pass through untouched)", rep.Output, tc.wantOutput)
			}
		})
	}
}

// TestTieredVerifierGuards pins the wiring-bug guards: a nil Full errors (with or
// without a scoped seam), and an erroring Full propagates.
func TestTieredVerifierGuards(t *testing.T) {
	if _, err := (&TieredVerifier{}).Check(context.Background()); err == nil {
		t.Fatal("nil Full must error")
	}
	boom := errors.New("boom")
	full := &stubFull{err: boom}
	tv := &TieredVerifier{Full: full, ScopedRed: func(context.Context) (bool, string, error) { return false, "", nil }}
	if _, err := tv.Check(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Full's error must propagate, got %v", err)
	}
}
