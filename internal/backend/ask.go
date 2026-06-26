package backend

// ask.go is the attended-only `ask_user` seam: the native loop's one way to put a
// sharp question (or a short sequence of them) to the HUMAN operator and block for
// the answer — for a genuine fork that no safe assumption resolves, or a guess on
// something irreversible/expensive (PERSONA §2). It is wired ONLY when a human is
// synchronously reachable (interactive chat / serve-live); headless runs leave the
// AskUser seam nil, so the tool is never advertised and a stray call fails closed.
//
// Like Peer/Inbox/Wake the interface + value types are declared HERE, in the
// frozen-contract backend package, over backend-owned types — the concrete
// collection machinery lives in internal/ask (which imports these types), so
// backend never imports that leaf and keeps its import graph clean (I1). The
// operator's answer is TRUSTED principal input folded un-guard.Wrap'd (the same
// narrow I7 exception as a steer/user turn), but length-clamped and logged
// metadata-only (I5).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nilcore/internal/model"
)

// ErrAskTimeout is the sentinel an AskHandle.Ask returns when the operator did not
// answer the whole batch within the handle's wall-clock backstop. The answers
// collected so far are still returned (never silently dropped); the loop folds them
// plus a "proceeding on assumptions" note. It is neither a fault nor a cancellation
// — the drive continues — so it is distinct from a ctx cancel (shutdown/Cancel).
var ErrAskTimeout = errors.New("ask_user: operator did not answer in time")

// AskChoice is one labelled option offered for a question. Detail is optional
// one-line context. Labels are model-authored; the harness echoes the chosen
// label(s) back verbatim, never an index (I7 per-field).
type AskChoice struct {
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

// AskQuestion is one question in a batch. Choices may be empty (a pure free-form
// question); MultiSelect lets the operator pick more than one. Custom free-form
// input is ALWAYS available regardless of choices (the implicit "or else").
type AskQuestion struct {
	Prompt      string      `json:"question"`
	Choices     []AskChoice `json:"choices,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
}

// AskAnswer is the resolved answer to one question. Selected holds chosen choice
// LABELS verbatim (model-authored, looked up by index — never operator-typed text);
// Custom holds the operator's free-form text (a trusted principal turn, clamped).
// Both empty ⇒ the operator declined (you-decide).
type AskAnswer struct {
	Selected []string
	Custom   string
}

// AskHandle is the native loop's handle on the attended ask seam. Ask poses a 1–5
// question batch and BLOCKS (honoring ctx) until every question is answered, the
// backstop fires (ErrAskTimeout, with partial answers), or ctx is cancelled. MaxAsks
// is the current per-DRIVE ask ceiling derived from the conversation's ask level (0
// ⇒ asking is off). SetLevel adjusts that level from a natural-language spec the
// operator gave ("less"/"more"/"off"/"normal"/"0".."6") and returns a short ack.
//
// It is satisfied by a session-side adapter over *ask.Box (the collection lives in
// internal/ask); declaring it here keeps backend import-leaf, exactly like Peer.
type AskHandle interface {
	Ask(ctx context.Context, qs []AskQuestion) ([]AskAnswer, error)
	MaxAsks() int
	SetLevel(spec string) (ack string, err error)
}

// askGuidance is appended to the worker prompt ONLY when an AskUser seam is wired
// (attended). It is the wired half of PERSONA §2: default to acting, ask sparingly,
// batch only INDEPENDENT questions. When no seam is wired the base prompt's headless
// "act on your best assumption" line stands and no ask tool exists, so the model is
// never told to ask a human it cannot reach. shellNote is dropped in read-only modes
// (no shell to probe with) by composing the variant without it.
const askGuidance = `A human operator is present and reachable. DEFAULT TO ACTING: proceed on reasonable
assumptions and STATE them so they can be corrected. Use the "ask_user" tool ONLY
when the work genuinely forks and no safe assumption resolves it, or when proceeding
would require guessing on something irreversible or expensive — never to confirm
reversible work, and never to re-ask something the conversation, task, or files
already answer. Prefer ONE sharp question; you may ask up to 5 at once, but batch
ONLY questions that are INDEPENDENT (all asked before you see any answer) — if a
question depends on the answer to another, ask the first alone, then call ask_user
again. Offer 2–4 concrete labelled choices when they fit (set multiSelect to let the
operator pick several); the operator may always type a free-form answer instead. Your
ask budget per drive is limited, so spend it only on decisions that truly need a human.`

const askShellProbeNote = ` When something a cheap reversible probe (read a file, run a command) would settle, do that instead of asking.`

// askUserToolDef is the ask_user tool, advertised only when an AskUser seam is wired.
func askUserToolDef() model.Tool {
	return model.Tool{
		Name: "ask_user",
		Description: "Ask the human operator 1–5 sharp questions (shown one after another) and BLOCK until " +
			"they answer. Use ONLY for a genuine fork no safe assumption resolves, or a guess on something " +
			"irreversible or expensive — never to confirm reversible work. Each question may offer 2–4 labelled " +
			"choices (set multiSelect to allow several) and the operator may always answer free-form. Batch ONLY " +
			"independent questions; for a dependent follow-up, call ask_user again. Returns the operator's answers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","properties":{"question":{"type":"string"},"choices":{"type":"array","minItems":0,"maxItems":6,"items":{"type":"object","properties":{"label":{"type":"string"},"detail":{"type":"string"}},"required":["label"]}},"multiSelect":{"type":"boolean"}},"required":["question"]}}},"required":["questions"]}`),
	}
}

