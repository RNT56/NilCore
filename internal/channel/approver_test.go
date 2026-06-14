package channel_test

import (
	"context"
	"errors"
	"testing"

	"nilcore/internal/channel"
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
