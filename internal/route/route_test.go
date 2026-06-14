package route_test

import (
	"context"
	"testing"

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
