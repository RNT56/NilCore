package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/graapprove"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
)

func TestWrapAutoApprove_DefaultOffByteIdentical(t *testing.T) {
	human := policy.NewConsoleApprover(strings.NewReader(""), io.Discard)
	// No envelope ⇒ the human approver is returned UNCHANGED (same value).
	if got := wrapAutoApprove(human, onboard.Config{}, "x.jsonl", nil, nil); got != policy.Approver(human) {
		t.Errorf("no envelope must return the human approver unchanged (byte-identical default-off)")
	}
	// A nil human (e.g. the swarm approver) is returned as-is — never wrapped.
	if got := wrapAutoApprove(nil, onboard.Config{}, "x.jsonl", nil, nil); got != nil {
		t.Errorf("nil human must be returned as-is, got %v", got)
	}
}

func TestWrapAutoApprove_WrapsWhenConfigured(t *testing.T) {
	env, err := graapprove.Preset("conservative")
	if err != nil {
		t.Fatalf("preset: %v", err)
	}
	human := policy.NewConsoleApprover(strings.NewReader("y\n"), io.Discard)
	got := wrapAutoApprove(human, onboard.Config{AutoApprove: &env}, "x.jsonl", nil, nil)
	if got == policy.Approver(human) {
		t.Errorf("a configured envelope must yield a wrapped (graded) approver, not the bare human")
	}
}

func TestAutoApproveSink_AppendsToChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	autoApproveSink{log}.Emit("auto_approve", map[string]any{"action": "open-pr", "scope": "feature/x"})
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A well-formed event was appended without breaking the hash chain.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("sink Emit broke the event-log chain: %v", err)
	}
}
