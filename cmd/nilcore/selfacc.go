package main

// selfacc.go is the CLOSED-LOOP self-acceptance wiring (internal/verify/selfacc) — the
// piece the leaf deliberately does NOT contain so that an agent-proposed verifier can
// never make itself binding. The agent raises its OWN bar at run time: once the
// project's verifier (the floor — I2) is green, the agent's own acceptance checks must
// ALSO pass before the run is judged done.
//
// The loop (selfAcceptHook → runSelfAcceptance, the orchestrator's SelfAcceptFunc):
//
//  1. PROPOSE+AUTHOR: a single bounded model call authors up to N sandbox-command
//     acceptance checks for THIS goal (authorSelfAcceptCandidates). The operator may
//     ALSO supply pre-approved checks via NILCORE_SELFACC_FILE.
//  2. ADMIT: every candidate passes the I4 meta-check (selfacc.Admit) — sandbox command
//     only; an un-admissible one is skipped, never bound.
//  3. GATE: each AGENT-authored check is approved like any boundary action via a typed
//     policy.GateAction{Type: BindSelfAuthored} — an attended operator approves, a
//     headless run denies, and graduated auto-approval (graapprove) admits only a
//     self-check class that has EARNED trust. Operator-file checks are pre-approved (the
//     file IS the operator's approval) and skip the gate.
//  4. BIND+RUN: each approved+admitted check runs INSIDE the worktree box (selfacc.
//     CheckFunc → box.Exec); exit 0 = the criterion holds, anything else = not held.
//  5. FOLD: the run is judged done only if EVERY bound check passed. A bound check that
//     fails reddens the verdict (it can only ADD to the bar — it never greens a red
//     floor; the orchestrator consults it only when the floor is already green).
//  6. AUDIT + EARN TRUST: every step emits an append-only event (I5); each gated check's
//     pass/fail emits a boundary_outcome so a proven self-check class can amortize its
//     review and later auto-approve within the envelope.
//
// `nilcore selfacc` (selfaccMain) is the read-only operator review surface: `propose`
// shows the contract-first criteria for a goal+plan; `check` meta-checks a candidate
// file before you trust it.
//
// Invariants: I2 (floor verifier still governs; self-acceptance only ADDS), I3
// (candidates inert; no secret read/handed beyond the sandbox), I4 (every check runs
// only as a sandbox command — no host fallback), I7 (the goal + every authored field is
// data, validated structurally, never an instruction). DEFAULT-OFF: NILCORE_SELFACC
// unset ⇒ selfAcceptHook returns nil ⇒ the orchestrator is byte-identical.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"nilcore/internal/agent"
	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/planner"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify/selfacc"
)

// selfAcceptanceEnv gates the whole layer. Unset ⇒ off (no registration, no behavior
// change). It is deliberately a SECOND opt-in beyond merely placing a file, so the
// capability cannot be turned on by a stray file alone.
const selfAcceptanceEnv = "NILCORE_SELFACC"

// selfAcceptanceFileEnv names the operator-authored approved-candidates file. Its
// contents are the operator's explicit approval of which self-authored verifiers may
// bind. An absolute, operator-controlled path keeps it OUT of the disposable worktree
// (so a run can never write its own approval).
const selfAcceptanceFileEnv = "NILCORE_SELFACC_FILE"

// approvedCandidate is the on-disk shape of one operator-approved self-acceptance
// verifier. It maps to selfacc.Candidate with operator-friendly snake_case keys.
type approvedCandidate struct {
	VerifierID string `json:"verifier_id"`
	Command    string `json:"command"`
	Rationale  string `json:"rationale,omitempty"`
}

// approvedFile is the top-level shape of the approved-candidates file: a named list so
// the format can grow (e.g. a version field) without breaking a bare-array reader.
type approvedFile struct {
	Candidates []approvedCandidate `json:"candidates"`
}

func (a approvedCandidate) toCandidate() selfacc.Candidate {
	return selfacc.Candidate{VerifierID: a.VerifierID, Command: a.Command, Rationale: a.Rationale}
}

// loadApprovedCandidates reads + parses the operator-approved candidates file. A read
// or parse error is returned (fail-closed); an empty/whitespace path returns nil with
// no error (nothing approved).
func loadApprovedCandidates(path string) ([]selfacc.Candidate, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading approved file %q: %w", path, err)
	}
	var f approvedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing approved file %q: %w", path, err)
	}
	out := make([]selfacc.Candidate, 0, len(f.Candidates))
	for _, c := range f.Candidates {
		out = append(out, c.toCandidate())
	}
	return out, nil
}

