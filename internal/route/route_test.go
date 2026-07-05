package route_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/model"
	"nilcore/internal/route"
	"nilcore/internal/verify"
)

type fakeBackend struct{ name string }

func (f fakeBackend) Name() string { return f.name }
func (f fakeBackend) Run(context.Context, backend.Task) (backend.Result, error) {
	return backend.Result{Backend: f.name, Summary: f.name + " did it", SelfClaimed: true}, nil
}

type fakeVerifier struct{ pass bool }

func (v fakeVerifier) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: v.pass}, nil
}

func TestRaceVerifierJudges(t *testing.T) {
	cands := []route.Candidate{
		{Backend: fakeBackend{"loser"}, Verifier: fakeVerifier{false}, Task: backend.Task{ID: "t"}},
		{Backend: fakeBackend{"winner"}, Verifier: fakeVerifier{true}, Task: backend.Task{ID: "t"}},
	}
	res, ok := route.Race(context.Background(), cands, nil)
	if !ok {
		t.Fatal("expected a winner")
	}
	if res.Backend != "winner" {
		t.Errorf("winner = %q, want the one whose verifier passed", res.Backend)
	}
}

func TestRaceNonePass(t *testing.T) {
	cands := []route.Candidate{
		{Backend: fakeBackend{"a"}, Verifier: fakeVerifier{false}, Task: backend.Task{ID: "t"}},
		{Backend: fakeBackend{"b"}, Verifier: fakeVerifier{false}, Task: backend.Task{ID: "t"}},
	}
	if _, ok := route.Race(context.Background(), cands, nil); ok {
		t.Error("no candidate passed; ok should be false")
	}
}

// blockingBackend blocks in Run until its ctx is cancelled (a slow loser), recording
// that it observed the cancel. It never passes on its own, so a fast winner must be the
// one to cancel it.
type blockingBackend struct {
	name      string
	cancelled *int32
}

func (b blockingBackend) Name() string { return b.name }
func (b blockingBackend) Run(ctx context.Context, _ backend.Task) (backend.Result, error) {
	select {
	case <-ctx.Done():
		atomic.StoreInt32(b.cancelled, 1)
		return backend.Result{Backend: b.name}, ctx.Err()
	case <-time.After(5 * time.Second):
		// A generous ceiling: if the winner never cancelled us we'd block here, and the
		// test's own assertions fail — never a hang forever.
		return backend.Result{Backend: b.name}, nil
	}
}

// TestRaceCancelsLosers: once a candidate passes, a still-running HIGHER-index loser
// must be cancelled (wasted-compute fix), and the winner is still returned. The winner
// is the LOWER index here: a passer cancels only strictly-higher indices (a lower-index
// candidate could still pass and win, so it is never cut short — that is what preserves
// the lowest-index-passer determinism guarantee; see Race and
// TestRaceLowestIndexPasserWinsDeterministically).
func TestRaceCancelsLosers(t *testing.T) {
	var cancelled int32
	cands := []route.Candidate{
		{Backend: fakeBackend{"winner"}, Verifier: fakeVerifier{true}, Task: backend.Task{ID: "t"}},
		{Backend: blockingBackend{"slow-loser", &cancelled}, Verifier: fakeVerifier{false}, Task: backend.Task{ID: "t"}},
	}
	res, ok := route.Race(context.Background(), cands, nil)
	if !ok || res.Backend != "winner" {
		t.Fatalf("Race = (%q, %v), want (winner, true)", res.Backend, ok)
	}
	if atomic.LoadInt32(&cancelled) != 1 {
		t.Error("the slow higher-index loser must be cancelled once the winner passed (loser cancellation)")
	}
}

// delayedBackend passes after d (respecting ctx) — a "slow winner" that a faster,
// higher-index passer must NOT be allowed to cancel or displace.
type delayedBackend struct {
	name string
	d    time.Duration
}

func (b delayedBackend) Name() string { return b.name }
func (b delayedBackend) Run(ctx context.Context, _ backend.Task) (backend.Result, error) {
	select {
	case <-ctx.Done():
		return backend.Result{Backend: b.name}, ctx.Err()
	case <-time.After(b.d):
		return backend.Result{Backend: b.name}, nil
	}
}

// TestRaceLowestIndexPasserWinsDeterministically: when a LOWER-index candidate would
// pass but finishes SLOWER than a higher-index passer, the lower-index candidate must
// still win — the winner is the lowest-index passer regardless of finish order. A
// whole-race cancel-on-first-pass would cut the slow lower-index candidate short (its
// Run returns ctx.Err()) and let the fast higher-index one win, making the result
// timing-dependent. Index-aware cancellation (a passer cancels only strictly-higher
// indices) never cancels the lowest-index passer, so it always completes and wins.
func TestRaceLowestIndexPasserWinsDeterministically(t *testing.T) {
	cands := []route.Candidate{
		{Backend: delayedBackend{"slow-lowest", 60 * time.Millisecond}, Verifier: fakeVerifier{true}, Task: backend.Task{ID: "t"}},
		{Backend: fakeBackend{"fast-higher"}, Verifier: fakeVerifier{true}, Task: backend.Task{ID: "t"}},
	}
	res, ok := route.Race(context.Background(), cands, nil)
	if !ok {
		t.Fatal("Race must report a pass")
	}
	if res.Backend != "slow-lowest" {
		t.Fatalf("Race = %q, want the lowest-index passer \"slow-lowest\" — a faster higher-index passer must not cancel or displace it", res.Backend)
	}
}

type reviewerModel struct{ reply string }

func (reviewerModel) Model() string { return "rev" }
func (r reviewerModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{Content: []model.Block{{Type: "text", Text: r.reply}}}, nil
}

func TestReviewApprove(t *testing.T) {
	ok, notes, err := route.Review(context.Background(), reviewerModel{`{"approved":true,"notes":"clean"}`}, "diff")
	if err != nil || !ok || notes != "clean" {
		t.Fatalf("review = %v %q %v", ok, notes, err)
	}
}

func TestReviewDeniesOnGarbage(t *testing.T) {
	ok, _, err := route.Review(context.Background(), reviewerModel{"looks fine to me"}, "diff")
	if err != nil || ok {
		t.Error("unparseable review must deny (safe default)")
	}
}

func TestSingleRouter(t *testing.T) {
	def := fakeBackend{"native"}
	got := route.SingleRouter{}.Route(context.Background(), backend.Task{}, def)
	if got.Name() != "native" {
		t.Errorf("SingleRouter should return the default backend")
	}
}
