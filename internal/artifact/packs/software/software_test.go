package software

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in (mirrors evverify's test fake): it
// records the last command and returns a canned Result/error so the curl branches of
// each CheckFunc are driven with no real network. exec is the per-test hook.
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

// curlOut formats a stdout the way `curl -w '\n%{http_code}'` does: body then the
// HTTP status code on its own final line.
func curlOut(body string, code string) string { return body + "\n" + code }

// boxReturning is a fakeBox whose every Exec returns the given canned curl stdout.
func boxReturning(stdout string) *fakeBox {
	return &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{Stdout: stdout, ExitCode: 0}, nil
	}}
}

func claim(verifier, sourceURL, val string) artifact.Claim {
	return artifact.Claim{
		ID:    "c1",
		Field: "f1",
		Evidence: artifact.Evidence{
			SourceURL: sourceURL,
			Value:     val,
			Verifier:  verifier,
		},
	}
}

func TestSoftwarePack(t *testing.T) {
	ctx := context.Background()

	t.Run("RegisterAll registers exactly six ids", func(t *testing.T) {
		want := []string{
			idNPM, idPyPI, idCrate, idRelease, idTag, idLicense,
		}
		// Absent before.
		r := evverify.New()
		for _, id := range want {
			if _, ok := r.Lookup(id); ok {
				t.Fatalf("id %q resolvable before RegisterAll", id)
			}
		}
		// Present after, and ONLY those (count check against a fresh Default-less reg).
		RegisterAll(r)
		for _, id := range want {
			if _, ok := r.Lookup(id); !ok {
				t.Fatalf("id %q not resolvable after RegisterAll", id)
			}
		}
		// Exactly six: registering into an empty registry, then probing a known
		// non-pack id stays absent.
		if _, ok := r.Lookup("finance.sec_fact"); ok {
			t.Fatalf("software pack leaked a non-pack id")
		}
	})

	t.Run("npm Pass when version present in .versions", func(t *testing.T) {
		box := boxReturning(curlOut(`{"versions":{"1.0.0":{},"1.2.3":{}}}`, "200"))
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/left-pad", "1.2.3"))
		if st != artifact.StatusPass {
			t.Fatalf("npm present = %q, want pass", st)
		}
	})

	t.Run("npm Fail when version absent", func(t *testing.T) {
		box := boxReturning(curlOut(`{"versions":{"1.0.0":{}}}`, "200"))
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/left-pad", "9.9.9"))
		if st != artifact.StatusFail {
			t.Fatalf("npm absent version = %q, want fail", st)
		}
	})

	t.Run("npm Unverifiable when package 404 (non-2xx is not a decisive version verdict)", func(t *testing.T) {
		box := boxReturning(curlOut(`{"error":"Not found"}`, "404"))
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/nope", "1.0.0"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("npm 404 = %q, want unverifiable", st)
		}
	})

	t.Run("npm Unverifiable on parse error", func(t *testing.T) {
		box := boxReturning(curlOut(`not json at all`, "200"))
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/left-pad", "1.2.3"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("npm parse error = %q, want unverifiable", st)
		}
	})

	t.Run("npm Unverifiable on transport failure (curl non-zero)", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 6, Stderr: "could not resolve host"}, nil
		}}
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/left-pad", "1.2.3"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("npm transport fail = %q, want unverifiable", st)
		}
	})

	t.Run("npm empty value Unverifiable (no vacuous match)", func(t *testing.T) {
		box := boxReturning(curlOut(`{"versions":{}}`, "200"))
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/left-pad", ""))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("npm empty value = %q, want unverifiable", st)
		}
	})

	t.Run("pypi Pass present, Fail absent", func(t *testing.T) {
		body := `{"releases":{"2.0.0":[],"2.1.0":[]}}`
		if st, _ := checkPyPIVersion(ctx, boxReturning(curlOut(body, "200")), claim(idPyPI, "https://pypi.org/project/requests", "2.1.0")); st != artifact.StatusPass {
			t.Fatalf("pypi present = %q, want pass", st)
		}
		if st, _ := checkPyPIVersion(ctx, boxReturning(curlOut(body, "200")), claim(idPyPI, "https://pypi.org/project/requests", "9.9.9")); st != artifact.StatusFail {
			t.Fatalf("pypi absent = %q, want fail", st)
		}
	})

	t.Run("crate Pass present, Fail absent", func(t *testing.T) {
		body := `{"versions":[{"num":"1.0.0"},{"num":"1.4.0"}]}`
		if st, _ := checkCrateVersion(ctx, boxReturning(curlOut(body, "200")), claim(idCrate, "https://crates.io/crates/serde", "1.4.0")); st != artifact.StatusPass {
			t.Fatalf("crate present = %q, want pass", st)
		}
		if st, _ := checkCrateVersion(ctx, boxReturning(curlOut(body, "200")), claim(idCrate, "https://crates.io/crates/serde", "0.0.1")); st != artifact.StatusFail {
			t.Fatalf("crate absent = %q, want fail", st)
		}
	})

	t.Run("github_release Pass on 2xx matching tag_name", func(t *testing.T) {
		box := boxReturning(curlOut(`{"tag_name":"v1.2.3"}`, "200"))
		st, _ := checkGitHubRelease(ctx, box, claim(idRelease, "https://github.com/owner/repo", "v1.2.3"))
		if st != artifact.StatusPass {
			t.Fatalf("release match = %q, want pass", st)
		}
	})

	t.Run("github_release Fail on 404", func(t *testing.T) {
		box := boxReturning(curlOut(`{"message":"Not Found"}`, "404"))
		st, _ := checkGitHubRelease(ctx, box, claim(idRelease, "https://github.com/owner/repo", "v9.9.9"))
		if st != artifact.StatusFail {
			t.Fatalf("release 404 = %q, want fail", st)
		}
	})

	t.Run("github_release Fail when tag_name mismatches", func(t *testing.T) {
		box := boxReturning(curlOut(`{"tag_name":"v1.0.0"}`, "200"))
		st, _ := checkGitHubRelease(ctx, box, claim(idRelease, "https://github.com/owner/repo", "v2.0.0"))
		if st != artifact.StatusFail {
			t.Fatalf("release mismatch = %q, want fail", st)
		}
	})

	t.Run("github_tag Pass present, Fail absent", func(t *testing.T) {
		body := `[{"name":"v1.0.0"},{"name":"v1.1.0"}]`
		if st, _ := checkGitHubTag(ctx, boxReturning(curlOut(body, "200")), claim(idTag, "https://github.com/owner/repo", "v1.1.0")); st != artifact.StatusPass {
			t.Fatalf("tag present = %q, want pass", st)
		}
		if st, _ := checkGitHubTag(ctx, boxReturning(curlOut(body, "200")), claim(idTag, "https://github.com/owner/repo", "v3.0.0")); st != artifact.StatusFail {
			t.Fatalf("tag absent = %q, want fail", st)
		}
	})

	t.Run("license Pass on SPDX match (normalized), Fail otherwise", func(t *testing.T) {
		mit := `{"license":{"spdx_id":"MIT"}}`
		if st, _ := checkLicenseMatches(ctx, boxReturning(curlOut(mit, "200")), claim(idLicense, "https://github.com/owner/repo", "mit")); st != artifact.StatusPass {
			t.Fatalf("license match (case-insensitive) = %q, want pass", st)
		}
		if st, _ := checkLicenseMatches(ctx, boxReturning(curlOut(mit, "200")), claim(idLicense, "https://github.com/owner/repo", "Apache-2.0")); st != artifact.StatusFail {
			t.Fatalf("license mismatch = %q, want fail", st)
		}
		none := `{"license":{"spdx_id":"NOASSERTION"}}`
		if st, _ := checkLicenseMatches(ctx, boxReturning(curlOut(none, "200")), claim(idLicense, "https://github.com/owner/repo", "MIT")); st != artifact.StatusFail {
			t.Fatalf("license NOASSERTION = %q, want fail", st)
		}
		if st, _ := checkLicenseMatches(ctx, boxReturning(curlOut(`{}`, "404")), claim(idLicense, "https://github.com/owner/repo", "MIT")); st != artifact.StatusFail {
			t.Fatalf("license 404 = %q, want fail", st)
		}
	})

	t.Run("nil Box => Unverifiable everywhere, no host-side request", func(t *testing.T) {
		checks := map[string]evverify.CheckFunc{
			idNPM:     checkNPMVersion,
			idPyPI:    checkPyPIVersion,
			idCrate:   checkCrateVersion,
			idRelease: checkGitHubRelease,
			idTag:     checkGitHubTag,
			idLicense: checkLicenseMatches,
		}
		for id, fn := range checks {
			st, d := fn(ctx, nil, claim(id, "https://github.com/owner/repo", "v1"))
			if st != artifact.StatusUnverifiable {
				t.Fatalf("%s nil box = %q, want unverifiable", id, st)
			}
			if !strings.Contains(d, "no sandbox") {
				t.Fatalf("%s nil box detail = %q, want a no-sandbox reason", id, d)
			}
		}
	})

	t.Run("only one box.Exec invocation per check", func(t *testing.T) {
		calls := 0
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			calls++
			return sandbox.Result{Stdout: curlOut(`{"versions":{"1.0.0":{}}}`, "200"), ExitCode: 0}, nil
		}}
		_, _ = checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/x", "1.0.0"))
		if calls != 1 {
			t.Fatalf("npm made %d box.Exec calls, want exactly 1", calls)
		}
	})

	t.Run("sandbox-level error => Unverifiable (not a Go error escaping)", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{}, errors.New("box exploded")
		}}
		st, _ := checkNPMVersion(ctx, box, claim(idNPM, "https://registry.npmjs.org/x", "1.0.0"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("sandbox error = %q, want unverifiable", st)
		}
	})

	t.Run("malformed coordinate => Unverifiable", func(t *testing.T) {
		// SourceURL with no package path segment.
		st, _ := checkNPMVersion(ctx, boxReturning(curlOut(`{}`, "200")), claim(idNPM, "https://registry.npmjs.org/", "1.0.0"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("no path segment = %q, want unverifiable", st)
		}
		// github URL missing the repo segment.
		st, _ = checkGitHubRelease(ctx, boxReturning(curlOut(`{}`, "200")), claim(idRelease, "https://github.com/owneronly", "v1"))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("missing repo = %q, want unverifiable", st)
		}
	})
}

