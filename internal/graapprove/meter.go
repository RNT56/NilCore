package graapprove

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Sink is the audit seam: the GradedApprover emits each decision through it. It is
// deliberately decoupled from eventlog (the capguard/blastbudget pattern) so this
// leaf stays pure and tests need no real log — the wiring layer adapts Emit to
// eventlog.Append. A nil Sink is silent.
type Sink interface {
	Emit(kind string, detail map[string]any)
}

// dayKey renders a time as the per-UTC-day window key used by the rate counter and
// blastbudget. The window rolls at midnight UTC by construction because each day is
// a distinct key.
func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// matchAny reports whether scope matches any glob pattern.
//
// A lone `*` means ANY scope. path.Match's `*` never crosses '/', but every real gate
// scope is a slash-y branch (worktree.Create names them "task/<id>"; watch/schedule open
// PRs from "task/trig-<nano>"), so the shipped presets' AllowBranches:["*"] matched
// NOTHING and graduated auto-approval was structurally unreachable for its two live
// classes. Widening `*` is safe because it is only ever an ALLOW predicate: the
// protected-base floor (isProd/isProtectedBase, on both the scope and the destination
// base), the operator's DenyBranches, the earned-trust bar, the per-day rate window and
// the blast budget are each evaluated separately and still bound the decision.
//
// An EMPTY scope never matches: an action with no target has no bounded blast radius and
// must never be auto-approved (fail-closed).
//
// Every other pattern keeps path.Match semantics, where `*` stays segment-local — so a
// deliberate "feat/*" still admits feat/x and not feat/x/y. A malformed pattern is a
// non-match (fail-safe: a bad allowlist entry never widens admission; deny entries are
// author-controlled host data and the protected-branch floor is enforced separately).
func matchAny(scope string, patterns []string) bool {
	// TrimSpace, not `== ""`: a whitespace-only scope is just as targetless, and it
	// would otherwise clear the floor (isProd/isProtectedBase both trim to "") and then
	// match a lone `*`.
	if strings.TrimSpace(scope) == "" {
		return false
	}
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if ok, err := path.Match(p, scope); err == nil && ok {
			return true
		}
	}
	return false
}

// trustScope collapses a gate scope into the STABLE FAMILY that earned trust and the
// per-day rate window accrue over.
//
// Every live gate scope is unique per run: worktree branches are "task/<taskID>",
// watch/schedule open PRs from "task/trig-<unix-nano>", and a swarm promote names its
// integration tip. Keying trust on the exact scope made `Green >= MinSuccesses`
// unsatisfiable by construction — no scope is ever seen twice — and keying the rate
// window on it made MaxPerDay unenforceable, since every auto-approval opened a fresh
// window. Both now key on the family:
//
//	task/trig-1720512345  ->  task/*        (a branch namespace)
//	feat/a/b              ->  feat/a/*
//	9f3c1ab...            ->  #commit       (a bare commit sha)
//	main, id@cmd-hash     ->  unchanged     (already stable)
//
// This is deliberately coarser than the exact branch: trust means "this agent has
// completed THIS CLASS of action against THIS FAMILY of target, verifier-green, N
// times". It is only ever a NECESSARY condition — the protected-base floor, the
// operator's Allow/DenyBranches, the rate cap, and the blast budget each bound the
// decision on the CONCRETE scope, which is what the audit event records.
func trustScope(scope string) string {
	s := strings.TrimSpace(scope)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "/"); i > 0 {
		return s[:i] + "/*"
	}
	if isCommitSHA(s) {
		return "#commit"
	}
	return s
}

// isCommitSHA reports whether s is a bare hex commit id (abbreviated or full). Such a
// scope is unique per run, so it collapses to one family rather than never recurring.
func isCommitSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// isProd reports whether a scope/environment is a production target. prod* is
// ALWAYS denied structurally for every class regardless of the operator allowlist
// (defense in depth alongside commonDeny).
func isProd(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "prod" || strings.HasPrefix(s, "prod")
}

// isProtectedBase reports whether a scope is a protected base branch that must
// NEVER be auto-approved, regardless of an operator-authored envelope. The presets
// bake these into commonDeny, but a hand-built ClassClause could omit them — so this
// is the STRUCTURAL floor enforced in ApproveStructured (the charter invariant
// "graduated auto-approval never auto-approves main/prod"). Matches main, master,
// and any release branch (release, release/*, release-*).
func isProtectedBase(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "main", "master", "release", "trunk", "stable":
		return true
	}
	return strings.HasPrefix(s, "release/") || strings.HasPrefix(s, "release-")
}

// countAutoApprovalsToday folds the append-only log READ-ONLY and counts the
// `auto_approve` events for (action,scope) whose event-day equals today (UTC). This
// rebuilds the per-day rate window from the durable log on every decision, so a
// restart never resets the window to zero (no fail-open; I5). A missing log is zero.
// A read/parse fault returns the count so far plus the error so the caller can fail
// closed (deny) rather than silently under-count.
func countAutoApprovalsToday(logPath, action, scope, today string) (int, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e boundaryEvent // reuse: same {Time,Kind,Detail} shape
		if err := json.Unmarshal(line, &e); err != nil {
			return count, err
		}
		if e.Kind != "auto_approve" {
			continue
		}
		a, _ := e.Detail["action"].(string)
		s, _ := e.Detail["scope"].(string)
		// The event records the CONCRETE scope; the window counts the family, or a
		// per-run-unique branch would open a fresh window on every auto-approval and
		// MaxPerDay would never bind.
		if a != action || trustScope(s) != trustScope(scope) {
			continue
		}
		if dayKey(e.Time) == today {
			count++
		}
	}
	if err := sc.Err(); err != nil {
		return count, err
	}
	return count, nil
}
