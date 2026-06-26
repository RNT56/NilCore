package super

// ask.go is the supervisor's attended human-clarification seam — the multi-agent
// analogue of the native loop's ask_user. The supervisor (the run's root planner) can
// pose 1–5 sharp questions to the operator at a round boundary and block for the answer.
// The value types are super-LOCAL (super does not import backend — the same leaf
// discipline as the Answer/Inbox seams); the wiring site adapts them to the SAME session
// ask box the native loop uses, so the question renders through every per-surface UI and
// the answer routes back through the one authorized path. nil seam ⇒ the tool is never
// advertised (headless-safe). resolveReply stays the sole answer parser (session-side);
// here the formatted answers are simply read back to the planner as a tool_result.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
)

const toolAskUser = "ask_user"

// AskChoice / AskQuestion / AskAnswer are the super-local mirror of the question/answer
// shapes (kept out of backend to keep super an import-leaf).
type AskChoice struct{ Label, Detail string }
type AskQuestion struct {
	Prompt      string
	Choices     []AskChoice
	MultiSelect bool
}
type AskAnswer struct {
	Selected []string
	Custom   string
}

// AskFunc poses a batch of questions to the human and blocks until answered (or the
// session-side backstop fires), returning one answer per question.
type AskFunc func(ctx context.Context, qs []AskQuestion) ([]AskAnswer, error)

// askUserToolDef is the supervisor's ask_user tool, advertised only when AskUser is
// wired. The schema mirrors the native loop's (1–5 questions, 0–6 labelled choices,
// per-question multiSelect, always-available free-form).
func askUserToolDef() model.Tool {
	return model.Tool{
		Name: toolAskUser,
		Description: "Ask the human operator 1–5 sharp questions (shown one after another) and BLOCK until " +
			"they answer. Use ONLY for a genuine planning fork no safe assumption resolves, or a guess on " +
			"something irreversible or expensive — never to confirm reversible work. Each question may offer 2–4 " +
			"labelled choices (set multiSelect to allow several); the operator may also answer free-form.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","properties":{"question":{"type":"string"},"choices":{"type":"array","minItems":0,"maxItems":6,"items":{"type":"object","properties":{"label":{"type":"string"},"detail":{"type":"string"}},"required":["label"]}},"multiSelect":{"type":"boolean"}},"required":["question"]}}},"required":["questions"]}`),
	}
}

type askUserInput struct {
	Questions []struct {
		Question string `json:"question"`
		Choices  []struct {
			Label  string `json:"label"`
			Detail string `json:"detail"`
		} `json:"choices"`
		MultiSelect bool `json:"multiSelect"`
	} `json:"questions"`
}

// doAskUser decodes the ask, poses it via the AskUser seam, and returns the operator's
// answers as a tool_result. nil seam fails closed (unknown tool); the answer is the
// trusted principal exception (un-fenced).
func (s *Supervisor) doAskUser(ctx context.Context, b model.Block) model.Block {
	if s.AskUser == nil {
		return errf(b.ID, "unknown tool: "+toolAskUser)
	}
	var in askUserInput
	if json.Unmarshal(b.Input, &in) != nil || len(in.Questions) == 0 {
		return errf(b.ID, "ask_user: provide 1–5 questions")
	}
	if len(in.Questions) > 5 {
		return errf(b.ID, fmt.Sprintf("ask_user: too many questions (%d) — ask at most 5", len(in.Questions)))
	}
	qs := make([]AskQuestion, 0, len(in.Questions))
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Question) == "" {
			return errf(b.ID, "ask_user: a question has an empty prompt")
		}
		ch := make([]AskChoice, 0, len(q.Choices))
		for _, c := range q.Choices {
			if strings.TrimSpace(c.Label) != "" {
				ch = append(ch, AskChoice{Label: c.Label, Detail: c.Detail})
			}
		}
		qs = append(qs, AskQuestion{Prompt: q.Question, Choices: ch, MultiSelect: q.MultiSelect && len(ch) > 0})
	}
	answers, err := s.AskUser(ctx, qs)
	if err != nil {
		return errf(b.ID, "ask_user: "+err.Error())
	}
	s.Log.Append(eventlog.Event{Task: supervisorTask, Kind: "super_ask_user",
		Detail: map[string]any{"questions": len(qs)}})
	return ok(b.ID, formatSuperAsk(qs, answers))
}

// formatSuperAsk renders the answers for the planner — quoted labels (comma-safe), the
// custom note if any, or "declined".
func formatSuperAsk(qs []AskQuestion, answers []AskAnswer) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "operator answered %d question(s):\n", len(qs))
	for i, q := range qs {
		fmt.Fprintf(&sb, "Q%d. %s\n", i+1, strings.TrimSpace(q.Prompt))
		var a AskAnswer
		if i < len(answers) {
			a = answers[i]
		}
		switch {
		case len(a.Selected) > 0 && strings.TrimSpace(a.Custom) != "":
			fmt.Fprintf(&sb, "   → chose: %s  (note: %s)\n", quoteJoin(a.Selected), strings.TrimSpace(a.Custom))
		case len(a.Selected) > 0:
			fmt.Fprintf(&sb, "   → chose: %s\n", quoteJoin(a.Selected))
		case strings.TrimSpace(a.Custom) != "":
			fmt.Fprintf(&sb, "   → %q\n", strings.TrimSpace(a.Custom))
		default:
			fmt.Fprintf(&sb, "   → declined (you decide)\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func quoteJoin(labels []string) string {
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = fmt.Sprintf("%q", l)
	}
	return strings.Join(parts, ", ")
}