// selfAcceptMaxEnv overrides the cap on how many checks the model may author per run
// (so an attended operator is never asked to approve an unbounded list). Default below.
const selfAcceptMaxEnv = "NILCORE_SELFACC_MAX"

// defaultSelfAcceptMax bounds authored checks per run.
const defaultSelfAcceptMax = 5

// selfAcceptEnabled reports the opt-in. Unset ⇒ off (selfAcceptHook returns nil).
func selfAcceptEnabled() bool { return strings.TrimSpace(os.Getenv(selfAcceptanceEnv)) != "" }

// selfAcceptMax returns the per-run authored-check cap (NILCORE_SELFACC_MAX, else 5).
func selfAcceptMax() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(selfAcceptMaxEnv))); err == nil && v > 0 {
		return v
	}
	return defaultSelfAcceptMax
}

// selfAcceptHook builds the orchestrator's closed-loop self-acceptance hook, capturing
// the model provider for authoring. It returns nil when self-acceptance is off, so the
// orchestrator is byte-identical (the hook is never installed).
func selfAcceptHook(prov model.Provider) agent.SelfAcceptFunc {
	if !selfAcceptEnabled() {
		return nil
	}
	maxN := selfAcceptMax()
	return func(ctx context.Context, goal string, box sandbox.Sandbox, gate func(policy.GateAction) bool, log *eventlog.Log) (bool, string) {
		return runSelfAcceptance(ctx, prov, maxN, goal, box, gate, log)
	}
}

// runSelfAcceptance gathers the run's self-acceptance checks — operator pre-approved
// (NILCORE_SELFACC_FILE) plus up to maxN agent-authored (one model call) — and runs
// them through runCandidates. A model/parse error in authoring is non-fatal (no
// proposals); a malformed operator file is non-fatal here (skipped with a note) — the
// floor verifier already governs, and self-acceptance only ever ADDS to the bar.
func runSelfAcceptance(ctx context.Context, prov model.Provider, maxN int, goal string, box sandbox.Sandbox, gate func(policy.GateAction) bool, log *eventlog.Log) (bool, string) {
	preApproved, ferr := loadApprovedCandidates(os.Getenv(selfAcceptanceFileEnv))
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "nilcore selfacc: ignoring approved file: %v\n", ferr)
		preApproved = nil
	}
	var proposed []selfacc.Candidate
	if prov != nil {
		var aerr error
		if proposed, aerr = authorSelfAcceptCandidates(ctx, prov, goal, maxN); aerr != nil {
			fmt.Fprintf(os.Stderr, "nilcore selfacc: authoring skipped: %v\n", aerr)
			proposed = nil
		}
	}
	appendSelfaccEvent(log, "selfacc_propose", map[string]any{
		"pre_approved": len(preApproved), "proposed": len(proposed),
	})
	return runCandidates(ctx, box, gate, log, preApproved, proposed)
}

// runCandidates is the testable core: admit → (gate the agent-proposed) → bind into a
// fresh per-run registry → resolve (run) each against the box → fold. It binds through
// the leaf's designed seam — selfacc.Register (the ONLY path a self-authored verifier
// becomes runnable) + selfacc.Resolve (fail-closed: an unbound/un-admitted id is
// Unverifiable, never Pass). preApproved candidates skip the gate (the operator file IS
// their approval); proposed candidates are each gated via a typed BindSelfAuthored
// action. Only a BOUND check that RUNS and does not pass reddens the result — an
// un-admissible or gate-denied candidate simply does not participate (denial ≠ failure).
// With no bound checks the result is green (byte-identical to no self-acceptance).
func runCandidates(ctx context.Context, box sandbox.Sandbox, gate func(policy.GateAction) bool, log *eventlog.Log, preApproved, proposed []selfacc.Candidate) (bool, string) {
	reg := evverify.New() // fresh, per-run: holds ONLY this run's bound self-checks
	var failures []string
	run := func(c selfacc.Candidate, needGate bool) {
		if err := selfacc.Admit(c); err != nil {
			appendSelfaccEvent(log, "selfacc_skip", map[string]any{"id": c.VerifierID, "reason": err.Error()})
			fmt.Fprintf(os.Stderr, "nilcore selfacc: skipping un-admissible candidate %q: %v\n", c.VerifierID, err)
			return
		}
		if needGate {
			action := policy.GateAction{Type: policy.BindSelfAuthored, Branch: c.VerifierID, Detail: clipSelfacc(c.Command, 120)}
			approved := gate(action)
			appendSelfaccEvent(log, "selfacc_gate", map[string]any{"id": c.VerifierID, "approved": approved})
			if !approved {
				return // not trusted ⇒ not bound; denial is not a failure
			}
		}
		if _, err := selfacc.Register(reg, c); err != nil { // re-admits (defense in depth)
			appendSelfaccEvent(log, "selfacc_skip", map[string]any{"id": c.VerifierID, "reason": err.Error()})
			return
		}
		claim := artifact.Claim{Evidence: artifact.Evidence{Verifier: c.VerifierID}}
		status, detail := selfacc.Resolve(ctx, reg, box, claim)
		passed := status == artifact.StatusPass
		appendSelfaccEvent(log, "selfacc_bind", map[string]any{
			"id": c.VerifierID, "status": string(status), "passed": passed,
		})
		if needGate {
			// Earn (or lose) trust for this self-check class so a proven one can later
			// auto-approve within the operator envelope (amortized review). Only gated
			// (agent-authored) checks feed trust; pre-approved operator checks do not.
			emitBoundaryOutcome(log, policy.BindSelfAuthored.String(), c.VerifierID, passed)
		}
		if !passed {
			d := strings.TrimSpace(detail)
			if d == "" {
				d = string(status)
			}
			failures = append(failures, fmt.Sprintf("%s (%s)", c.VerifierID, clipSelfacc(d, 160)))
		}
	}
	for _, c := range preApproved {
		run(c, false)
	}
	for _, c := range proposed {
		run(c, true)
	}
	if len(failures) > 0 {
		return false, "failed self-authored check(s): " + strings.Join(failures, "; ")
	}
	return true, ""
}

