package project

// judge.go is the hierarchical verifier-as-judge (docs/MULTI-AGENT.md §5, I2). Two
// rules are absolute and tested:
//
//   - Done is an EXIT-CODE AND. JudgeProject runs the project VerifyCmd and EVERY
//     acceptance Criterion command; done ⟺ all of them exit 0. No LLM text — not a
//     proposal, not a "looks done", not a summary — ever participates in the
//     verdict. The model proposes the bar; the sandbox decides whether it is met.
//   - The bar is ADD-ONLY. DeriveAcceptance has the advisor PROPOSE criteria, then
//     DRY-RUNS each proposed command in the sandbox and DROPS any that is unrunnable
//     (a typo, a missing tool, a non-zero "command not found"). Refinement only ever
//     ADDS runnable criteria to the existing set — it never silently lowers the bar
//     by removing one. So a compromised or sloppy proposal can at most fail to raise
//     the bar; it can never weaken it.

import (
	"context"
	"strings"

	"nilcore/internal/advisor"
	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
	"nilcore/internal/summarize"
	"nilcore/internal/verify"
)

// JudgeProject is the project-level done-authority: it runs the project verifier
// (built from VerifyCmd over the repo) AND every Criterion's command, and reports
// done ⟺ ALL exit 0, plus the count still unmet. It is a pure exit-code AND — the
// Criterion.Description (LLM prose) is never read. A verifier transport error
// counts as unmet (a check we could not run is NOT a pass), so an infrastructure
// failure can never masquerade as done.
//
// projVerifier is the project's own VerifyCmd verifier; criteria carry their own
// per-command verifiers (resolved by DeriveAcceptance against the sandbox). Passing
// the project verifier explicitly (rather than reading it off a field) keeps this a
// pure function the loop and tests can drive identically.
func JudgeProject(ctx context.Context, projVerifier verify.Verifier, criteria []Criterion) (done bool, unmet int) {
	done = true

	// The project VerifyCmd is the base gate. An error or a non-zero exit is unmet.
	if projVerifier != nil {
		rep, err := projVerifier.Check(ctx)
		if err != nil || !rep.Passed {
			done = false
			unmet++
		}
	}

	// Each criterion is an independent exit-code gate over its own command. A
	// criterion with no verifier (empty Command, covered by VerifyCmd) is not an
	// independent gate and is skipped — it was already dropped at derivation, but we
	// guard here too so a hand-built criterion can never gate on nothing.
	for _, c := range criteria {
		if c.Verifier == nil || strings.TrimSpace(c.Command) == "" {
			continue
		}
		rep, err := c.Verifier.Check(ctx)
		if err != nil || !rep.Passed {
			done = false
			unmet++
		}
	}
	return done, unmet
}

// judge runs JudgeProject over the loop's current state: it builds the project
// verifier for the repo from VerifyCmd and judges it against the derived criteria.
// It is the single call site the loop uses for both done-detection and the unmet
// snapshot, so the two can never disagree.
func (l *Loop) judge(ctx context.Context, st State) (done bool, unmet int) {
	var pv verify.Verifier
	if l.Verifier != nil {
		pv = l.Verifier(st.Repo)
	}
	return JudgeProject(ctx, pv, st.Criteria)
}

// DeriveAcceptance turns a goal into a set of runnable acceptance criteria: the
// advisor PROPOSES criteria; each proposed command is DRY-RUN in the sandbox and
// DROPPED if unrunnable; the survivors are folded into the EXISTING set ADD-ONLY
// (an existing criterion is never removed, so the bar never silently lowers).
//
// box is the sandbox the project verifier runs in; the per-criterion verifiers
// resolved here are verify.CommandVerifier over that same box, so JudgeProject runs
// each criterion exactly as the project's own checks run. A nil advisor or a
// proposal failure yields the existing criteria unchanged (no bar change), never an
// error — derivation degrades to "keep the current bar", it never aborts the loop.
//
// "Dry-run" here means: actually execute the command in the sandbox once and treat
// a runnable command (the box returned a Result without a transport error) as
// keepable. We deliberately do NOT require exit 0 at derivation — a brand-new,
// currently-RED acceptance command is exactly what we want to keep (it is supposed
// to fail until the feature lands). We drop only commands the sandbox could not run
// at all (a malformed command, a missing interpreter): those can never gate
// meaningfully, so keeping them would wedge the loop on a permanently-red check.
func DeriveAcceptance(ctx context.Context, adv *advisor.Advisor, box sandbox.Sandbox,
	goal string, existing []Criterion, log *eventlog.Log) []Criterion {

	if adv == nil || box == nil {
		return existing
	}

	proposed, err := proposeCriteria(ctx, adv, goal)
	if err != nil || len(proposed) == 0 {
		// No new bar to add: keep the existing one exactly (add-only ⟹ never lower).
		return existing
	}

	// Index existing commands so refinement is genuinely add-only and idempotent: a
	// re-proposed command already in the set is not duplicated.
	have := map[string]bool{}
	for _, c := range existing {
		have[normalizeCmd(c.Command)] = true
	}

	out := append([]Criterion(nil), existing...)
	added, dropped := 0, 0
	for _, p := range proposed {
		cmd := strings.TrimSpace(p.Command)
		if cmd == "" {
			continue // "covered by VerifyCmd" — no independent gate to add
		}
		key := normalizeCmd(cmd)
		if have[key] {
			continue // already in the bar; add-only and idempotent
		}
		if !dryRunnable(ctx, box, cmd) {
			dropped++
			continue // unrunnable proposal — dropped, never gates
		}
		have[key] = true
		out = append(out, Criterion{
			Description: p.Description,
			Command:     cmd,
			Verifier:    verify.New(box, cmd),
		})
		added++
	}

	if log != nil {
		log.Append(eventlog.Event{Task: projectTask, Kind: "project_acceptance",
			Detail: map[string]any{"proposed": len(proposed), "added": added,
				"dropped": dropped, "total": len(out)}})
	}
	return out
}

