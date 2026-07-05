package channel_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/channel"
	"nilcore/internal/policy"
)

type fakeChannel struct {
	answer   bool
	askErr   error
	askedQ   string
	askedTID string
}

func (f *fakeChannel) Receive(context.Context) (channel.TaskRequest, error) {
	return channel.TaskRequest{}, nil
}
func (f *fakeChannel) Update(context.Context, string, string) error { return nil }
func (f *fakeChannel) Ask(_ context.Context, threadID, q string) (bool, error) {
	f.askedTID, f.askedQ = threadID, q
	return f.answer, f.askErr
}

func TestNewApprover(t *testing.T) {
	fc := &fakeChannel{answer: true}
	ap := channel.NewApprover(context.Background(), fc, "thread-7")
	if !ap.Approve("git push origin main") {
		t.Error("expected approval")
	}
	if fc.askedTID != "thread-7" || fc.askedQ == "" {
		t.Errorf("Ask routed wrong: tid=%q q=%q", fc.askedTID, fc.askedQ)
	}

	// An Ask error must default-deny.
	fc2 := &fakeChannel{answer: true, askErr: errors.New("network")}
	if channel.NewApprover(context.Background(), fc2, "t").Approve("x") {
		t.Error("error must deny")
	}
}

// TestApproverStructured pins the channel gate's evidence contract: an
// evidence-less structured action produces the byte-identical legacy question; an
// evidence-carrying one appends the bounded compact block (diffstat + verify tail
// + spend, NEVER the diff excerpt) inside transport message caps.
func TestApproverStructured(t *testing.T) {
	action := policy.GateAction{Type: policy.PromoteToBase, Branch: "main", Detail: "verified tip"}

	// No evidence ⇒ byte-identical to the legacy Approve message.
	legacy := &fakeChannel{answer: true}
	channel.NewApprover(context.Background(), legacy, "t1").Approve(action.Describe())
	structured := &fakeChannel{answer: true}
	sa, ok := channel.NewApprover(context.Background(), structured, "t1").(policy.StructuredApprover)
	if !ok {
		t.Fatal("channel approver must implement policy.StructuredApprover")
	}
	if !sa.ApproveStructured(action) {
		t.Fatal("channel yes must approve")
	}
	if structured.askedQ != legacy.askedQ {
		t.Errorf("payload-less structured question drifted:\nlegacy:     %q\nstructured: %q", legacy.askedQ, structured.askedQ)
	}

	// With evidence ⇒ compact appendix, bounded, no raw diff.
	withEv := action
	withEv.Evidence = &policy.GateEvidence{
		Diffstat:    "3 file(s) changed, +40 −2",
		DiffExcerpt: "diff --git a/x b/x\n+raw diff line",
		VerifyTail:  "all checks passed",
		SpentUSD:    2.5,
	}
	fc := &fakeChannel{answer: false}
	sa2 := channel.NewApprover(context.Background(), fc, "t2").(policy.StructuredApprover)
	if sa2.ApproveStructured(withEv) {
		t.Fatal("channel no must deny")
	}
	q := fc.askedQ
	for _, want := range []string{
		"Approve this irreversible action?",
		withEv.Describe(),
		"3 file(s) changed, +40 −2",
		"all checks passed",
		"$2.5000",
		"full diff excerpt: see the run terminal",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("channel question missing %q in:\n%s", want, q)
		}
	}
	if strings.Contains(q, "+raw diff line") {
		t.Errorf("the diff excerpt must not ride a channel message:\n%s", q)
	}
	if len(q) > 4096 {
		t.Errorf("channel question exceeds transport cap: %d chars", len(q))
	}
}
