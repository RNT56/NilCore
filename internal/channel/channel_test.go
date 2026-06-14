package channel_test

import (
	"context"
	"testing"

	"nilcore/internal/channel"
)

// stubChannel is a compile-time check that the interface is implementable, plus
// a tiny behavioral smoke (Receive → Ask).
type stubChannel struct {
	req    channel.TaskRequest
	answer bool
	asked  string
}

func (s *stubChannel) Receive(context.Context) (channel.TaskRequest, error) { return s.req, nil }
func (s *stubChannel) Update(context.Context, string, string) error         { return nil }
func (s *stubChannel) Ask(_ context.Context, _, q string) (bool, error) {
	s.asked = q
	return s.answer, nil
}

// Interface conformance, asserted at compile time.
var _ channel.Channel = (*stubChannel)(nil)

func TestChannelStub(t *testing.T) {
	s := &stubChannel{req: channel.TaskRequest{Goal: "fix the bug", ThreadID: "t1"}, answer: true}

	got, err := s.Receive(context.Background())
	if err != nil || got.Goal != "fix the bug" {
		t.Fatalf("Receive = %+v, %v", got, err)
	}
	ok, err := s.Ask(context.Background(), "t1", "merge to main?")
	if err != nil || !ok {
		t.Fatalf("Ask = %v, %v", ok, err)
	}
	if s.asked != "merge to main?" {
		t.Errorf("Ask question = %q", s.asked)
	}
}
