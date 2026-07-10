package backend

// fixreview_test.go covers the native-loop defect-review fixes:
//   - #1  an empty-content model reply never poisons history (would 400 the next call)
//   - LOW a finish-but-verifier-failed turn keeps co-emitted tool images
//   - LOW an output-cap 400 degrades (halve + retry) instead of killing the run

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/tools"
)

// --- #1: empty-content reply must not poison history -------------------------

func TestNativeEmptyContentReplyDoesNotPoisonHistory(t *testing.T) {
	// A reply with err==nil, no tool calls, and ZERO content blocks (e.g. a native
	// web-search-only turn the provider decoded to empty) used to be appended verbatim
	// as an assistant turn, which marshals to "content":null and 400s the NEXT request
	// — killing the run and discarding the worktree. The loop must instead keep history
	// marshalable and make progress.
	m := &toolCapturingModel{responses: []model.Response{
		{Content: nil, StopReason: "end_turn"},
		{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5}
	res, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"})
	if err != nil {
		t.Fatalf("an empty-content reply must not kill the run: %v", err)
	}
	if !res.SelfClaimed {
		t.Error("loop should recover from the empty reply and finish")
	}
	if m.i < 2 {
		t.Errorf("loop should continue to a 2nd model call, got %d call(s)", m.i)
	}
	// The history handed to the SECOND call must contain NO empty/null-content assistant
	// turn — the exact shape that 400s the real provider.
	for _, msg := range m.lastMsgs {
		if msg.Role == "assistant" && len(msg.Content) == 0 {
			t.Errorf("an empty-content assistant turn poisoned history: %+v", m.lastMsgs)
		}
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("history turn must stay marshalable: %v", err)
		}
		if msg.Role == "assistant" && strings.Contains(string(b), `"content":null`) {
			t.Errorf("assistant turn marshals to null content (would 400 the next call): %s", b)
		}
	}
}

func TestNativeEmptyRepliesThenBudgetExhaustionIsClean(t *testing.T) {
	// A model that only ever returns empty content must exhaust the budget cleanly,
	// never dying with a marshal/transport error mid-run.
	m := &toolCapturingModel{} // out of responses ⇒ always {StopReason:"end_turn"} empty
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 3}
	res, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"})
	if err != nil {
		t.Fatalf("repeated empty replies must not error: %v", err)
	}
	if res.SelfClaimed {
		t.Error("nothing finished; SelfClaimed must be false")
	}
	if !strings.Contains(res.Summary, "budget exhausted") {
		t.Errorf("summary = %q, want budget-exhausted", res.Summary)
	}
}

// --- LOW: finish + co-emitted image, verifier fails, image is kept -----------

type snapTool struct{}