// authorSelfAcceptCandidates makes ONE bounded model call asking the agent to author up
// to maxN sandbox-command acceptance checks for the goal, and parses the JSON reply as
// UNTRUSTED data (I7) — each becomes a selfacc.Candidate, validated later by Admit. A
// model error or unparseable reply returns an error (the caller treats it as "no
// proposals", never a silent pass).
func authorSelfAcceptCandidates(ctx context.Context, prov model.Provider, goal string, maxN int) ([]selfacc.Candidate, error) {
	sys := fmt.Sprintf(`You author ACCEPTANCE CHECKS for a coding task. For the given goal, propose up to %d checks that would prove the work is actually done. Each check is a SINGLE shell command run INSIDE a sandbox at the repository root; exit code 0 means the criterion holds, non-zero means it does not. Commands must be deterministic, non-interactive, self-contained, and avoid the network. Do NOT propose host/in-process execution. Return ONLY a JSON object, no prose:
{"candidates":[{"verifier_id":"candidate.<short_snake_case>","command":"<one shell line>","rationale":"<why this proves the criterion>"}]}`, maxN)
	msgs := []model.Message{{Role: "user", Content: []model.Block{{Type: "text", Text: "Goal:\n" + goal}}}}
	resp, err := prov.Complete(ctx, sys, msgs, nil, 1024)
	if err != nil {
		return nil, fmt.Errorf("authoring model call: %w", err)
	}
	cands, err := parseSelfaccCandidates(firstSelfaccText(resp.Content))
	if err != nil {
		return nil, err
	}
	if len(cands) > maxN {
		cands = cands[:maxN]
	}
	return cands, nil
}

// parseSelfaccCandidates extracts the JSON object from the model reply and maps it to
// candidates. UNTRUSTED data: no field is interpreted, only carried for Admit.
func parseSelfaccCandidates(s string) ([]selfacc.Candidate, error) {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in authoring reply")
	}
	var f approvedFile
	if err := json.Unmarshal([]byte(s[start:end+1]), &f); err != nil {
		return nil, fmt.Errorf("parsing authored candidates: %w", err)
	}
	out := make([]selfacc.Candidate, 0, len(f.Candidates))
	for _, c := range f.Candidates {
		out = append(out, c.toCandidate())
	}
	return out, nil
}

// firstSelfaccText returns the first text block of a model response.
func firstSelfaccText(blocks []model.Block) string {
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			return b.Text
		}
	}
	return ""
}

// appendSelfaccEvent appends a metadata-only self-acceptance audit event (I5). nil-safe.
func appendSelfaccEvent(log *eventlog.Log, kind string, detail map[string]any) {
	if log == nil {
		return
	}
	log.Append(eventlog.Event{Kind: kind, Detail: detail})
}

