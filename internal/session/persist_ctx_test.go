package session

import (
	"context"
	"testing"
)

// ctxStore is a Store that honors context cancellation the way a real store does:
// database/sql checks ctx.Err() before the query ever reaches the driver, so an
// already-cancelled ctx makes the write a guaranteed no-op. The fakeStore in
// persist_test.go ignores ctx entirely, which is precisely why it could not catch
// the drive-time persist regression these tests pin.
type ctxStore struct {
	saves  int
	detail string
}

func (c *ctxStore) SaveConversation(ctx context.Context, id, goal, detail string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.saves++
	c.detail = detail
	return nil
}

func (c *ctxStore) LoadConversation(ctx context.Context, id string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return c.detail, c.detail != "", nil
}

// A terminal drive calls persist AFTER clearDriveCancelLocked has already fired
// the drive context's cancel. The write must still land: the earned state has to
// outlive the cancellation of the work that produced it.
func TestPersistWritesDespiteCancelledDriveCtx(t *testing.T) {
	st := &ctxStore{}
	s := &Session{ID: "conv-1", Store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // exactly what clearDriveCancelLocked() does before persist runs

	s.persist(ctx, WorkState{})

	if st.saves != 1 {
		t.Fatalf("persist with a cancelled drive ctx wrote %d times, want 1 "+
			"(the conversation state must survive the drive's own cancellation)", st.saves)
	}
}

// Cancel() mid-drive also cancels the drive ctx; the fold that follows must still
// persist, otherwise a cancelled-then-resumed conversation silently restarts.
func TestPersistWritesAfterMidDriveCancel(t *testing.T) {
	st := &ctxStore{}
	s := &Session{ID: "conv-2", Store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.persist(ctx, WorkState{})
	s.persist(ctx, WorkState{}) // a second terminal fold on the same dead ctx

	if st.saves != 2 {
		t.Fatalf("persist wrote %d times, want 2", st.saves)
	}
}

// Checkpoint runs on the SIGTERM path, whose ctx is cancelled by construction.
func TestCheckpointWritesDespiteCancelledShutdownCtx(t *testing.T) {
	st := &ctxStore{}
	s := &Session{ID: "conv-3", Store: st}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint on a cancelled shutdown ctx: %v", err)
	}
	if st.saves != 1 {
		t.Fatalf("Checkpoint wrote %d times, want 1", st.saves)
	}
}

// A nil Store stays a no-op on both paths (in-memory conversations).
func TestPersistAndCheckpointNilStoreNoOp(t *testing.T) {
	s := &Session{ID: "conv-4"}
	s.persist(context.Background(), WorkState{})
	if err := s.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint with nil Store: %v", err)
	}
}
