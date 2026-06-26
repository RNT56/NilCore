package selfacc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/planner"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in: it returns a canned
// Result/error so a candidate verifier's exec branch is driven with no real
// sandbox and no network. exec is the hook each test sets.
type fakeBox struct {
	lastCmd string
	exec    func(cmd string) (sandbox.Result, error)
}

func (b *fakeBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.lastCmd = cmd
	if b.exec != nil {
		return b.exec(cmd)
	}
	return sandbox.Result{}, nil
}
func (b *fakeBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *fakeBox) Workdir() string { return "/work" }

// --- Propose -----------------------------------------------------------------

func TestProposeLiftsPlanAcceptanceIntoCriteria(t *testing.T) {
	tree := &planner.Tree{
		Goal: "ship widget",
		Tasks: []planner.PlanTask{
			{ID: "t1", Goal: "build", Acceptance: "make build passes"},
			{ID: "t2", Goal: "test", Acceptance: "go test ./... is green"},
			{ID: "t3", Goal: "no-acc", Acceptance: ""}, // contributes nothing
		},
	}
	p := Propose("ship widget", tree)
	if p.Goal != "ship widget" {
		t.Fatalf("goal = %q, want %q", p.Goal, "ship widget")
	}
	if len(p.Criteria) != 2 {
		t.Fatalf("got %d criteria, want 2 (the empty-acceptance task is skipped): %+v", len(p.Criteria), p.Criteria)
	}
	if p.Criteria[0].Field != "t1" || p.Criteria[0].Statement != "make build passes" {
		t.Errorf("criterion[0] = %+v, want field t1 / 'make build passes'", p.Criteria[0])
	}
	// A freshly proposed criterion binds no verifier — proposing is not asserting.
	for _, c := range p.Criteria {
		if c.Verifier != "" {
			t.Errorf("proposed criterion must bind no verifier, got %q", c.Verifier)
		}
	}
}

func TestProposeNilTreeIsHonestEmpty(t *testing.T) {
	p := Propose("  vague goal  ", nil)
	if p.Goal != "vague goal" {
		t.Errorf("goal not trimmed: %q", p.Goal)
	}
	if len(p.Criteria) != 0 {
		t.Errorf("nil tree must yield no criteria, got %d", len(p.Criteria))
	}
}

func TestProposeDedupesIdenticalCriteria(t *testing.T) {
	tree := &planner.Tree{
		Tasks: []planner.PlanTask{
			{ID: "t1", Goal: "g", Acceptance: "same"},
			{ID: "t1", Goal: "g", Acceptance: "same"}, // duplicate id+stmt
		},
	}
	p := Propose("g", tree)
	if len(p.Criteria) != 1 {
		t.Fatalf("duplicate criteria not deduped: got %d", len(p.Criteria))
	}
}

func TestProposalClaimsStartUnverified(t *testing.T) {
	p := Proposal{Criteria: []AcceptanceCriterion{{Field: "f", Statement: "s"}}}
	claims := p.Claims()
	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1", len(claims))
	}
	if claims[0].Evidence.Status != artifact.StatusUnverified {
		t.Errorf("claim status = %q, want unverified (nothing has run)", claims[0].Evidence.Status)
	}
	if claims[0].Statement != "s" {
		t.Errorf("claim statement = %q, want untrusted prose carried as data", claims[0].Statement)
	}
}

// --- Admit (the meta-check) ---------------------------------------------------

