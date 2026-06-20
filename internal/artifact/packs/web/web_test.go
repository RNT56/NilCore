package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in (no network): it records the last
// command and returns a canned Result/error keyed off the command, so each curl
// branch of a check is driven deterministically.
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

func claim(verifier, srcURL, value string) artifact.Claim {
	return artifact.Claim{
		ID:    "c1",
		Field: "f1",
		Evidence: artifact.Evidence{
			SourceURL: srcURL,
			Value:     value,
			Verifier:  verifier,
		},
	}
}

func TestWebPack(t *testing.T) {
	ctx := context.Background()

	t.Run("RegisterAll makes exactly the four ids resolvable", func(t *testing.T) {
		r := evverify.New()
		ids := []string{IDURLResolves, IDQuoteExists, IDDateMatches, IDNotStale}
		for _, id := range ids {
			if _, ok := r.Lookup(id); ok {
				t.Fatalf("id %q present before RegisterAll", id)
			}
		}
		RegisterAll(r)
		for _, id := range ids {
			if _, ok := r.Lookup(id); !ok {
				t.Fatalf("id %q absent after RegisterAll", id)
			}
		}
		// And nothing this pack does not own leaked in.
		if _, ok := r.Lookup("web.something_else"); ok {
			t.Fatal("RegisterAll registered an unexpected id")
		}
	})

	t.Run("url_resolves: 2xx => Pass", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0}, nil
		}}
		st, _ := checkURLResolves(ctx, box, claim(IDURLResolves, "https://example.com/ok", ""))
		if st != artifact.StatusPass {
			t.Fatalf("2xx status = %q, want pass", st)
		}
		if !strings.Contains(box.lastCmd, "curl") || !strings.Contains(box.lastCmd, "'https://example.com/ok'") {
			t.Fatalf("cmd %q should curl the single-quoted URL", box.lastCmd)
		}
	})

	t.Run("url_resolves: non-2xx (exit 22) => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 22, Stderr: "HTTP 404"}, nil
		}}
		st, d := checkURLResolves(ctx, box, claim(IDURLResolves, "https://example.com/missing", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("non-2xx status = %q, want unverifiable", st)
		}
		if st == artifact.StatusPass || d == "" {
			t.Fatalf("a non-2xx must never Pass and must carry a detail (status=%q detail=%q)", st, d)
		}
	})

	t.Run("quote_exists: present => Pass, absent => Fail", func(t *testing.T) {
		// A body that reflows the quote with extra whitespace: normalization must match.
		body := "<p>The   quick\n brown   fox</p>"
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0, Stdout: body}, nil
		}}

		st, _ := checkQuoteExists(ctx, box, claim(IDQuoteExists, "https://example.com", "quick brown fox"))
		if st != artifact.StatusPass {
			t.Fatalf("present quote status = %q, want pass", st)
		}

		st, _ = checkQuoteExists(ctx, box, claim(IDQuoteExists, "https://example.com", "lazy dog"))
		if st != artifact.StatusFail {
			t.Fatalf("absent quote status = %q, want fail", st)
		}
	})

	t.Run("quote_exists: empty value => Unverifiable, box never reached", func(t *testing.T) {
		reached := false
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			reached = true
			return sandbox.Result{ExitCode: 0}, nil
		}}
		st, _ := checkQuoteExists(ctx, box, claim(IDQuoteExists, "https://example.com", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("empty value status = %q, want unverifiable", st)
		}
		if reached {
			t.Fatal("an empty value must not reach the box")
		}
	})

	t.Run("quote_exists: non-2xx body => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 22, Stderr: "HTTP 500"}, nil
		}}
		st, _ := checkQuoteExists(ctx, box, claim(IDQuoteExists, "https://example.com", "anything"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("unreachable-body status = %q, want unverifiable", st)
		}
	})

	t.Run("date_matches: present => Pass, absent => Fail", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0, Stdout: "Published on 2024-06-01 by the desk"}, nil
		}}
		st, _ := checkDateMatches(ctx, box, claim(IDDateMatches, "https://example.com", "2024-06-01"))
		if st != artifact.StatusPass {
			t.Fatalf("present date status = %q, want pass", st)
		}
		st, _ = checkDateMatches(ctx, box, claim(IDDateMatches, "https://example.com", "1999-12-31"))
		if st != artifact.StatusFail {
			t.Fatalf("absent date status = %q, want fail", st)
		}
	})

	t.Run("not_stale: fresh server header => Pass", func(t *testing.T) {
		fresh := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC1123)
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0, Stdout: "HTTP/2 200\r\nLast-Modified: " + fresh + "\r\nContent-Type: text/html\r\n"}, nil
		}}
		st, _ := checkNotStale(ctx, box, claim(IDNotStale, "https://example.com", ""))
		if st != artifact.StatusPass {
			t.Fatalf("fresh source status = %q, want pass", st)
		}
		// It must be a HEAD probe (the -I flag is bundled into -fsSLI).
		if !strings.Contains(box.lastCmd, "-fsSLI") {
			t.Fatalf("not_stale cmd %q should be a HEAD request", box.lastCmd)
		}
	})

	t.Run("not_stale: old server header => Stale", func(t *testing.T) {
		old := time.Now().Add(-365 * 24 * time.Hour).UTC().Format(time.RFC1123)
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0, Stdout: "HTTP/1.1 200 OK\r\nLast-Modified: " + old + "\r\n"}, nil
		}}
		st, _ := checkNotStale(ctx, box, claim(IDNotStale, "https://example.com", ""))
		if st != artifact.StatusStale {
			t.Fatalf("old source status = %q, want stale", st)
		}
	})

	t.Run("not_stale: ignores model RetrievedAt over an unreachable source", func(t *testing.T) {
		// The model fabricates a fresh RetrievedAt=now, but the source is unreachable
		// (curl non-zero). The check MUST NOT Pass on the model's timestamp.
		c := claim(IDNotStale, "https://example.com", "")
		c.Evidence.RetrievedAt = time.Now()
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 6, Stderr: "could not resolve host"}, nil
		}}
		st, _ := checkNotStale(ctx, box, c)
		if st == artifact.StatusPass {
			t.Fatal("not_stale must never Pass on a model-authored RetrievedAt over an unreachable source")
		}
		if st != artifact.StatusUnverifiable {
			t.Fatalf("unreachable source status = %q, want unverifiable", st)
		}
	})

	t.Run("not_stale: resolves but no freshness header => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 0, Stdout: "HTTP/2 200\r\nContent-Type: text/html\r\n"}, nil
		}}
		st, _ := checkNotStale(ctx, box, claim(IDNotStale, "https://example.com", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("no-header status = %q, want unverifiable", st)
		}
	})

	t.Run("nil Box => Unverifiable on every check, no host-side request", func(t *testing.T) {
		c := claim("", "https://example.com", "needle")
		for name, fn := range map[string]evverify.CheckFunc{
			IDURLResolves: checkURLResolves,
			IDQuoteExists: checkQuoteExists,
			IDDateMatches: checkDateMatches,
			IDNotStale:    checkNotStale,
		} {
			st, _ := fn(ctx, nil, c)
			if st != artifact.StatusUnverifiable {
				t.Fatalf("%s with nil box = %q, want unverifiable", name, st)
			}
			if st == artifact.StatusPass {
				t.Fatalf("%s must never Pass with a nil box", name)
			}
		}
	})

	t.Run("invalid / injection URL => Unverifiable, box never reached", func(t *testing.T) {
		reached := false
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			reached = true
			return sandbox.Result{}, nil
		}}
		bad := []string{"", "ftp://example.com", "not a url", "https://example.com/'; rm -rf /"}
		for _, u := range bad {
			for _, fn := range []evverify.CheckFunc{checkURLResolves, checkQuoteExists, checkDateMatches, checkNotStale} {
				st, _ := fn(ctx, box, claim("", u, "needle"))
				if st != artifact.StatusUnverifiable {
					t.Fatalf("bad url %q status = %q, want unverifiable", u, st)
				}
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
		for _, fn := range []evverify.CheckFunc{checkURLResolves, checkQuoteExists, checkDateMatches, checkNotStale} {
			st, _ := fn(ctx, box, claim("", "https://example.com", "needle"))
			if st != artifact.StatusUnverifiable {
				t.Fatalf("sandbox-error status = %q, want unverifiable", st)
			}
		}
	})
}

// TestWebPackThroughRegistry exercises the pack the way the ArtifactVerifier will:
// resolve by id through a registry RegisterAll populated, proving the binding works
// end-to-end (and that an unregistered web id stays Unverifiable).
func TestWebPackThroughRegistry(t *testing.T) {
	ctx := context.Background()
	r := evverify.New()
	RegisterAll(r)

	box := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 0}, nil
	}}
	st, _ := r.Resolve(ctx, box, claim(IDURLResolves, "https://example.com", ""))
	if st != artifact.StatusPass {
		t.Fatalf("resolve url_resolves status = %q, want pass", st)
	}

	st, _ = r.Resolve(ctx, box, claim("web.not_registered", "https://example.com", ""))
	if st != artifact.StatusUnverifiable {
		t.Fatalf("unregistered web id status = %q, want unverifiable", st)
	}
}
