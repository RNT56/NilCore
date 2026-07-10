// checks.go holds the six software-research CheckFuncs. Each derives its registry/API
// coordinates from the claim's key-free SourceURL (the single canonical provenance),
// fetches ONE document via curl-in-box, and parses it host-side as trusted Go. The
// asserted datum is always Evidence.Value (a version string, a tag, an SPDX id).
//
// Verdict discipline shared by all six:
//   - the coordinate or the value missing/malformed  => Unverifiable (nothing to assert)
//   - the fetch unreachable / non-JSON / parse error  => Unverifiable
//   - a definitive "does not exist" (e.g. version not present, release 404) => Fail
//   - the asserted datum present and matching          => Pass
//
// Fail vs Unverifiable matters for requeue routing (Pillar 4): Fail re-derives the
// value, Unverifiable fixes the source/binding.

package software

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// --- coordinate extraction (from the key-free SourceURL) ---------------------

// pkgName returns the last non-empty path segment of the SourceURL — the package
// name on a registry URL (registry.npmjs.org/<pkg>, pypi.org/project/<pkg>,
// crates.io/crates/<pkg>). It URL-unescapes so a scoped npm name (@scope%2fname) is
// handled, and validates the result carries only registry-safe characters.
func pkgName(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("invalid source_url: %w", err)
	}
	segs := splitPath(u.Path)
	if len(segs) == 0 {
		return "", fmt.Errorf("source_url has no package path segment")
	}
	name, err := url.PathUnescape(segs[len(segs)-1])
	if err != nil {
		return "", fmt.Errorf("undecodable package name: %w", err)
	}
	return validateName(name)
}

// ownerRepo extracts <owner>/<repo> from a GitHub-style SourceURL: it takes the
// first two path segments (github.com/<owner>/<repo>/...), trimming a trailing
// ".git". Both segments are validated.
func ownerRepo(rawURL string) (owner, repo string, err error) {
	u, perr := url.Parse(strings.TrimSpace(rawURL))
	if perr != nil {
		return "", "", fmt.Errorf("invalid source_url: %w", perr)
	}
	segs := splitPath(u.Path)
	if len(segs) < 2 {
		return "", "", fmt.Errorf("source_url has no <owner>/<repo> path")
	}
	owner, err = validateName(segs[0])
	if err != nil {
		return "", "", err
	}
	repo = strings.TrimSuffix(segs[1], ".git")
	repo, err = validateName(repo)
	if err != nil {
		return "", "", err
	}
	return owner, repo, nil
}

// splitPath splits a URL path into non-empty segments.
func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// validateName constrains a derived coordinate (package/owner/repo/scope) to a
// conservative charset before it is interpolated into a canonical registry URL —
// belt-and-suspenders since the final URL is re-validated by validateURL, but it
// keeps a malformed coordinate from ever forming a surprising URL.
func validateName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty coordinate")
	}
	for _, r := range s {
		ok := r == '-' || r == '_' || r == '.' || r == '@' || r == '/' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "", fmt.Errorf("coordinate %q has a disallowed character", s)
		}
	}
	return s, nil
}

// value returns the trimmed asserted datum, or an error if empty (an empty Value is
// not a real check — it would otherwise vacuously match).
func value(c artifact.Claim) (string, error) {
	v := strings.TrimSpace(c.Evidence.Value)
	if v == "" {
		return "", fmt.Errorf("evidence.value (the asserted datum) is required")
	}
	return v, nil
}

// --- npm ---------------------------------------------------------------------

// checkNPMVersion asserts Evidence.Value is a published version of the package named
// by the SourceURL. It curls the registry metadoc and checks Value is a key of
// .versions. The asserted unit is the VERSION: a present version => Pass, a fetched
// package missing that version => Fail. A non-2xx (the package itself could not be
// fetched) or a parse error is NOT a decisive verdict about the version => Unverifiable
// (per the spec: "Fail if absent, Unverifiable on non-2xx/parse error").
func checkNPMVersion(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	name, err := pkgName(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	ver, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	// registry.npmjs.org/<name>; a scoped name keeps its '@' and '/' which url.Parse
	// already accepts in a path. PathEscape the '/' inside a scope is unnecessary —
	// the registry accepts the literal scoped name.
	u := "https://registry.npmjs.org/" + name
	code, body, reason, ok := fetchJSON(ctx, box, u)
	if !ok {
		return artifact.StatusUnverifiable, detail(reason)
	}
	if !is2xx(code) {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("npm package %q not fetchable (HTTP %d)", name, code))
	}
	var meta struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		return artifact.StatusUnverifiable, detail("npm metadata parse error: " + err.Error())
	}
	if _, present := meta.Versions[ver]; present {
		return artifact.StatusPass, fmt.Sprintf("npm %s@%s present in .versions", name, ver)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("npm %s has no version %q", name, ver))
}