func TestAdmitRejectsNonSandboxAndUnboundedCandidates(t *testing.T) {
	tests := []struct {
		name string
		c    Candidate
	}{
		{"empty id", Candidate{Command: "true"}},
		{"empty command", Candidate{VerifierID: "candidate.x"}},
		{"whitespace command", Candidate{VerifierID: "candidate.x", Command: "   "}},
		{"in-process marker", Candidate{VerifierID: "candidate.x", Command: "run in-process go check"}},
		{"host-exec marker", Candidate{VerifierID: "candidate.x", Command: "host:exec /bin/sh"}},
		{"go in-process marker", Candidate{VerifierID: "candidate.x", Command: "go:in-process Verify()"}},
		{"NUL byte", Candidate{VerifierID: "candidate.x", Command: "echo a\x00b"}},
		{"control byte", Candidate{VerifierID: "candidate.x", Command: "echo \x07"}},
		{"id with space", Candidate{VerifierID: "candidate x", Command: "true"}},
		{"oversized command", Candidate{VerifierID: "candidate.x", Command: strings.Repeat("a", maxCommandLen+1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Admit(tc.c); err == nil {
				t.Fatalf("Admit accepted an inadmissible candidate %+v", tc.c)
			}
			if Admissible(tc.c) {
				t.Errorf("Admissible reported true for inadmissible candidate %+v", tc.c)
			}
			// An un-admitted candidate must not be buildable into a runnable check.
			if _, err := CheckFunc(tc.c); err == nil {
				t.Errorf("CheckFunc built a check from an un-admitted candidate %+v", tc.c)
			}
		})
	}
}

func TestAdmitAcceptsBoundedSandboxCommand(t *testing.T) {
	c := Candidate{VerifierID: "candidate.build_passes", Command: "make build", Rationale: "build must compile"}
	if err := Admit(c); err != nil {
		t.Fatalf("Admit rejected a valid bounded sandbox command: %v", err)
	}
	if !Admissible(c) {
		t.Errorf("Admissible = false for a valid candidate")
	}
}

// --- The fail-closed I2/I4 guarantee -----------------------------------------

// An UNPROVEN (un-registered) candidate verifier resolves to Unverifiable, never
// Pass — even when the underlying box would succeed. This is the heart of the
// self-acceptance safety property.
func TestUnprovenCandidateResolvesUnverifiableNeverPass(t *testing.T) {
	reg := evverify.New() // nothing registered: no candidate has been proven/admitted
	box := &fakeBox{exec: func(string) (sandbox.Result, error) {
		// Even a "successful" box must not produce a Pass for an unbound id.
		return sandbox.Result{ExitCode: 0}, nil
	}}
	claim := artifact.Claim{
		ID:    "acc-1",
		Field: "build_passes",
		Evidence: artifact.Evidence{
			Verifier: "candidate.build_passes", // proposed but never registered
			Status:   artifact.StatusUnverified,
		},
	}
	status, detail := Resolve(context.Background(), reg, box, claim)
	if status == artifact.StatusPass {
		t.Fatalf("unproven candidate resolved to Pass — I2 violation")
	}
	if status != artifact.StatusUnverifiable {
		t.Fatalf("status = %q, want unverifiable", status)
	}
	if !strings.Contains(detail, "unregistered") {
		t.Errorf("detail = %q, want it to name the unregistered id", detail)
	}
}

func TestResolveNilRegistryFailsClosed(t *testing.T) {
	status, _ := Resolve(context.Background(), nil, &fakeBox{}, artifact.Claim{
		Evidence: artifact.Evidence{Verifier: "candidate.x"},
	})
	if status != artifact.StatusUnverifiable {
		t.Errorf("nil registry must fail closed to unverifiable, got %q", status)
	}
}

// --- The admitted, sandboxed check at run time -------------------------------

func TestAdmittedCandidateRunsOnlyInSandbox(t *testing.T) {
	c := Candidate{VerifierID: "candidate.ok", Command: "make test"}
	fn, err := CheckFunc(c)
	if err != nil {
		t.Fatalf("CheckFunc on admitted candidate: %v", err)
	}

	t.Run("nil box fails closed (no host fallback, I4)", func(t *testing.T) {
		status, detail := fn(context.Background(), nil, artifact.Claim{})
		if status != artifact.StatusUnverifiable {
			t.Fatalf("nil box status = %q, want unverifiable", status)
		}
		if !strings.Contains(detail, "no sandbox") {
			t.Errorf("detail = %q, want it to refuse a host-side fallback", detail)
		}
	})

	t.Run("zero exit is the only Pass", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0}, nil
		}}
		status, _ := fn(context.Background(), box, artifact.Claim{})
		if status != artifact.StatusPass {
			t.Fatalf("zero exit status = %q, want pass", status)
		}
		if box.lastCmd != "make test" {
			t.Errorf("command run in box = %q, want the candidate command", box.lastCmd)
		}
	})

	t.Run("non-zero exit is Unverifiable, never a trusted Fail", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 2, Stderr: "boom"}, nil
		}}
		status, detail := fn(context.Background(), box, artifact.Claim{})
		if status != artifact.StatusUnverifiable {
			t.Fatalf("non-zero exit status = %q, want unverifiable", status)
		}
		if !strings.Contains(detail, "boom") {
			t.Errorf("detail = %q, want the stderr tail", detail)
		}
	})

	t.Run("sandbox error fails closed", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("box down")
		}}
		status, detail := fn(context.Background(), box, artifact.Claim{})
		if status != artifact.StatusUnverifiable {
			t.Fatalf("sandbox error status = %q, want unverifiable", status)
		}
		if !strings.Contains(detail, "box down") {
			t.Errorf("detail = %q, want the sandbox error tail", detail)
		}
	})
}

// Once a candidate is admitted AND registered, a claim binding it resolves
// through the real evverify seam — and only then can it pass, only on a zero
// exit, only through the sandbox box.
func TestRegisterThenResolveProvesTheCandidate(t *testing.T) {
	reg := evverify.New()
	c := Candidate{VerifierID: "candidate.proven", Command: "make verify"}
	id, err := Register(reg, c)
	if err != nil {
		t.Fatalf("Register admitted candidate: %v", err)
	}
	if id != "candidate.proven" {
		t.Fatalf("registered id = %q", id)
	}

	claim := artifact.Claim{
		Field:    "f",
		Evidence: artifact.Evidence{Verifier: "candidate.proven"},
	}

	passBox := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 0}, nil
	}}
	if status, _ := Resolve(context.Background(), reg, passBox, claim); status != artifact.StatusPass {
		t.Errorf("proven candidate on zero exit = %q, want pass", status)
	}

	failBox := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 1}, nil
	}}
	if status, _ := Resolve(context.Background(), reg, failBox, claim); status != artifact.StatusUnverifiable {
		t.Errorf("proven candidate on non-zero exit = %q, want unverifiable", status)
	}

	// And it still refuses to run host-side when there is no box.
	if status, _ := Resolve(context.Background(), reg, nil, claim); status != artifact.StatusUnverifiable {
		t.Errorf("proven candidate with nil box = %q, want unverifiable (no host fallback)", status)
	}
}

func TestRegisterRejectsUnadmittedCandidate(t *testing.T) {
	reg := evverify.New()
	if _, err := Register(reg, Candidate{VerifierID: "candidate.x", Command: "in-process hack"}); err == nil {
		t.Fatalf("Register accepted an un-admitted candidate")
	}
	// Nothing was registered, so resolving still fails closed.
	claim := artifact.Claim{Evidence: artifact.Evidence{Verifier: "candidate.x"}}
	if status, _ := Resolve(context.Background(), reg, &fakeBox{}, claim); status != artifact.StatusUnverifiable {
		t.Errorf("status after rejected register = %q, want unverifiable", status)
	}
}
