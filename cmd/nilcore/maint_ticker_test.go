package main

import (
	"context"
	"testing"
	"time"
)

// The maintenance ticker must exit promptly when the serve ctx is cancelled — a
// long-lived serve must not leak this goroutine on shutdown. (The GC it runs is
// independently tested; here we only assert the lifecycle.)
func TestMaintenanceTickerExitsOnCancel(t *testing.T) {
	log := openTestLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runMaintenanceTicker(ctx, t.TempDir(), log); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runMaintenanceTicker did not exit on ctx cancellation (goroutine leak)")
	}
}