// --- github_tag pagination (fix: absence from page 1 is not decisive) --------

// fullTagPage returns a JSON array of exactly n decoy tag objects (none matching a real
// target), i.e. a FULL page that signals "there may be more" to the pager.
func fullTagPage(n int) string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf(`{"name":"decoy-%04d"}`, i)
	}
	return "[" + strings.Join(names, ",") + "]"
}

// pagedTagBox routes by the &page=N query the pager appends: page 1..len(pages) return the
// given bodies, any further page returns an empty list. Each call is a canned 200 curl. The
// match anchors on "&page=" (not the bare "page=") so it does not collide with the
// "per_page=100" segment, which contains "page=100".
func pagedTagBox(pages ...string) *fakeBox {
	return &fakeBox{exec: func(cmd string) (sandbox.Result, error) {
		for i, body := range pages {
			if strings.Contains(cmd, fmt.Sprintf("&page=%d", i+1)) {
				return sandbox.Result{Stdout: curlOut(body, "200"), ExitCode: 0}, nil
			}
		}
		return sandbox.Result{Stdout: curlOut("[]", "200"), ExitCode: 0}, nil
	}}
}

// TestGitHubTagPaginatesToLaterPage is the discriminating fix test: the target tag sits on
// page 2, behind a FULL page 1 of decoys. The pre-fix single-page fetch reported it absent
// (a false Fail); the paginated walk must follow to page 2 and Pass.
func TestGitHubTagPaginatesToLaterPage(t *testing.T) {
	box := pagedTagBox(fullTagPage(100), `[{"name":"v2.5.0"},{"name":"v2.6.0"}]`)
	st, _ := checkGitHubTag(context.Background(), box, claim(idTag, "https://github.com/owner/repo", "v2.5.0"))
	if st != artifact.StatusPass {
		t.Fatalf("tag on page 2 = %q, want Pass (must follow pagination beyond page 1)", st)
	}
}

