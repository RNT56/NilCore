package evverify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in: it records the last command and
// returns a canned Result/error, so the network branches of a CheckFunc are driven
// without any real network. exec is the hook each test sets.
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

// claimWithURL builds a claim whose SourceURL drives the url_resolves check.
func claimWithURL(verifier, url string) artifact.Claim {
	return artifact.Claim{
		ID:    "c1",
		Field: "f1",
		Evidence: artifact.Evidence{
			SourceURL: url,
			Verifier:  verifier,
		},
	}
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()

	t.Run("Lookup unknown id returns (nil,false)", func(t *testing.T) {
		r := New()
		if fn, ok := r.Lookup("does.not.exist"); ok || fn != nil {
			t.Fatalf("Lookup(unknown) = (%v,%v), want (nil,false)", fn, ok)
		}
	})

	t.Run("Resolve unknown id yields Unverifiable, never Pass", func(t *testing.T) {
		r := New()
		st, d := r.Resolve(ctx, &fakeBox{}, claimWithURL("nope.nope", "https://example.com"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("Resolve(unregistered) status = %q, want unverifiable", st)
		}
		if st == artifact.StatusPass {
			t.Fatal("unregistered id must never Pass")
		}
		if !strings.Contains(d, "unregistered") {
			t.Fatalf("detail %q should explain the unregistered id", d)
		}
	})

	t.Run("Resolve empty verifier id yields Unverifiable", func(t *testing.T) {
		r := Default()
		st, _ := r.Resolve(ctx, &fakeBox{}, claimWithURL("", "https://example.com"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("Resolve(no verifier) status = %q, want unverifiable", st)
		}
	})

	t.Run("Register then Lookup resolves", func(t *testing.T) {
		r := New()
		if _, ok := r.Lookup("x.custom"); ok {
			t.Fatal("id present before Register")
		}
		r.Register("x.custom", func(context.Context, sandbox.Sandbox, artifact.Claim) (artifact.Status, string) {
			return artifact.StatusPass, "ok"
		})
		fn, ok := r.Lookup("x.custom")
		if !ok || fn == nil {
			t.Fatal("id absent after Register")
		}
		st, _ := fn(ctx, &fakeBox{}, artifact.Claim{})
		if st != artifact.StatusPass {
			t.Fatalf("registered fn status = %q, want pass", st)
		}
	})

	t.Run("Register rejects empty id and nil fn", func(t *testing.T) {
		r := New()
		r.Register("", func(context.Context, sandbox.Sandbox, artifact.Claim) (artifact.Status, string) {
			return artifact.StatusPass, ""
		})
		r.Register("x.nilfn", nil)
		if _, ok := r.Lookup(""); ok {
			t.Fatal("empty id should not register")
		}
		if _, ok := r.Lookup("x.nilfn"); ok {
			t.Fatal("nil fn should not register")
		}
	})

	t.Run("Default registers web.url_resolves and no always-pass verifier", func(t *testing.T) {
		r := Default()
		if _, ok := r.Lookup("web.url_resolves"); !ok {
			t.Fatal("Default missing web.url_resolves")
		}
		// Default must register ONLY web.url_resolves — no noop/always-pass id slips in.
		if len(r.checks) != 1 {
			ids := make([]string, 0, len(r.checks))
			for id := range r.checks {
				ids = append(ids, id)
			}
			t.Fatalf("Default registered %d checks (%v), want exactly [web.url_resolves]", len(r.checks), ids)
		}
		// Every registered check must be a real assertion: feed it a box whose only
		// behavior is "the network is unreachable" and verify NONE returns Pass — an
		// always-pass verifier would Pass here.
		unreachable := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 7, Stderr: "could not resolve host"}, nil
		}}
		for id, fn := range r.checks {
			st, _ := fn(ctx, unreachable, claimWithURL(id, "https://unreachable.invalid"))
			if st == artifact.StatusPass {
				t.Fatalf("check %q PASSed against an unreachable host — always-pass verifier", id)
			}
		}
	})
}

