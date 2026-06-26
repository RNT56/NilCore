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

// matchAny reports whether scope matches any glob pattern (path.Match semantics).
// An empty scope never matches a non-empty pattern set unless a pattern explicitly
// admits it. A malformed pattern is treated as a non-match (fail-safe: a bad
// allowlist entry never widens admission, a bad denylist entry simply does not deny
// — but deny entries are author-controlled host data, and the protected-branch
// floor is enforced separately).
func matchAny(scope string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, scope); err == nil && ok {
			return true
		}
	}
	return false
}

// isProd reports whether a scope/environment is a production target. prod* is
// ALWAYS denied structurally for every class regardless of the operator allowlist
// (defense in depth alongside commonDeny).
func isProd(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "prod" || strings.HasPrefix(s, "prod")
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
		if a != action || s != scope {
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
