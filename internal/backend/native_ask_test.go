package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// recordModel records the msgs and tool defs of every Complete call so a test can
// assert what the model was advertised and what tool_results it was fed back.
type recordModel struct {
	responses []model.Response
	i         int
	calls     [][]model.Message
	defs      [][]model.Tool
}

func (m *recordModel) Model() string { return "fake" }
func (m *recordModel) Complete(_ context.Context, _ string, msgs []model.Message, defs []model.Tool, _ int) (model.Response, error) {
	m.calls = append(m.calls, msgs)
	m.defs = append(m.defs, defs)
	if m.i >= len(m.responses) {
		return model.Response{StopReason: "end_turn"}, nil
	}
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// fakeAsker is a scripted AskHandle.
type fakeAsker struct {
	answers []AskAnswer
	err     error
	max     int
	gotQs   []AskQuestion
	calls   int
	level   string
}

func (f *fakeAsker) Ask(_ context.Context, qs []AskQuestion) ([]AskAnswer, error) {
	f.calls++
	f.gotQs = qs
	return f.answers, f.err
}
func (f *fakeAsker) MaxAsks() int { return f.max }
func (f *fakeAsker) SetLevel(spec string) (string, error) {
	f.level = spec
	return "ask level: " + spec, nil
}

func askToolUse(id string, qs []AskQuestion) model.Block {
	in, _ := json.Marshal(askUserInput{Questions: qs})
	return model.Block{Type: "tool_use", ID: id, Name: "ask_user", Input: in}
}

func defNames(defs []model.Tool) map[string]bool {
	m := map[string]bool{}
	for _, d := range defs {
		m[d.Name] = true
	}
	return m
}

// allToolResults returns the concatenated text of every tool_result block across all
// recorded calls, so a test can assert what the model was fed back.
func allToolResults(calls [][]model.Message) string {
	var b strings.Builder
	for _, msgs := range calls {
		for _, m := range msgs {
			for _, blk := range m.Content {
				if blk.Type == "tool_result" {
					b.WriteString(blk.Content)
					b.WriteString("\n")
				}
			}
		}
	}
	return b.String()
}

// TestAskUserAdvertisedIffWired: ask_user / set_ask_level appear in the tool defs only
// when an AskUser seam is wired; the headless loop never sees them.
func TestAskUserAdvertisedIffWired(t *testing.T) {
	mk := func(ask AskHandle) *recordModel {
		m := &recordModel{responses: []model.Response{
			{Content: []model.Block{toolUse("f", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
		}}
		n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 3, AskUser: ask}
		if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
			t.Fatalf("run: %v", err)
		}
		return m
	}
	with := defNames(mk(&fakeAsker{max: 3}).defs[0])
	if !with["ask_user"] || !with["set_ask_level"] {
		t.Fatalf("attended loop should advertise ask_user + set_ask_level, got %v", with)
	}
	without := defNames(mk(nil).defs[0])
	if without["ask_user"] || without["set_ask_level"] {
		t.Fatalf("headless loop must NOT advertise ask tools, got %v", without)
	}
}

// TestAskUserAnswerFlowsBack: the operator's answer comes back as the tool_result for
// the ask_user call, and the same drive continues to finish.
func TestAskUserAnswerFlowsBack(t *testing.T) {
	asker := &fakeAsker{max: 3, answers: []AskAnswer{{Selected: []string{"Postgres"}, Custom: "but only for prod"}}}
	m := &recordModel{responses: []model.Response{
		{Content: []model.Block{askToolUse("u1", []AskQuestion{{Prompt: "which db?", Choices: []AskChoice{{Label: "Postgres"}, {Label: "SQLite"}}}})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5, AskUser: asker}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if asker.calls != 1 || len(asker.gotQs) != 1 {
		t.Fatalf("asker should be called once with one question, calls=%d qs=%d", asker.calls, len(asker.gotQs))
	}
	res := allToolResults(m.calls)
	if !strings.Contains(res, `"Postgres"`) || !strings.Contains(res, "but only for prod") {
		t.Fatalf("answer not folded back as tool_result; got:\n%s", res)
	}
}

// TestAskUserCoEmissionRejected: ask_user emitted alongside another tool refuses (so a
// half-built turn never freezes behind a human wait) while the co-emitted tool runs.
func TestAskUserCoEmissionRejected(t *testing.T) {
	asker := &fakeAsker{max: 3, answers: []AskAnswer{{Custom: "x"}}}
	box := &recordingBox{}
	m := &recordModel{responses: []model.Response{
		{Content: []model.Block{
			askToolUse("u1", []AskQuestion{{Prompt: "q"}}),
			toolUse("u2", "run", map[string]string{"cmd": "echo hi"}),
		}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u3", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: box, Verifier: okVerifier{}, MaxSteps: 5, AskUser: asker}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if asker.calls != 0 {
		t.Fatalf("ask_user must NOT block when co-emitted; calls=%d", asker.calls)
	}
	if len(box.execed) != 1 {
		t.Fatalf("co-emitted run should still execute, execed=%v", box.execed)
	}
	if !strings.Contains(allToolResults(m.calls), "only the only tool call") && !strings.Contains(allToolResults(m.calls), "must be the only tool") {
		t.Fatalf("ask_user should return a co-emission error; got:\n%s", allToolResults(m.calls))
	}
}

// TestAskUserNilFailsClosed: a hallucinated ask_user in a headless (nil-seam) loop
// returns unknown-tool and NEVER blocks — the structural never-block guarantee.
func TestAskUserNilFailsClosed(t *testing.T) {
	m := &recordModel{responses: []model.Response{
		{Content: []model.Block{askToolUse("u1", []AskQuestion{{Prompt: "q"}})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5} // AskUser nil
	res, err := n.Run(context.Background(), Task{ID: "t", Goal: "g"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.SelfClaimed {
		t.Fatal("loop should have completed (never blocked) on a stray ask_user")
	}
	if !strings.Contains(allToolResults(m.calls), "unknown tool: ask_user") {
		t.Fatalf("stray ask_user should fail closed; got:\n%s", allToolResults(m.calls))
	}
}

// TestAskUserBudgetOff: at level off (MaxAsks 0) ask_user refuses without ever calling
// the asker (the model is told to proceed on assumptions).
func TestAskUserBudgetOff(t *testing.T) {
	asker := &fakeAsker{max: 0}
	m := &recordModel{responses: []model.Response{
		{Content: []model.Block{askToolUse("u1", []AskQuestion{{Prompt: "q"}})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5, AskUser: asker}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if asker.calls != 0 {
		t.Fatalf("ask_user must not call the asker at level off; calls=%d", asker.calls)
	}
	if !strings.Contains(allToolResults(m.calls), "turned off") {
		t.Fatalf("expected an 'asking off' tool_result; got:\n%s", allToolResults(m.calls))
	}
}

// TestSetAskLevelDispatch: the set_ask_level tool reaches the asker's SetLevel.
func TestSetAskLevelDispatch(t *testing.T) {
	asker := &fakeAsker{max: 3}
	m := &recordModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "set_ask_level", map[string]string{"spec": "less"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: &recordingBox{}, Verifier: okVerifier{}, MaxSteps: 5, AskUser: asker}
	if _, err := n.Run(context.Background(), Task{ID: "t", Goal: "g"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if asker.level != "less" {
		t.Fatalf("set_ask_level should pass spec to SetLevel, got %q", asker.level)
	}
}

// TestAskGuidanceGatedOnSeam: the prompt licenses asking only when the seam is wired,
// and the base prompt no longer categorically forbids asking.
func TestAskGuidanceGatedOnSeam(t *testing.T) {
	if strings.Contains(systemPrompt, "Do not ask the user questions") {
		t.Fatal("base systemPrompt should no longer forbid asking outright")
	}
	headless := (&Native{}).systemFor()
	if strings.Contains(headless, "ask_user") {
		t.Fatalf("headless systemFor must not mention ask_user:\n%s", headless)
	}
	attended := (&Native{AskUser: &fakeAsker{}}).systemFor()
	if !strings.Contains(attended, "ask_user") {
		t.Fatalf("attended systemFor should include the ask guidance:\n%s", attended)
	}
	// Mode-aware: the shell-probe clause is present with a shell, dropped without one.
	if !strings.Contains(attended, "cheap reversible probe") {
		t.Fatal("attended (shell-on) guidance should mention the probe alternative")
	}
	readOnly := (&Native{AskUser: &fakeAsker{}, DisableShell: true}).systemFor()
	if strings.Contains(readOnly, "cheap reversible probe") {
		t.Fatal("read-only (shell-off) guidance must not promise a run-a-command probe")
	}
}

// TestValidateAskQuestions exercises the decode-time contract.
func TestValidateAskQuestions(t *testing.T) {
	if _, reason := validateAskQuestions(nil); reason == "" {
		t.Fatal("empty batch should be rejected")
	}
	if _, reason := validateAskQuestions(make([]AskQuestion, 6)); reason == "" {
		t.Fatal(">5 questions should be rejected")
	}
	if _, reason := validateAskQuestions([]AskQuestion{{Prompt: " "}}); reason == "" {
		t.Fatal("empty prompt should be rejected")
	}
	if _, reason := validateAskQuestions([]AskQuestion{{Prompt: "q", Choices: []AskChoice{{Label: "a"}, {Label: "a"}}}}); reason == "" {
		t.Fatal("duplicate labels should be rejected")
	}
	// A one-choice menu is promoted to free-form.
	got, reason := validateAskQuestions([]AskQuestion{{Prompt: "q", Choices: []AskChoice{{Label: "only"}}, MultiSelect: true}})
	if reason != "" {
		t.Fatalf("one-choice should be accepted (promoted), got %q", reason)
	}
	if len(got[0].Choices) != 0 || got[0].MultiSelect {
		t.Fatalf("one-choice question should be promoted to free-form, got %+v", got[0])
	}
}