// TestResolveStrengthBinding is the I2 "hollow green" gate: a claim that ASSERTS a Value
// must be bound to a verifier that actually checks that Value. A value-blind verifier
// (url_resolves / not_stale) or a box-free self-consistency check (variance_bounded) bound
// to a value-bearing claim fails closed BEFORE the check runs — while a value-LESS claim
// keeps the legitimate provenance use.
func TestResolveStrengthBinding(t *testing.T) {
	ctx := context.Background()

	t.Run("value-bearing url_resolves fails closed (never a hollow green), box never reached", func(t *testing.T) {
		r := Default() // registers web.url_resolves → checkURLResolves
		// A box that WOULD pass url_resolves (curl -f exits 0 on a 2xx) — so without the gate
		// this greens. The fabricated Value is never inspected by url_resolves.
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0}, nil
		}}
		c := claimWithURL("web.url_resolves", "https://example.com/report")
		c.Evidence.Value = "GDP grew 3.2% in Q2" // a value nothing here verifies
		st, d := r.Resolve(ctx, box, c)
		if st == artifact.StatusPass {
			t.Fatal("a value-bearing claim on a value-blind verifier must NOT Pass (hollow green)")
		}
		if st != artifact.StatusUnverifiable {
			t.Fatalf("status = %q, want unverifiable", st)
		}
		if box.lastCmd != "" {
			t.Fatalf("the gate must fire BEFORE the check runs; box saw %q", box.lastCmd)
		}
		if !strings.Contains(d, "value-blind") {
			t.Fatalf("detail %q should explain the value-blind refusal", d)
		}
	})

	t.Run("value-LESS url_resolves is still allowed on a 2xx (legit use preserved)", func(t *testing.T) {
		r := Default()
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0}, nil
		}}
		// claimWithURL leaves Value empty — a pure "this URL resolves" assertion.
		st, _ := r.Resolve(ctx, box, claimWithURL("web.url_resolves", "https://example.com"))
		if st != artifact.StatusPass {
			t.Fatalf("a value-less url_resolves claim on a 2xx must Pass; got %q", st)
		}
	})

	t.Run("standalone variance_bounded is refused for a value-bearing claim, check never runs", func(t *testing.T) {
		// Register a variance_bounded stub that WOULD Pass, to prove the gate fails closed
		// before the box-free self-consistency check ever runs (it re-measures nothing, so it
		// can never green a value-bearing claim standalone).
		r := New()
		ran := false
		r.Register("benchmark.variance_bounded", func(context.Context, sandbox.Sandbox, artifact.Claim) (artifact.Status, string) {
			ran = true
			return artifact.StatusPass, "cv within ceiling"
		})
		c := claimWithURL("benchmark.variance_bounded", "")
		c.Evidence.Value = `{"cv_max":0.05,"samples":[1,1,1]}`
		st, _ := r.Resolve(ctx, &fakeBox{}, c)
		if st != artifact.StatusUnverifiable {
			t.Fatalf("standalone variance_bounded on a value-bearing claim must be Unverifiable; got %q", st)
		}
		if ran {
			t.Fatal("the box-free check must NOT run — the gate fails closed before it")
		}
	})
}

func TestRegistryURLResolves(t *testing.T) {
	ctx := context.Background()

	t.Run("HTTP 2xx => Pass", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0}, nil // curl -f exits 0 on a 2xx
		}}
		st, _ := checkURLResolves(ctx, box, claimWithURL("web.url_resolves", "https://example.com/ok"))
		if st != artifact.StatusPass {
			t.Fatalf("2xx status = %q, want pass", st)
		}
		if !strings.Contains(box.lastCmd, "curl") || !strings.Contains(box.lastCmd, "'https://example.com/ok'") {
			t.Fatalf("cmd %q should curl the single-quoted URL", box.lastCmd)
		}
	})

	t.Run("non-2xx / unreachable => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 22, Stderr: "HTTP 404"}, nil // curl -f exits 22 on >=400
		}}
		st, d := checkURLResolves(ctx, box, claimWithURL("web.url_resolves", "https://example.com/missing"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("non-2xx status = %q, want unverifiable", st)
		}
		if st == artifact.StatusPass {
			t.Fatal("a non-2xx must never Pass")
		}
		if d == "" {
			t.Fatal("expected a detail tail for the failure")
		}
	})

	t.Run("nil Box => Unverifiable, no host-side request", func(t *testing.T) {
		st, d := checkURLResolves(ctx, nil, claimWithURL("web.url_resolves", "https://example.com"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("nil box status = %q, want unverifiable", st)
		}
		if !strings.Contains(strings.ToLower(d), "no sandbox") {
			t.Fatalf("nil box detail %q should explain the missing sandbox", d)
		}
	})

	t.Run("invalid URL => Unverifiable, box never reached", func(t *testing.T) {
		reached := false
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			reached = true
			return sandbox.Result{}, nil
		}}
		for _, bad := range []string{"", "ftp://example.com", "not a url", "https://example.com/'; rm -rf /"} {
			st, _ := checkURLResolves(ctx, box, claimWithURL("web.url_resolves", bad))
			if st != artifact.StatusUnverifiable {
				t.Fatalf("bad url %q status = %q, want unverifiable", bad, st)
			}
		}
		if reached {
			t.Fatal("an invalid URL must not reach the box")
		}
	})

	t.Run("sandbox error => Unverifiable, never Pass", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("box exploded")
		}}
		st, _ := checkURLResolves(ctx, box, claimWithURL("web.url_resolves", "https://example.com"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("sandbox-error status = %q, want unverifiable", st)
		}
	})
}
