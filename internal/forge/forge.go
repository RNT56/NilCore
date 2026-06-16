// Package forge opens pull requests on a remote SCM (GitHub) from a verified
// working branch. It is harness-side, gated code: opening a PR is outward-facing
// and irreversible, so the only entry point (GatedOpen) runs behind the human
// gate — a nil approver default-denies (no ambient authority, invariant I3). The
// auth token is supplied by the caller from the SecretStore and is used only to
// set a per-request header; it is never logged, never placed in a prompt, and
// never given to the model (I3). The agent never merges — a PR is opened for a
// human to review and land.
//
// Stdlib only (invariant I6): the GitHub REST API is spoken over net/http with
// encoding/json, no client module.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nilcore/internal/policy"
)

// PR describes a pull request to open. Head is the working branch (e.g.
// "task/P9-T07"); Base is the branch to target (e.g. "main"). Draft is true by
// default at the call site so the PR is clearly agent-authored and awaiting
// review.
type PR struct {
	Owner string
	Repo  string
	Head  string
	Base  string
	Title string
	Body  string
	Draft bool
}

// Client opens PRs against a GitHub-compatible REST API. BaseURL is overridable
// for self-hosted GitHub Enterprise and for tests (httptest).
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a GitHub forge client. The token is held only for per-request
// headers (I3).
func NewClient(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: "https://api.github.com",
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// GatedOpen gates the PR behind the human approver (a PR is irreversible /
// outward-facing), and only on approval runs prepare (e.g. push the working
// branch with hardened git) and then opens the PR. It returns the PR URL and
// whether it was opened. A nil approver default-denies — there is no ambient
// authority for an irreversible step. prepare may be nil.
func (c *Client) GatedOpen(ctx context.Context, ask policy.Approver, pr PR, prepare func(context.Context) error) (url string, opened bool, err error) {
	action := policy.GateAction{Type: policy.OpenPR, Branch: pr.Head, Detail: pr.Title}
	if !policy.GateStructured(action, ask) {
		return "", false, nil
	}
	if prepare != nil {
		if err := prepare(ctx); err != nil {
			return "", false, fmt.Errorf("prepare branch %q: %w", pr.Head, err)
		}
	}
	url, err = c.Open(ctx, pr)
	if err != nil {
		return "", false, err
	}
	return url, true, nil
}

// Open POSTs the create-pull-request call and returns the PR's html_url. It does
// no gating of its own — callers must gate first (use GatedOpen). It never merges.
func (c *Client) Open(ctx context.Context, pr PR) (string, error) {
	if pr.Owner == "" || pr.Repo == "" || pr.Head == "" || pr.Base == "" {
		return "", fmt.Errorf("forge: owner, repo, head, and base are required")
	}
	reqBody, err := json.Marshal(map[string]any{
		"title": pr.Title,
		"head":  pr.Head,
		"base":  pr.Base,
		"body":  pr.Body,
		"draft": pr.Draft,
	})
	if err != nil {
		return "", fmt.Errorf("marshal pr: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", strings.TrimRight(c.BaseURL, "/"), pr.Owner, pr.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/vnd.github+json")
	// The token sets a per-request header only (I3): never logged, never persisted.
	if c.Token != "" {
		req.Header.Set("authorization", "Bearer "+c.Token)
	}

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("open pr request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// The body may echo the request; do not surface the token (it is only ever
		// in the header, never the body). Trim to a short tail for the error.
		return "", fmt.Errorf("forge api: %s: %s", resp.Status, tail(string(raw), 500))
	}

	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode pr response: %w", err)
	}
	return out.HTMLURL, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
