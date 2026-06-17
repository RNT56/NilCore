package forge

// Remote parsing turns a `git remote get-url origin` value into the owner/repo
// pair the PR-create call needs. It lives here (not in the cmd layer) because it
// is pure, hermetic, and table-testable — the actual `git remote get-url`, the
// hardened push, and the gate all live in the cmd integration (see D4-T01). This
// file performs no I/O: it only parses a string the caller already obtained.
//
// Stdlib only (invariant I6): plain string surgery, no URL/SCM module.

import (
	"fmt"
	"strings"
)

// ParseRemote extracts the GitHub owner and repo from a git remote URL. It
// accepts the forms git actually emits for GitHub:
//
//	SSH (scp-like):  git@github.com:owner/repo.git
//	SSH (URL):       ssh://git@github.com/owner/repo.git
//	HTTPS:           https://github.com/owner/repo.git
//	HTTP:            http://github.com/owner/repo
//
// The trailing ".git" and any trailing slash are optional in every form. An
// embedded credential (e.g. https://user:tok@github.com/...) is tolerated and
// stripped — it is never returned or logged here (I3). A remote that is empty,
// malformed, or not GitHub returns a clear error so the caller can degrade
// cleanly (log it, open no PR) rather than guessing.
func ParseRemote(remoteURL string) (owner, repo string, err error) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return "", "", fmt.Errorf("forge: empty remote URL")
	}

	host, path, err := splitRemote(raw)
	if err != nil {
		return "", "", err
	}
	if !isGitHubHost(host) {
		// Surface only the host, never the raw remote: an HTTPS remote may embed a
		// credential (https://user:tok@host/...) that must not reach a log (I3).
		return "", "", fmt.Errorf("forge: remote host %q is not github.com", host)
	}

	owner, repo, err = ownerRepo(path)
	if err != nil {
		// Same reasoning: report the parsed owner/repo tail, not the raw remote.
		return "", "", fmt.Errorf("forge: %w", err)
	}
	return owner, repo, nil
}

// splitRemote separates the host from the owner/repo path across the SSH-scp,
// ssh://, and http(s):// shapes. It returns the lowercased host and the raw
// "owner/repo[.git]" tail (no leading slash).
func splitRemote(raw string) (host, path string, err error) {
	// scheme://[user[:pass]@]host[:port]/owner/repo
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme := strings.ToLower(raw[:i])
		switch scheme {
		case "ssh", "https", "http", "git":
			// recognized
		default:
			return "", "", fmt.Errorf("forge: unsupported remote scheme %q", scheme)
		}
		rest := raw[i+3:]
		authority, p, ok := cutSlash(rest)
		if !ok {
			// Do not echo raw: an ssh:// or https:// authority may carry a credential.
			return "", "", fmt.Errorf("forge: remote has no repository path")
		}
		return hostFromAuthority(authority), p, nil
	}

	// scp-like SSH: [user@]host:owner/repo  (the colon, not a slash, separates).
	// Reject anything that smells like a Windows path or a bare local path.
	if i := strings.Index(raw, ":"); i >= 0 {
		authority := raw[:i]
		p := strings.TrimPrefix(raw[i+1:], "/")
		if authority == "" || p == "" {
			return "", "", fmt.Errorf("forge: remote is not a recognized git remote")
		}
		return hostFromAuthority(authority), p, nil
	}

	return "", "", fmt.Errorf("forge: remote is not a recognized git remote")
}

// hostFromAuthority drops any "user[:pass]@" prefix and any ":port" suffix,
// returning the lowercased bare host. Credentials are dropped, never surfaced.
func hostFromAuthority(authority string) string {
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if c := strings.Index(authority, ":"); c >= 0 {
		authority = authority[:c]
	}
	return strings.ToLower(authority)
}

// cutSlash splits on the first "/", reporting whether a slash was present.
func cutSlash(s string) (before, after string, ok bool) {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}

func isGitHubHost(host string) bool {
	return host == "github.com"
}

// ownerRepo turns "owner/repo[.git]" (with optional trailing slash) into its two
// parts, validating that exactly one path segment separates them.
func ownerRepo(path string) (owner, repo string, err error) {
	p := strings.Trim(path, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", "", fmt.Errorf("missing owner/repo path")
	}

	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected owner/repo, got %q", p)
	}
	return parts[0], parts[1], nil
}

// DefaultBase is the PR base branch used when the caller has not configured one.
// It is a tiny pure helper the cmd layer wants so the default lives in one place
// (D4-T01: base = the configured base branch, default "main").
func DefaultBase() string { return "main" }