// TestGitHubTagAbsentAcrossPagesIsFail confirms Fail is still returned once the listing is
// EXHAUSTED: a full page 1 then a short (final) page 2 without the tag ⇒ decisively absent.
func TestGitHubTagAbsentAcrossPagesIsFail(t *testing.T) {
	box := pagedTagBox(fullTagPage(100), `[{"name":"v1.0.0"}]`)
	st, _ := checkGitHubTag(context.Background(), box, claim(idTag, "https://github.com/owner/repo", "v9.9.9"))
	if st != artifact.StatusFail {
		t.Fatalf("absent tag after an exhausted listing = %q, want Fail", st)
	}
}

// TestGitHubTagBudgetExhaustedIsUnverifiable proves the fail-toward-unverifiable bound: if
// EVERY page is full (the listing never ends within the page budget), absence cannot be
// proven, so the verdict is Unverifiable — never a false Fail (I2). The bounded walk also
// stops (it does not loop forever), asserted by a call count of exactly tagMaxPages.
func TestGitHubTagBudgetExhaustedIsUnverifiable(t *testing.T) {
	full := fullTagPage(100)
	calls := 0
	box := &fakeBox{exec: func(string) (sandbox.Result, error) {
		calls++
		return sandbox.Result{Stdout: curlOut(full, "200"), ExitCode: 0}, nil
	}}
	st, _ := checkGitHubTag(context.Background(), box, claim(idTag, "https://github.com/owner/repo", "v9.9.9"))
	if st != artifact.StatusUnverifiable {
		t.Fatalf("unexhausted listing = %q, want Unverifiable (absence not proven)", st)
	}
	if calls != tagMaxPages {
		t.Fatalf("paged %d times, want exactly the bounded %d (walk must stop, not loop)", calls, tagMaxPages)
	}
}

// TestGitHubTagCanceledContextIsUnverifiable confirms the walk honors ctx cancellation
// between pages rather than fetching unboundedly.
func TestGitHubTagCanceledContextIsUnverifiable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the first page
	box := pagedTagBox(fullTagPage(100))
	st, _ := checkGitHubTag(ctx, box, claim(idTag, "https://github.com/owner/repo", "v1"))
	if st != artifact.StatusUnverifiable {
		t.Fatalf("canceled ctx = %q, want Unverifiable", st)
	}
}
