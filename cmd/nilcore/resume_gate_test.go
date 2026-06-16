package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/channel"
)

func TestOwnerThreadFromTaskID(t *testing.T) {
	cases := map[string]string{
		"123456789-1":  "123456789", // telegram numeric thread + seq
		"123456789-42": "123456789",
		"a-b-3":        "a-b", // only the last numeric segment is the seq
		"thread":       "",    // no seq suffix
		"thread-":      "",    // empty suffix
		"thread-x":     "",    // non-numeric suffix
		"-5":           "",    // no prefix
	}
	for in, want := range cases {
		if got := ownerThreadFromTaskID(in); got != want {
			t.Errorf("ownerThreadFromTaskID(%q) = %q, want %q", in, got, want)
		}
	}
}

// captureChannel records Update calls so the test can assert the resume gate informed
// the right thread.
type captureChannel struct {
	mu      sync.Mutex
	updates map[string]string // threadID -> last message
}

func (c *captureChannel) Receive(ctx context.Context) (channel.TaskRequest, error) {
	<-ctx.Done()
	return channel.TaskRequest{}, ctx.Err()
}
func (c *captureChannel) Update(_ context.Context, threadID, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updates == nil {
		c.updates = map[string]string{}
	}
	c.updates[threadID] = message
	return nil
}
func (c *captureChannel) Ask(context.Context, string, string) (bool, error) { return false, nil }

// On a resumed gate, the approver INFORMS the owner's thread (recovered from the task
// id) of the gate, then DENIES — never auto-approves (I3).
func TestInformGateApproverInformsAndDenies(t *testing.T) {
	ch := &captureChannel{}
	a := informGateApprover{ch: ch, ctx: context.Background(), taskID: "555000-7"}

	if a.Approve("promote integration branch to base") {
		t.Fatal("a resumed gate must NEVER auto-approve (I3)")
	}
	msg, ok := ch.updates["555000"]
	if !ok {
		t.Fatalf("owner thread 555000 was not informed; updates=%v", ch.updates)
	}
	if !strings.Contains(msg, "promote integration branch to base") || !strings.Contains(strings.ToUpper(msg), "GATE") {
		t.Errorf("inform message should carry the gate action; got %q", msg)
	}

	// No channel ⇒ silent deny, no panic.
	if (informGateApprover{ch: nil, ctx: context.Background(), taskID: "x-1"}).Approve("anything") {
		t.Error("nil channel must still deny")
	}
	// Unrecoverable owner thread ⇒ deny, no push.
	ch2 := &captureChannel{}
	if (informGateApprover{ch: ch2, ctx: context.Background(), taskID: "no-seq"}).Approve("act") {
		t.Error("must deny")
	}
	if len(ch2.updates) != 0 {
		t.Errorf("an unrecoverable owner thread must not push anywhere; got %v", ch2.updates)
	}
}
