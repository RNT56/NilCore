package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/forge"
	"nilcore/internal/policy"
	"nilcore/internal/tools"
)

// credInURL matches an embedded "user[:secret]@" in a URL, so a credential baked
// into the origin remote (https://user:token@host/…) can be scrubbed before any
// git output reaches the error string or the append-only event log (I3 — a secret
// must never be logged).
var credInURL = regexp.MustCompile(`(//)[^/@\s]+@`)

func scrubCreds(s string) string { return credInURL.ReplaceAllString(s, "$1") }

// openGatedPR pushes a verified branch and opens a DRAFT pull request — but ONLY
// after the human gate approves (D4-T01). It is the opt-in completion of a
// self-started, verified, reversible task (`--open-pr` on watch/schedule). The
// agent NEVER merges; the push runs inside GatedOpen's prepare step, so nothing
// leaves the repo until the gate says yes (I3). A missing token/remote, an
// unparseable remote, or an empty diff degrades cleanly: it logs and opens no PR —
// never crashes, never guesses, never pushes unapproved.
func openGatedPR(ctx context.Context, repoDir, branch, goal string, approver policy.Approver, cred func(string) string, log *eventlog.Log) {
	token := cred("NILCORE_FORGE_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "open-pr: NILCORE_FORGE_TOKEN not set; skipping PR")
		return
	}
	remote, err := gitRemoteURL(ctx, repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open-pr: cannot read origin remote: %v; skipping PR\n", err)
		return
	}
	owner, repo, err := forge.ParseRemote(remote)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open-pr: %v; skipping PR\n", err)
		return
	}
	base := forge.DefaultBase()
	if !branchAhead(ctx, repoDir, base, branch) {
		fmt.Fprintf(os.Stderr, "open-pr: %s has no commits ahead of %s; skipping PR\n", branch, base)
		return
	}

	pr := forge.PR{
		Owner: owner, Repo: repo, Head: branch, Base: base,
		Title: prTitle(goal),
		Body:  "Opened by nilcore from a verified, self-started task.\n\nGoal:\n" + goal,
		Draft: true,
	}
	// The push is the gate's prepare step: it runs only after the approver says yes.
	push := func(ctx context.Context) error { return gitPush(ctx, repoDir, branch) }

	url, opened, err := forge.NewClient(token).GatedOpen(ctx, approver, pr, push)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "open-pr: %v\n", err)
		log.Append(eventlog.Event{Kind: "open_pr", Detail: map[string]any{"branch": branch, "error": err.Error()}})
	case !opened:
		fmt.Fprintln(os.Stderr, "open-pr: gate denied; no PR opened, nothing pushed")
		log.Append(eventlog.Event{Kind: "open_pr", Detail: map[string]any{"branch": branch, "opened": false}})
	default:
		fmt.Fprintf(os.Stderr, "open-pr: draft PR opened: %s\n", url)
		log.Append(eventlog.Event{Kind: "open_pr", Detail: map[string]any{"branch": branch, "opened": true}})
	}
}

// prTitle derives a concise PR title from the goal (first line, bounded).
func prTitle(goal string) string {
	t := goal
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	const max = 72
	if len(t) > max {
		t = strings.TrimSpace(t[:max])
	}
	if t == "" {
		t = "nilcore: automated change"
	}
	return t
}

// hardenedGit builds an exec.Cmd for git in repoDir with the same host-side
// hardening the structured git tool uses (HardenArgs + HardenedEnv): hooks and
// fsmonitor disabled, ambient git config stripped, no credential prompt — so a
// model-writable .git can never run code or read host config on the host (I4).
func hardenedGit(ctx context.Context, repoDir string, args ...string) *exec.Cmd {
	full := append(tools.HardenArgs(), append([]string{"-C", repoDir}, args...)...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = tools.HardenedEnv()
	return cmd
}

func gitRemoteURL(ctx context.Context, repoDir string) (string, error) {
	out, err := hardenedGit(ctx, repoDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// branchAhead reports whether branch has at least one commit not in base.
func branchAhead(ctx context.Context, repoDir, base, branch string) bool {
	out, err := hardenedGit(ctx, repoDir, "rev-list", "--count", base+".."+branch).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "0"
}

func gitPush(ctx context.Context, repoDir, branch string) error {
	cmd := hardenedGit(ctx, repoDir, "push", "origin", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s: %v: %s", branch, err, scrubCreds(strings.TrimSpace(string(out))))
	}
	return nil
}
