package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

type recordingBox struct{ execed []string }

func (r *recordingBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	r.execed = append(r.execed, cmd)
	return sandbox.Result{Stdout: "ok"}, nil
}
func (r *recordingBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return r.Exec(ctx, cmd)
}
func (r *recordingBox) Workdir() string { return "/work" }

type scriptModel struct {
	responses []model.Response
	i         int
}

func (s *scriptModel) Model() string { return "fake" }
func (s *scriptModel) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	if s.i >= len(s.responses) {
		return model.Response{StopReason: "end_turn"}, nil
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}

type okVerifier struct{}

func (okVerifier) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: true, Output: "ok"}, nil
}

func toolUse(id, name string, in map[string]string) model.Block {
	b, _ := json.Marshal(in)
	return model.Block{Type: "tool_use", ID: id, Name: name, Input: b}
}

func TestNativeDeniedCommandNotExecuted(t *testing.T) {
	box := &recordingBox{}
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "rm -rf /"})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{
		Model:        m,
		Box:          box,
		Verifier:     okVerifier{},
		CommandGuard: policy.DefaultCommandPolicy().Check,
		MaxSteps:     5,
	}

	res, err := n.Run(context.Background(), Task{ID: "t1", Goal: "x", Dir: "/work"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, c := range box.execed {
		if strings.Contains(c, "rm -rf /") {
			t.Fatalf("denied command was executed: %q", c)
		}
	}
	if !res.SelfClaimed {
		t.Error("loop should still finish after the denied call returned an error")
	}
}

func TestNativeAllowedCommandRuns(t *testing.T) {
	box := &recordingBox{}
	m := &scriptModel{responses: []model.Response{
		{Content: []model.Block{toolUse("u1", "run", map[string]string{"cmd": "go test ./..."})}, StopReason: "tool_use"},
		{Content: []model.Block{toolUse("u2", "finish", map[string]string{"summary": "done"})}, StopReason: "tool_use"},
	}}
	n := &Native{Model: m, Box: box, Verifier: okVerifier{}, CommandGuard: policy.DefaultCommandPolicy().Check, MaxSteps: 5}
	if _, err := n.Run(context.Background(), Task{ID: "t1", Goal: "x"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ran bool
	for _, c := range box.execed {
		if strings.Contains(c, "go test") {
			ran = true
		}
	}
	if !ran {
		t.Error("allowed command should have executed")
	}
}