func (snapTool) Name() string            { return "snap" }
func (snapTool) Description() string     { return "snap" }
func (snapTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (snapTool) Run(context.Context, string, json.RawMessage) (string, error) {
	return "snapped", nil
}
func (snapTool) RunWithImage(context.Context, string, json.RawMessage) (string, *tools.Image, error) {
	return "snapped", &tools.Image{MediaType: "image/png", Base64: "SNAP64"}, nil
}

func TestNativeFinishFailKeepsCoEmittedImage(t *testing.T) {
	// finish co-emitted with an image tool (a browser screenshot) in the same turn; the
	// verifier fails, so the loop folds the tool_results back for another attempt. The
	// screenshot must ride along so the vision model sees what rendered when it retries.
	m := &toolCapturingModel{responses: []model.Response{
		{Content: []model.Block{
			toolUse("u1", "snap", map[string]string{}),
			toolUse("u2", "finish", map[string]string{"summary": "try 1"}),
		}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u3", "finish", map[string]string{"summary": "try 2"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: &flakyVerifier{failFirst: 1},
		Tools: tools.NewRegistry(snapTool{}), MaxSteps: 6}
	res, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.SelfClaimed {
		t.Fatal("loop should pass on the second attempt")
	}
	sawImage := false
	for _, msg := range m.lastMsgs {
		for _, b := range msg.Content {
			if b.Type == "image" && b.Source != nil && b.Source.Data == "SNAP64" {
				sawImage = true
			}
		}
	}
	if !sawImage {
		t.Error("the co-emitted screenshot was dropped from the verifier-fail user turn")
	}
}

// --- LOW: output-cap 400 degrades instead of dying ---------------------------

// capThenFinishModel returns an output-cap 400 on its FIRST call, then finishes,
// recording the maxTokens it was handed each call.
type capThenFinishModel struct {
	calls   int
	maxSeen []int
}

func (m *capThenFinishModel) Model() string { return "fake" }
func (m *capThenFinishModel) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, maxTok int) (model.Response, error) {
	m.calls++
	m.maxSeen = append(m.maxSeen, maxTok)
	if m.calls == 1 {
		return model.Response{}, model.NewAPIError(400, "invalid_request_error", "",
			"max_tokens: 16384 > 8192, which is the maximum number of output tokens for this model", "")
	}
	return model.Response{Content: []model.Block{toolUse("u1", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"}, nil
}

func TestNativeOutputCapDegradesInsteadOfDying(t *testing.T) {
	m := &capThenFinishModel{}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5} // default cap 16384
	res, err := n.Run(context.Background(), Task{ID: "t", Goal: "x"})
	if err != nil {
		t.Fatalf("an output-cap 400 must degrade, not kill the run: %v", err)
	}
	if !res.SelfClaimed {
		t.Error("loop should retry with a smaller cap and finish")
	}
	if len(m.maxSeen) != 2 || m.maxSeen[0] != 16384 || m.maxSeen[1] != 8192 {
		t.Errorf("maxTokens per call = %v, want [16384 8192] (halved on the retry)", m.maxSeen)
	}
}

func TestIsOutputCapError(t *testing.T) {
	capErr := model.NewAPIError(400, "invalid_request_error", "",
		"max_tokens: 16384 > 8192, which is the maximum allowed for this model", "")
	overflow := model.NewAPIError(400, "invalid_request_error", "",
		"input length and max_tokens exceed context limit: 205000 tokens > 200000", "")
	generic := model.NewAPIError(400, "invalid_request_error", "", "temperature must be between 0 and 2", "")
	rate := model.NewAPIError(429, "rate_limit_error", "", "max_tokens exceeded slow down", "")

	if !isOutputCapError(capErr) {
		t.Error("a max_tokens-too-large 400 must be recognized")
	}
	if isOutputCapError(overflow) {
		t.Error("a context-overflow 400 must NOT be treated as an output-cap error (it is compacted)")
	}
	if isOutputCapError(generic) {
		t.Error("a generic 400 must not match")
	}
	if isOutputCapError(rate) {
		t.Error("a 429 must not match (not a 400/422)")
	}
}

func TestReduceOutputCap(t *testing.T) {
	capErr := model.NewAPIError(400, "invalid_request_error", "", "max_tokens is greater than the maximum", "")
	generic := model.NewAPIError(400, "invalid_request_error", "", "bad temperature", "")

	cases := []struct {
		maxTok, streak, wantNext int
		wantOK                   bool
	}{
		{16384, 0, 8192, true},
		{8192, 1, 4096, true},
		{2048, 3, 1024, true},
		{2048, 4, 0, false},  // streak exhausted
		{1024, 0, 0, false},  // already at the floor
		{16384, 4, 0, false}, // streak exhausted
	}
	for _, c := range cases {
		next, ok := reduceOutputCap(c.maxTok, c.streak, capErr)
		if ok != c.wantOK || (ok && next != c.wantNext) {
			t.Errorf("reduceOutputCap(%d,%d) = (%d,%v), want (%d,%v)", c.maxTok, c.streak, next, ok, c.wantNext, c.wantOK)
		}
	}
	if _, ok := reduceOutputCap(16384, 0, generic); ok {
		t.Error("reduceOutputCap must not fire for a non-cap error")
	}
}