// clipSelfacc bounds a string for a prompt/detail field so an untrusted command/output
// can never flood a gate prompt or an event Detail.
func clipSelfacc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// selfaccMain implements `nilcore selfacc` — the operator review surface. Subcommands:
//
//	propose -goal "..." [-plan tree.json]   print the contract-first criteria the agent
//	                                        would assert for an under-specified goal
//	check   -file approved.json             admit every candidate in the file and report
//	                                        which are admissible (and why a rejected one
//	                                        is not), WITHOUT registering anything
//
// Neither subcommand runs a candidate or changes any verdict — they are read-only
// operator aids for reviewing a self-acceptance set before trusting it.
func selfaccMain(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, selfaccUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "propose":
		selfaccPropose(args[1:])
	case "check":
		selfaccCheck(args[1:])
	default:
		fmt.Fprintln(os.Stderr, selfaccUsage)
		os.Exit(2)
	}
}

const selfaccUsage = `usage:
  nilcore selfacc propose -goal "<goal>" [-plan <tree.json>]   show proposed acceptance criteria
  nilcore selfacc check   -file <approved.json>                meta-check an approved-candidates file`

// selfaccPropose runs selfacc.Propose over a goal and an optional plan tree, printing
// the proposed criteria. With no plan it honestly prints "nothing proposed yet" rather
// than fabricating criteria.
func selfaccPropose(args []string) {
	fs := flag.NewFlagSet("selfacc propose", flag.ExitOnError)
	goal := fs.String("goal", "", "the (under-specified) goal to propose acceptance criteria for")
	plan := fs.String("plan", "", "optional path to a plan tree JSON whose tasks' acceptance fields seed the criteria")
	_ = fs.Parse(args)
	if strings.TrimSpace(*goal) == "" {
		fmt.Fprintln(os.Stderr, "nilcore selfacc propose: -goal is required")
		os.Exit(2)
	}

	var tree *planner.Tree
	if strings.TrimSpace(*plan) != "" {
		data, err := os.ReadFile(*plan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nilcore selfacc propose: reading plan: %v\n", err)
			os.Exit(1)
		}
		var t planner.Tree
		if err := json.Unmarshal(data, &t); err != nil {
			fmt.Fprintf(os.Stderr, "nilcore selfacc propose: parsing plan: %v\n", err)
			os.Exit(1)
		}
		tree = &t
	}

	p := selfacc.Propose(*goal, tree)
	if len(p.Criteria) == 0 {
		fmt.Printf("goal: %s\n(no criteria proposed yet — supply a -plan whose tasks state acceptance criteria)\n", p.Goal)
		return
	}
	fmt.Printf("goal: %s\nproposed acceptance criteria (%d):\n", p.Goal, len(p.Criteria))
	for _, c := range p.Criteria {
		fmt.Printf("  - [%s] %s\n", c.Field, c.Statement)
	}
	fmt.Println("\nThese are INERT proposals. With NILCORE_SELFACC=1 the agent authors a")
	fmt.Println("sandbox-command check per criterion at run time and you approve each at the")
	fmt.Printf("gate; or pre-approve a reviewed set via %s=<file>. See docs/SELFACC.md.\n", selfAcceptanceFileEnv)
}

// selfaccCheck admits every candidate in an approved file and reports the verdict per
// candidate, WITHOUT registering or running anything. It is the operator's pre-trust
// review: it surfaces exactly which candidates would bind and why a rejected one would
// not. Exit code is non-zero if ANY candidate is un-admissible, so it composes into a
// pre-flight check.
func selfaccCheck(args []string) {
	fs := flag.NewFlagSet("selfacc check", flag.ExitOnError)
	file := fs.String("file", "", "path to the approved-candidates JSON file to meta-check")
	_ = fs.Parse(args)
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(os.Stderr, "nilcore selfacc check: -file is required")
		os.Exit(2)
	}
	cands, err := loadApprovedCandidates(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nilcore selfacc check: %v\n", err)
		os.Exit(1)
	}
	if len(cands) == 0 {
		fmt.Printf("%s: no candidates\n", *file)
		return
	}
	rejected := 0
	fmt.Printf("%s: %d candidate(s)\n", *file, len(cands))
	for _, c := range cands {
		if aerr := selfacc.Admit(c); aerr != nil {
			rejected++
			fmt.Printf("  ✗ %s — REJECTED: %v\n", c.VerifierID, aerr)
			continue
		}
		fmt.Printf("  ✓ %s — admissible (sandbox command)\n", c.VerifierID)
	}
	if rejected > 0 {
		fmt.Printf("\n%d candidate(s) un-admissible — they would be SKIPPED (never bound).\n", rejected)
		os.Exit(1)
	}
	fmt.Println("\nAll candidates admissible. Named by NILCORE_SELFACC_FILE (with NILCORE_SELFACC=1)")
	fmt.Println("each runs as a PRE-APPROVED, sandboxed, fail-closed check during the run (it skips")
	fmt.Println("the gate — the file is your approval). Agent-authored checks are gated per-run instead.")
}
