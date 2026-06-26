package selfacc

import (
	"context"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// Resolve runs a proposed claim against the evverify Registry and returns the
// trusted Status. It exists to make ONE rule impossible to get wrong from a
// caller: a claim bound to a candidate verifier that has NOT been admitted and
// registered is StatusUnverifiable — NEVER StatusPass. It delegates entirely to
// evverify.Registry.Resolve, which already centralizes the unregistered-id ⇒
// Unverifiable rule; selfacc adds no permissive path of its own.
//
// A nil registry is fail-closed: with no registry there is no admitted verifier,
// so nothing can be asserted and the claim is Unverifiable. This is the I2/I4
// guarantee for the self-acceptance flow — an agent-proposed criterion only ever
// becomes green by passing a real, admitted, SANDBOXED check.
func Resolve(ctx context.Context, reg *evverify.Registry, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	if reg == nil {
		return artifact.StatusUnverifiable, "no verifier registry (refusing to assert without an admitted verifier)"
	}
	return reg.Resolve(ctx, box, c)
}

// Register binds an ADMITTED candidate's sandboxed check into the supplied
// registry under the candidate's verifier id, returning the bound id. It refuses
// an un-admitted candidate (the meta-check runs again here as defense-in-depth),
// so an untrusted candidate can never reach the registry. This is the ONLY path
// by which a self-authored verifier becomes runnable, and it is explicit: the
// caller (an operator-controlled wiring layer) chooses to call it — this package
// never auto-registers anything (default-off / additive).
//
// reg must be non-nil. The check it installs runs only inside the sandbox box
// (see CheckFunc), never on the host (I4).
func Register(reg *evverify.Registry, c Candidate) (string, error) {
	fn, err := CheckFunc(c)
	if err != nil {
		return "", err
	}
	reg.Register(c.VerifierID, evverify.CheckFunc(fn))
	return c.VerifierID, nil
}