// proposal is one advisor-proposed acceptance criterion, parsed from the advisor's
// reply. Only Command is load-bearing (it becomes the gate); Description is data.
type proposal struct {
	Description string
	Command     string
}

// proposeCriteria asks the advisor for acceptance criteria as command lines and
// parses them. The contract with the advisor is intentionally tiny and robust: one
// criterion per line as "description :: command", with a bare line treated as a
// command-only criterion. We parse leniently so a slightly-off reply still yields
// usable proposals rather than failing the whole derivation.
func proposeCriteria(ctx context.Context, adv *advisor.Advisor, goal string) ([]proposal, error) {
	q := "Propose concrete, machine-checkable acceptance criteria for this goal. " +
		"Output ONE criterion per line as `description :: command`, where command is a " +
		"shell command that EXITS 0 exactly when the criterion is met (it may be RED now). " +
		"Use only commands runnable in a minimal sandbox. No prose, no numbering, no fences."
	reply, err := adv.Consult(ctx, summarize.ContextSummary{Goal: goal}, q)
	if err != nil {
		return nil, err
	}
	return parseProposals(reply), nil
}

// parseProposals extracts criteria from the advisor's free-text reply, one per
// non-empty line. It strips common list markers and code fences so a slightly
// formatted reply still parses. A line with "::" splits into description/command;
// a bare line is a command-only criterion.
func parseProposals(reply string) []proposal {
	var out []proposal
	for _, raw := range strings.Split(reply, "\n") {
		line := strings.TrimSpace(raw)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		line = strings.Trim(line, "`")
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		if i := strings.Index(line, "::"); i >= 0 {
			desc := strings.TrimSpace(line[:i])
			cmd := strings.TrimSpace(line[i+2:])
			if cmd != "" {
				out = append(out, proposal{Description: desc, Command: cmd})
			}
			continue
		}
		out = append(out, proposal{Command: line})
	}
	return out
}

// dryRunnable reports whether the sandbox could actually run cmd. A runnable
// command (the box returned a Result without a transport error) is keepable
// REGARDLESS of its exit code: a currently-red acceptance command is exactly what
// we want to keep. Only a transport-level failure (the box could not execute at
// all) drops the command. A 127 "command not found" surfaces as exit 127 with no
// transport error in most shells; to keep the bar honest we additionally drop a
// command whose combined output names it as not found, so a typo'd tool never
// becomes a permanently-unsatisfiable gate.
func dryRunnable(ctx context.Context, box sandbox.Sandbox, cmd string) bool {
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		return false
	}
	if res.ExitCode == 127 {
		return false // conventional shell "command not found" exit code
	}
	// A non-zero exit that the shell explains as a missing command is unrunnable,
	// not merely red. We check the shell's own diagnostic ("command not found") only
	// on a non-zero exit, so a passing command whose OUTPUT happens to contain that
	// phrase (e.g. a test asserting an error message) is never dropped.
	if res.ExitCode != 0 {
		combined := strings.ToLower(res.Stdout + " " + res.Stderr)
		if strings.Contains(combined, "command not found") {
			return false
		}
	}
	return true
}

// normalizeCmd collapses whitespace so two textually-equivalent commands dedupe to
// one criterion (add-only refinement must be idempotent across iterations).
func normalizeCmd(cmd string) string {
	return strings.Join(strings.Fields(cmd), " ")
}
