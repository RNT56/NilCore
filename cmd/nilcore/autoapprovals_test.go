package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
)

// TestXC05_AutoApprovalsAccount proves the revocation/undo accounting surface lists past
// auto-approvals with their evidence + the per-class undo story, and is fail-closed.
func TestXC05_AutoApprovalsAccount(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "e.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Kind: "auto_approve", Detail: map[string]any{"action": "open-pr", "scope": "feature/x"}})
	log.Append(eventlog.Event{Kind: "auto_deny", Detail: map[string]any{"action": "promote-to-base", "scope": "main", "reason": "out_of_scope"}})
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// Default: lists the auto_approve + the undo story, not the deny.
	out, err := runAutoApprovals(logPath, false)
	if err != nil {
		t.Fatalf("runAutoApprovals: %v", err)
	}
	if !strings.Contains(out, "1 auto-approved") || !strings.Contains(out, "open-pr") || !strings.Contains(out, "feature/x") {
		t.Fatalf("account must list the auto-approval:\n%s", out)
	}
	if !strings.Contains(out, "kill-switch") || !strings.Contains(out, "close the draft PR") {
		t.Fatalf("account must include the per-class undo story:\n%s", out)
	}
	if strings.Contains(out, "out_of_scope") {
		t.Fatalf("auto_deny must not show without -denied:\n%s", out)
	}

	// -denied also surfaces the deny + its reason.
	out, err = runAutoApprovals(logPath, true)
	if err != nil {
		t.Fatalf("runAutoApprovals -denied: %v", err)
	}
	if !strings.Contains(out, "out_of_scope") {
		t.Fatalf("-denied must list the auto_deny reason:\n%s", out)
	}

	// Fail-closed: a tampered chain yields no trustworthy account.
	data, _ := os.ReadFile(logPath)
	corrupt := []byte(strings.Replace(string(data), "feature/x", "feature/Z", 1))
	if err := os.WriteFile(logPath, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runAutoApprovals(logPath, false); err == nil {
		t.Fatal("a tampered chain must fail closed (no account over forged evidence)")
	}
}
