package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/session"
)

// The gate must cap how many drives run at once and still return each one's result.
func TestDriveGateCapsConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newDriveGate(ctx, 2)
	defer gate.close()

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			out, err := gate.runOutcome(ctx, "t", func(context.Context) (session.DriveOutcome, error) {
				cur := inFlight.Add(1)
				for { // raise peak
					old := peak.Load()
					if cur <= old || peak.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(15 * time.Millisecond)
				inFlight.Add(-1)
				return session.DriveOutcome{Summary: "ok", Verified: true}, nil
			})
			if err != nil || out.Summary != "ok" || !out.Verified {
				t.Errorf("drive %d = %+v, %v", n, out, err)
			}
		}(i)
	}
	wg.Wait()

	if p := peak.Load(); p > 2 {
		t.Errorf("peak concurrency %d exceeded the cap of 2", p)
	}
}

// A cancelled ctx releases a parked drive (no wedge at shutdown). A nil gate runs
// inline.
func TestDriveGateCancelAndNil(t *testing.T) {
	// nil gate: inline passthrough.
	var nilGate *driveGate
	out, err := nilGate.runOutcome(context.Background(), "t", func(context.Context) (session.DriveOutcome, error) {
		return session.DriveOutcome{Summary: "inline"}, nil
	})
	if err != nil || out.Summary != "inline" {
		t.Fatalf("nil gate = %+v, %v", out, err)
	}

	// Saturate a cap-1 gate, then a second drive parks; cancelling its ctx must
	// release it rather than hang. (close() is called only after both run-func
	// goroutines return — mirroring serve's drainShutdown-then-close ordering.)
	ctx, cancel := context.WithCancel(context.Background())
	gate := newDriveGate(context.Background(), 1)
	release := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = gate.runOutcome(context.Background(), "hold", func(context.Context) (session.DriveOutcome, error) {
			<-release
			return session.DriveOutcome{}, nil
		})
	}()
	time.Sleep(10 * time.Millisecond) // let the holder occupy the single slot

	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, e := gate.runOutcome(ctx, "parked", func(context.Context) (session.DriveOutcome, error) {
			return session.DriveOutcome{}, nil
		})
		done <- e
	}()
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Error("a cancelled parked drive must return ctx.Err()")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked drive did not release on ctx cancel — shutdown would wedge")
	}
	close(release)
	wg.Wait()    // both run-func callers have returned...
	gate.close() // ...so draining the pool cannot race a live Submit
}
