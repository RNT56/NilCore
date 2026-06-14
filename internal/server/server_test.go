package server_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/channel"
	"nilcore/internal/policy"
	"nilcore/internal/server"
)

type fakeChannel struct {
	reqs    chan channel.TaskRequest
	mu      sync.Mutex
	updates []string
}

func (f *fakeChannel) Receive(ctx context.Context) (channel.TaskRequest, error) {
	select {
	case r := <-f.reqs:
		return r, nil
	case <-ctx.Done():
		return channel.TaskRequest{}, ctx.Err()
	}
}
func (f *fakeChannel) Update(_ context.Context, _ string, msg string) error {
	f.mu.Lock()
	f.updates = append(f.updates, msg)
	f.mu.Unlock()
	return nil
}
func (f *fakeChannel) Ask(context.Context, string, string) (bool, error) { return true, nil }

func (f *fakeChannel) sawUpdate(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.updates {
		if u == want {
			return true
		}
	}
	return false
}

func TestServeDispatchAndShutdown(t *testing.T) {
	fc := &fakeChannel{reqs: make(chan channel.TaskRequest, 1)}
	fc.reqs <- channel.TaskRequest{Goal: "do it", ThreadID: "t1", Sender: "u1"}

	ran := make(chan backend.Task, 1)
	run := func(_ context.Context, task backend.Task, _ policy.Approver) (string, error) {
		ran <- task
		return "done: verified", nil
	}

	srv := &server.Server{Channel: fc, Run: run} // nil Log is fine (nil-safe Append)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	select {
	case task := <-ran:
		if task.Goal != "do it" || task.ID == "" {
			t.Fatalf("dispatched task = %+v", task)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("task was not dispatched")
	}

	cancel() // SIGINT-equivalent
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want clean shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not shut down on cancel")
	}

	if !fc.sawUpdate("Starting: do it") || !fc.sawUpdate("done: verified") {
		t.Errorf("missing progress updates: %v", fc.updates)
	}
}
