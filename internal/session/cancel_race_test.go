package session

import (
	"context"
	"testing"
)

// blockingRouter parks in Route until the drive ctx is cancelled, holding the
// Session in the Routing phase so a test can exercise a Cancel issued DURING
// routing (the classifier window) — the exact window where Cancel's drives.Wait
// could previously race the launch-time drives.Add.
type blockingRouter struct{ entered chan struct{} }

func (r *blockingRouter) Route(ctx context.Context, _ string, _ WorkState) (Route, error) {
	close(r.entered)
	<-ctx.Done()
	return RouteContinue, ctx.Err()
}

// TestCancelDuringRoutingNoRace is the regression for the WaitGroup Add-vs-Wait
// race: a Cancel issued while a drive is mid-Routing must (a) be data-race-free
// (assert by running `go test -race ./internal/session/...`) and (b) fully join the
// unwinding drive before returning, so the Session is back to Idle. Before the fix
// (drives.Add at launch, which routing never reached), Cancel's Wait ran at counter
// zero — racing the eventual Add and returning before the drive unwound (Phase
// still Routing). After it (Add at the Routing flip, Done in toIdle), Wait always
// has a positive counter and a guaranteed matching Done.
func TestCancelDuringRoutingNoRace(t *testing.T) {
	r := &blockingRouter{entered: make(chan struct{})}
	s := New("c", "local", "/repo", nil)
	s.Router = r
	s.Drivers = Drivers{Native: newFakeDriver(DriveResult{Verified: true})}

	turnDone := make(chan struct{})
	go func() { _ = s.Turn(context.Background(), "do work"); close(turnDone) }()

	waitClosed(t, r.entered) // Turn has flipped to Routing (+Add) and is parked in Route

	if !s.Cancel() {
		t.Fatal("Cancel during the Routing window must report it cancelled a run")
	}
	if got := s.PhaseNow(); got != Idle {
		t.Fatalf("after Cancel phase = %v, want Idle — Cancel must fully join the unwinding drive", got)
	}
	waitClosed(t, turnDone)
}