// setAskLevelToolDef is the set_ask_level tool: it lets the model honor a spoken
// request to ask more or fewer questions ("ask me less") by moving the conversation's
// ask level one notch, or to a named/numbered level. Advertised with ask_user.
func setAskLevelToolDef() model.Tool {
	return model.Tool{
		Name: "set_ask_level",
		Description: "Adjust how often you may ask the operator questions, when THEY ask you to (e.g. 'ask me " +
			"fewer/more questions'). spec is 'less' or 'more' (one notch), 'off', 'normal', or a level 'off'..'max'. " +
			"Returns the new level. Use only on the operator's explicit request — do not change it on your own.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"spec":{"type":"string"}},"required":["spec"]}`),
	}
}

// askUserInput is the decoded ask_user tool input.
type askUserInput struct {
	Questions []AskQuestion `json:"questions"`
}

// validateAskQuestions enforces the decode-time contract (1–5 questions; each 0–6
// choices with non-empty unique labels; non-empty prompt; a one-choice menu is
// promoted to free-form). It returns the cleaned questions or a model-facing reason
// — surfaced as an errorResult so the model self-corrects rather than the loop
// parking on a malformed ask.
func validateAskQuestions(qs []AskQuestion) ([]AskQuestion, string) {
	if len(qs) == 0 {
		return nil, "ask_user: provide 1–5 questions"
	}
	if len(qs) > 5 {
		return nil, fmt.Sprintf("ask_user: too many questions (%d) — ask at most 5 at once", len(qs))
	}
	out := make([]AskQuestion, 0, len(qs))
	for i, q := range qs {
		if strings.TrimSpace(q.Prompt) == "" {
			return nil, fmt.Sprintf("ask_user: question %d has an empty prompt", i+1)
		}
		if len(q.Choices) > 6 {
			return nil, fmt.Sprintf("ask_user: question %d has too many choices (max 6)", i+1)
		}
		seen := map[string]bool{}
		clean := make([]AskChoice, 0, len(q.Choices))
		for _, c := range q.Choices {
			lbl := strings.TrimSpace(c.Label)
			if lbl == "" {
				return nil, fmt.Sprintf("ask_user: question %d has an empty choice label", i+1)
			}
			if seen[lbl] {
				return nil, fmt.Sprintf("ask_user: question %d has duplicate choice label %q", i+1, lbl)
			}
			seen[lbl] = true
			clean = append(clean, AskChoice{Label: lbl, Detail: strings.TrimSpace(c.Detail)})
		}
		// A menu of one is malformed — promote it to a pure free-form question so the
		// operator is not offered a single take-it-or-leave-it choice.
		if len(clean) == 1 {
			clean = nil
		}
		out = append(out, AskQuestion{Prompt: q.Prompt, Choices: clean, MultiSelect: q.MultiSelect && len(clean) > 0})
	}
	return out, ""
}

// formatAskResult renders the operator's answers as the single tool_result block the
// model reads. Labels are quoted so a comma-bearing label is unambiguous; the model
// sees only resolved strings, never indices or the AskAnswer struct. partial marks a
// backstop timeout so the model knows some answers are assumptions-to-proceed.
func formatAskResult(qs []AskQuestion, answers []AskAnswer, partial bool) string {
	var b strings.Builder
	answered := 0
	for _, a := range answers {
		if len(a.Selected) > 0 || strings.TrimSpace(a.Custom) != "" {
			answered++
		}
	}
	if partial {
		fmt.Fprintf(&b, "operator answered %d of %d questions before timing out — proceed on your best assumptions for the rest:\n", answered, len(qs))
	} else {
		fmt.Fprintf(&b, "operator answered %d question(s):\n", len(qs))
	}
	for i, q := range qs {
		fmt.Fprintf(&b, "Q%d. %s\n", i+1, strings.TrimSpace(q.Prompt))
		var a AskAnswer
		if i < len(answers) {
			a = answers[i]
		}
		switch {
		case len(a.Selected) > 0 && strings.TrimSpace(a.Custom) != "":
			fmt.Fprintf(&b, "   → chose: %s  (note: %s)\n", quoteJoin(a.Selected), strings.TrimSpace(a.Custom))
		case len(a.Selected) > 0:
			fmt.Fprintf(&b, "   → chose: %s\n", quoteJoin(a.Selected))
		case strings.TrimSpace(a.Custom) != "":
			fmt.Fprintf(&b, "   → %q\n", strings.TrimSpace(a.Custom))
		default:
			fmt.Fprintf(&b, "   → declined (you decide)\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// quoteJoin renders selected labels as a quoted, comma-separated list so a label
// that itself contains a comma stays unambiguous to the model.
func quoteJoin(labels []string) string {
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = fmt.Sprintf("%q", l)
	}
	return strings.Join(parts, ", ")
}
