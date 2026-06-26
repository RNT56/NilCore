package selfacc

import (
	"context"
	"fmt"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// maxCommandLen bounds a candidate verifier's command so a model-authored string
// can never balloon a sandbox invocation or an event Detail. A real check is a
// short shell line; anything longer is rejected by the meta-check as untrusted.
const maxCommandLen = 2048

// Candidate is an UNTRUSTED, model-authored verifier proposal. The agent, faced
// with a goal that has no acceptance pack, may author one of these to say "here
// is how I would check this criterion". It is inert and untrusted by
// construction: a Candidate is just a name + a sandbox command + the success
// rule, and it can ONLY ever be run as a sandboxed command (I4) — there is no
// field, anywhere, for an in-process host Go func. Until Admit accepts it and a
// wiring layer Registers its CheckFunc, any claim naming it resolves to
// artifact.StatusUnverifiable (never Pass).
type Candidate struct {
	// VerifierID is the namespaced id the criterion's claim would bind to
	// (e.g. "candidate.build_passes"). UNTRUSTED model-authored data.
	VerifierID string
	// Command is the single shell command run INSIDE the sandbox box to decide
	// the criterion. UNTRUSTED: it is validated structurally by Admit and only
	// ever executed through sandbox.Sandbox.Exec — never on the host. A zero exit
	// is read as the criterion holding; a non-zero exit (or a sandbox error, or a
	// nil box) is Unverifiable, never a silent pass.
	Command string
	// Rationale is the agent's prose justification. Pure data for the audit
	// surface — never executed, never templated into Command.
	Rationale string
}

// Admit is the META-CHECK: it decides whether a model-authored Candidate is
// ADMISSIBLE as a sandboxed verifier. It is the I4 gate. A candidate is admitted
// ONLY when it is a bounded sandbox command — never arbitrary host code — and it
// names a non-empty verifier id. Admit explicitly rejects:
//
//   - an empty verifier id (nothing to bind to);
//   - an empty or whitespace-only command (a verifier that asserts nothing must
//     never resolve to a pass — fail-closed);
//   - an over-long command (a model-authored string flooding the invocation);
//   - a command carrying a control byte / NUL (smuggling past the shell line);
//   - any marker that the candidate intends in-process / host execution rather
//     than a sandbox command (the structural I4 refusal).
//
// Admit performs NO execution and reaches NO network — it is a pure structural
// decision over untrusted data. A rejected candidate stays untrusted: it cannot
// be turned into a CheckFunc, so it can never run.
func Admit(c Candidate) error {
	id := strings.TrimSpace(c.VerifierID)
	if id == "" {
		return fmt.Errorf("candidate verifier rejected: empty verifier id")
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("candidate verifier rejected: id %q has whitespace or control characters", id)
		}
	}

	cmd := strings.TrimSpace(c.Command)
	if cmd == "" {
		// A candidate with no command asserts nothing. Admitting it would let a
		// claim resolve to Pass with no work done — exactly the I2 violation this
		// package exists to prevent.
		return fmt.Errorf("candidate verifier %q rejected: empty sandbox command (a verifier must run a check)", id)
	}
	if len(cmd) > maxCommandLen {
		return fmt.Errorf("candidate verifier %q rejected: command exceeds %d bytes", id, maxCommandLen)
	}
	for _, r := range cmd {
		if r == 0x00 {
			return fmt.Errorf("candidate verifier %q rejected: command contains a NUL byte", id)
		}
		// Bare control characters (newline/tab aside) have no place in a single
		// command line and are a classic smuggling vector.
		if r < ' ' && r != '\n' && r != '\t' {
			return fmt.Errorf("candidate verifier %q rejected: command contains a control character", id)
		}
	}
	// The structural I4 refusal: a candidate must be a SANDBOX command. Any hint
	// that it wants in-process / host execution is rejected outright, because the
	// only sanctioned execution path for an untrusted, model-authored verifier is
	// through the sandbox box. There is no host-side fallback to admit.
	for _, marker := range hostExecutionMarkers {
		if strings.Contains(strings.ToLower(cmd), marker) {
			return fmt.Errorf("candidate verifier %q rejected: not a sandbox command (host/in-process marker %q)", id, marker)
		}
	}
	return nil
}

// hostExecutionMarkers are substrings that signal a candidate is asking for
// host-side or in-process execution rather than a sandbox command. They are a
// conservative structural denylist: the safe answer to any ambiguity is to
// reject (the candidate stays untrusted and simply never runs), never to admit.
var hostExecutionMarkers = []string{
	"go:in-process", // an explicit in-process Go-func request
	"host:exec",     // an explicit host-execution request
	"in-process",    // any in-process intent
}

// Admissible reports whether a candidate passes the meta-check, without
// surfacing the reason. Callers that need the rejection reason use Admit.
func Admissible(c Candidate) bool { return Admit(c) == nil }

// CheckFunc converts an ADMITTED candidate into an evverify-shaped check that
// runs the candidate's command inside the sandbox box and returns a trusted
// artifact.Status. It refuses to build a check from an un-admitted candidate
// (returning an error), so an untrusted candidate can never be wrapped into a
// runnable verifier. The returned func has the exact evverify.CheckFunc shape, so
// a wiring layer may Register it under c.VerifierID; this package never registers
// it itself (default-off / additive).
//
// At run time the returned check is fail-closed:
//
//   - a nil box ⇒ StatusUnverifiable (refuse a host-side fallback — I4);
//   - a sandbox error ⇒ StatusUnverifiable (the box could not decide);
//   - a non-zero exit ⇒ StatusUnverifiable (the candidate did not affirm the
//     claim — a candidate verifier may report "not proven", but it is never
//     trusted to assert a Fail; only an affirmative zero exit is a Pass);
//   - a zero exit ⇒ StatusPass (the admitted, sandboxed check affirmed it).
//
// The signature matches evverify.CheckFunc exactly, but this package does NOT
// import evverify for the type — it returns the structural func so the leaf stays
// minimal and the dependency direction (wiring imports both) is preserved.
func CheckFunc(c Candidate) (func(ctx context.Context, box sandbox.Sandbox, claim artifact.Claim) (artifact.Status, string), error) {
	if err := Admit(c); err != nil {
		return nil, fmt.Errorf("cannot build check from un-admitted candidate: %w", err)
	}
	cmd := strings.TrimSpace(c.Command)
	return func(ctx context.Context, box sandbox.Sandbox, _ artifact.Claim) (artifact.Status, string) {
		if box == nil {
			// No sandbox to reach through. Refuse rather than fall back to host
			// execution, which would bypass the sandbox boundary (I4).
			return artifact.StatusUnverifiable, "no sandbox available (refusing host-side execution)"
		}
		res, err := box.Exec(ctx, cmd)
		if err != nil {
			return artifact.StatusUnverifiable, boundedDetail("sandbox: " + err.Error())
		}
		if res.ExitCode != 0 {
			d := strings.TrimSpace(res.Stderr)
			if d == "" {
				d = fmt.Sprintf("candidate verifier exited %d", res.ExitCode)
			}
			return artifact.StatusUnverifiable, boundedDetail(d)
		}
		return artifact.StatusPass, "candidate verifier exited 0"
	}, nil
}

// maxDetail bounds the verifier-authored detail tail so a candidate's output can
// never flood the artifact JSON or an event Detail.
const maxDetail = 512

// boundedDetail trims a harness-authored note to the bounded tail. It is for
// verifier commentary only — never templated as an instruction.
func boundedDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}