// --- pypi --------------------------------------------------------------------

// checkPyPIVersion asserts Evidence.Value is a release of the PyPI project named by
// the SourceURL, via pypi.org/pypi/<name>/json and the .releases keys. Present =>
// Pass; fetched project missing the release => Fail; non-2xx/parse error =>
// Unverifiable.
func checkPyPIVersion(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	name, err := pkgName(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	ver, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	u := "https://pypi.org/pypi/" + name + "/json"
	code, body, reason, ok := fetchJSON(ctx, box, u)
	if !ok {
		return artifact.StatusUnverifiable, detail(reason)
	}
	if !is2xx(code) {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("pypi project %q not fetchable (HTTP %d)", name, code))
	}
	var meta struct {
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		return artifact.StatusUnverifiable, detail("pypi metadata parse error: " + err.Error())
	}
	if _, present := meta.Releases[ver]; present {
		return artifact.StatusPass, fmt.Sprintf("pypi %s==%s present in .releases", name, ver)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("pypi %s has no release %q", name, ver))
}

// --- crates.io ---------------------------------------------------------------

// checkCrateVersion asserts Evidence.Value is a published version of the crate named
// by the SourceURL, via crates.io/api/v1/crates/<name> and the .versions[].num list.
// Present => Pass; fetched crate missing the version => Fail; non-2xx/parse error =>
// Unverifiable.
func checkCrateVersion(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	name, err := pkgName(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	ver, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	u := "https://crates.io/api/v1/crates/" + name
	code, body, reason, ok := fetchJSON(ctx, box, u)
	if !ok {
		return artifact.StatusUnverifiable, detail(reason)
	}
	if !is2xx(code) {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("crate %q not fetchable (HTTP %d)", name, code))
	}
	var meta struct {
		Versions []struct {
			Num string `json:"num"`
		} `json:"versions"`
	}
	if err := json.Unmarshal([]byte(body), &meta); err != nil {
		return artifact.StatusUnverifiable, detail("crates metadata parse error: " + err.Error())
	}
	for _, v := range meta.Versions {
		if v.Num == ver {
			return artifact.StatusPass, fmt.Sprintf("crate %s@%s present in .versions", name, ver)
		}
	}
	return artifact.StatusFail, detail(fmt.Sprintf("crate %s has no version %q", name, ver))
}

// --- github release ----------------------------------------------------------

