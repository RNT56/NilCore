package main

// autoapprovals.go implements `nilcore auto-approvals` — the revocation/undo ACCOUNTING
// surface (Phase 16, XC-T05). The kill-switch (`.nilcore/AUTOAPPROVE_OFF` /
// `NILCORE_AUTOAPPROVE_OFF`) stops FUTURE auto-approvals instantly; this read-only verb
// accounts for PAST ones: it replays the append-only log and lists every `auto_approve`
// (and self-improve auto-merge), with `-denied` also every `auto_deny`, each with its
// evidence, plus the documented per-class UNDO story so an operator can review and
// reverse what auto-proceeded.
//
// It mirrors trust/lessons' read-only discipline: a new subcommand off main's switch, no
// new event kinds (purely a reader — I5), default behaviour off the literal first-arg.
// It is FAIL-CLOSED on a broken hash chain: a tampered log yields no trustworthy account,
// so the command prints the error and exits non-zero rather than account over forged
// evidence (I5).

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"nilcore/internal/eventlog"
)

// autoApprovalsMain is the `nilcore auto-approvals` entrypoint. Read-only: it never
// writes the log. A broken chain is printed and exits non-zero (fail-closed).
func autoApprovalsMain(args []string) {
	fs := flag.NewFlagSet("auto-approvals", flag.ExitOnError)
	logPath := fs.String("log", defaultLogPath, "append-only event log path")
	denied := fs.Bool("denied", false, "also list auto_deny decisions (why an action fell through to the human)")
	_ = fs.Parse(args)

	out, err := runAutoApprovals(*logPath, *denied)
	if err != nil {
		fatal(err)
	}
	fmt.Fprint(os.Stdout, out)
}

// autoApprovalDecision is one folded auto-approval / auto-deny record.
type autoApprovalDecision struct {
	When   time.Time
	Kind   string // auto_approve | auto_approve_selfimprove | auto_deny
	Action string
	Scope  string
	Reason string // auto_deny only
}

// runAutoApprovals is the pure command core (separated for testing): it replays the log
// READ-ONLY, folds the auto-approval decisions, runs eventlog.Verify (fail-closed), and
// renders the account + the per-class undo story. A missing log is an empty account.
func runAutoApprovals(logPath string, includeDenied bool) (string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "no auto-approvals recorded yet (no event log at " + logPath + ").\n", nil
		}
		return "", fmt.Errorf("auto-approvals: opening event log: %w", err)
	}
	defer f.Close()

	var decisions []autoApprovalDecision
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e struct {
			Time   time.Time      `json:"time"`
			Kind   string         `json:"kind"`
			Detail map[string]any `json:"detail"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return "", fmt.Errorf("auto-approvals: event %d: %w", n, err)
		}
		switch e.Kind {
		case "auto_approve", "auto_approve_selfimprove":
			action, _ := e.Detail["action"].(string)
			scope, _ := e.Detail["scope"].(string)
			decisions = append(decisions, autoApprovalDecision{When: e.Time, Kind: e.Kind, Action: action, Scope: scope})
		case "auto_deny":
			if !includeDenied {
				continue
			}
			action, _ := e.Detail["action"].(string)
			scope, _ := e.Detail["scope"].(string)
			reason, _ := e.Detail["reason"].(string)
			decisions = append(decisions, autoApprovalDecision{When: e.Time, Kind: e.Kind, Action: action, Scope: scope, Reason: reason})
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("auto-approvals: reading event log: %w", err)
	}

	// Chain integrity is eventlog's authority: a tampered log yields no trustworthy
	// account (fail-closed — I5), so we surface the error AFTER dropping what we read.
	if err := eventlog.Verify(logPath); err != nil {
		return "", fmt.Errorf("auto-approvals: verifying chain (account untrustworthy): %w", err)
	}

	sort.SliceStable(decisions, func(i, j int) bool { return decisions[i].When.Before(decisions[j].When) })
	return renderAutoApprovals(decisions, includeDenied), nil
}

// renderAutoApprovals formats the decisions + the per-class undo story.
func renderAutoApprovals(decisions []autoApprovalDecision, includeDenied bool) string {
	var b strings.Builder
	approved := 0
	for _, d := range decisions {
		if d.Kind != "auto_deny" {
			approved++
		}
	}
	fmt.Fprintf(&b, "auto-approvals: %d auto-approved boundary action(s)\n", approved)
	if len(decisions) == 0 {
		b.WriteString("  (none yet)\n")
	}
	for _, d := range decisions {
		ts := d.When.UTC().Format("2006-01-02T15:04:05Z")
		switch d.Kind {
		case "auto_deny":
			fmt.Fprintf(&b, "  ✗ %s  %-16s %-24s denied: %s\n", ts, d.Action, d.Scope, d.Reason)
		case "auto_approve_selfimprove":
			fmt.Fprintf(&b, "  ✓ %s  self-improve     %s\n", ts, d.Action)
		default:
			fmt.Fprintf(&b, "  ✓ %s  %-16s %s\n", ts, d.Action, d.Scope)
		}
	}
	b.WriteString("\nThe kill-switch (.nilcore/AUTOAPPROVE_OFF or NILCORE_AUTOAPPROVE_OFF=1) stops FUTURE\n" +
		"auto-approvals instantly. To reverse a PAST one, per class:\n" +
		"  open-pr          → close the draft PR (no merge ever happens for an auto-opened PR)\n" +
		"  promote-to-base  → reset/delete the non-main branch the tip was promoted to\n" +
		"  deploy           → redeploy the previous release (bounded by the $/day + rate caps)\n" +
		"  self-improve     → git revert the prompt/skill commit the flow merged\n")
	if !includeDenied {
		b.WriteString("\n(pass -denied to also list auto_deny decisions — why an action fell through to the human.)\n")
	}
	return b.String()
}
