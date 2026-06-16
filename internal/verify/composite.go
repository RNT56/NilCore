package verify

import (
	"context"
	"strings"
)

// Composite runs several verifiers in order and passes only if all pass — the
// project's own checks PLUS, when configured, a behavioral browser check. It
// short-circuits on the first failure (there is no point exercising a running app
// whose build is red), and reports which check failed. It keeps the verifier the
// sole authority on "done" (invariant I2): a behavioral check is one input to the
// verdict, never a self-report and never a bypass.
//
// The zero value (no verifiers) passes — but it is never wired that way; the
// command verifier is always first.
type Composite struct {
	// Named pairs each verifier with a label for the failure report. Order matters:
	// cheapest/most-fundamental first (e.g. "make verify", then "browser").
	Named []NamedVerifier
}

// NamedVerifier is a verifier with a human label for the composite's report.
type NamedVerifier struct {
	Name string
	V    Verifier
}

// Check runs each verifier in order, returning the first failing report (prefixed
// with the failing check's name) or an aggregate pass.
func (c Composite) Check(ctx context.Context) (Report, error) {
	var passed []string
	for _, nv := range c.Named {
		if nv.V == nil {
			continue
		}
		rep, err := nv.V.Check(ctx)
		if err != nil {
			return Report{}, err
		}
		if !rep.Passed {
			return Report{
				Passed: false,
				Output: "check " + nv.Name + " failed:\n" + rep.Output,
			}, nil
		}
		passed = append(passed, nv.Name)
	}
	return Report{Passed: true, Output: "passed: " + strings.Join(passed, ", ")}, nil
}