// checkGitHubRelease asserts a release tagged Evidence.Value exists in the repo named
// by the SourceURL, via api.github.com/repos/<o>/<r>/releases/tags/<tag>. Pass on a
// 2xx whose tag_name matches; Fail on 404 (no such release).
func checkGitHubRelease(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	owner, repo, err := ownerRepo(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	tag, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	u := "https://api.github.com/repos/" + owner + "/" + repo + "/releases/tags/" + url.PathEscape(tag)
	code, body, reason, ok := fetchJSON(ctx, box, u, githubAccept)
	if !ok {
		return artifact.StatusUnverifiable, detail(reason)
	}
	if code == 404 {
		return artifact.StatusFail, detail(fmt.Sprintf("no release tagged %q in %s/%s", tag, owner, repo))
	}
	if !is2xx(code) {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("github releases API HTTP %d", code))
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal([]byte(body), &rel); err != nil {
		return artifact.StatusUnverifiable, detail("github release parse error: " + err.Error())
	}
	if rel.TagName == tag {
		return artifact.StatusPass, fmt.Sprintf("release %s exists in %s/%s", tag, owner, repo)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("release tag_name %q != asserted %q", rel.TagName, tag))
}

// --- github tag --------------------------------------------------------------

// tagsPerPage is GitHub's max page size for the tags listing, and tagMaxPages bounds the
// walk so a repo with an unbounded number of tags can never spin the verifier forever. At
// most tagsPerPage*tagMaxPages tags are inspected; beyond that we return Unverifiable (we
// could not decisively prove absence), never a false Fail (I2). Each page is one box.Exec,
// ctx-honored.
const (
	tagsPerPage = 100
	tagMaxPages = 10
)

// checkGitHubTag asserts a git tag named Evidence.Value exists in the repo named by the
// SourceURL, via api.github.com/repos/<o>/<r>/tags (the PAGED tag list). It walks pages
// until the tag is found or a short/empty page proves the listing is exhausted: present =>
// Pass; a fully-walked list without it => Fail; a non-2xx / fetch error => Unverifiable.
// Absence from page 1 is NOT decisive — GitHub returns at most tagsPerPage tags per page, so
// a real tag can sit on page 2+ and must not be reported missing (the false-RED this fixes).
// If the page budget is exhausted before a short last page, we cannot prove absence and fail
// toward Unverifiable rather than a false Fail.
func checkGitHubTag(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	owner, repo, err := ownerRepo(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	tag, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	for page := 1; page <= tagMaxPages; page++ {
		// Honor cancellation between pages so a slow walk stays bounded by ctx.
		if err := ctx.Err(); err != nil {
			return artifact.StatusUnverifiable, detail("context canceled while paging github tags: " + err.Error())
		}
		u := fmt.Sprintf("https://api.github.com/repos/%s/%s/tags?per_page=%d&page=%d", owner, repo, tagsPerPage, page)
		code, body, reason, ok := fetchJSON(ctx, box, u, githubAccept)
		if !ok {
			return artifact.StatusUnverifiable, detail(reason)
		}
		if !is2xx(code) {
			return artifact.StatusUnverifiable, detail(fmt.Sprintf("github tags API HTTP %d", code))
		}
		var tags []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(body), &tags); err != nil {
			return artifact.StatusUnverifiable, detail("github tags parse error: " + err.Error())
		}
		for _, t := range tags {
			if t.Name == tag {
				return artifact.StatusPass, fmt.Sprintf("tag %s exists in %s/%s", tag, owner, repo)
			}
		}
		if len(tags) < tagsPerPage {
			// A short (or empty) page is the LAST page — the listing is exhausted and the tag
			// is decisively absent. Only here is Fail correct.
			return artifact.StatusFail, detail(fmt.Sprintf("tag %q not in %s/%s tag list", tag, owner, repo))
		}
		// A full page: more tags may follow — continue to the next page.
	}
	// The page budget ran out before a short last page. We did NOT exhaust the listing, so
	// absence is not proven: fail toward Unverifiable, never a false Fail (I2).
	return artifact.StatusUnverifiable, detail(fmt.Sprintf("tag %q not found in the first %d pages of %s/%s tags (listing not exhausted)", tag, tagMaxPages, owner, repo))
}

// --- license -----------------------------------------------------------------

// checkLicenseMatches asserts the repo named by the SourceURL is licensed under the
// SPDX id Evidence.Value, via api.github.com/repos/<o>/<r>/license and .license.spdx_id.
// Comparison is case-insensitive + whitespace-normalized. A repo with no detected
// license (404 / null spdx_id) => Fail; a non-2xx (other) => Unverifiable.
func checkLicenseMatches(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	owner, repo, err := ownerRepo(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	want, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	u := "https://api.github.com/repos/" + owner + "/" + repo + "/license"
	code, body, reason, ok := fetchJSON(ctx, box, u, githubAccept)
	if !ok {
		return artifact.StatusUnverifiable, detail(reason)
	}
	if code == 404 {
		return artifact.StatusFail, detail(fmt.Sprintf("no detected license in %s/%s", owner, repo))
	}
	if !is2xx(code) {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("github license API HTTP %d", code))
	}
	var lic struct {
		License struct {
			SPDXID string `json:"spdx_id"`
		} `json:"license"`
	}
	if err := json.Unmarshal([]byte(body), &lic); err != nil {
		return artifact.StatusUnverifiable, detail("github license parse error: " + err.Error())
	}
	got := lic.License.SPDXID
	if got == "" || strings.EqualFold(got, "NOASSERTION") {
		return artifact.StatusFail, detail(fmt.Sprintf("%s/%s has no asserted SPDX license", owner, repo))
	}
	if normalize(got) == normalize(want) {
		return artifact.StatusPass, fmt.Sprintf("%s/%s license is %s", owner, repo, got)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("license %q != asserted %q", got, want))
}

// githubAccept is the GitHub REST media-type header. A pack-authored constant — never
// model input — so it is safe to interpolate into the curl header list.
const githubAccept = "Accept: application/vnd.github+json"
