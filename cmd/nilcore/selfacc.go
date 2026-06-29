package main

// selfacc.go is the OPERATOR-CONTROLLED WIRING LAYER for self-authored acceptance
// verifiers (internal/verify/selfacc) — the piece that package deliberately does NOT
// contain so that an agent-proposed verifier can never make itself binding. It does
// two things:
//
//   - registerSelfAcceptance — the run-time wiring: when (and only when) the operator
//     has BOTH opted in (NILCORE_SELFACC) AND pointed NILCORE_SELFACC_FILE at a file
//     they authored/reviewed, it admits each candidate (the I4 meta-check) and
//     registers the admissible ones into the run's evverify.Registry, so a claim that
//     names one resolves through a real, SANDBOXED, fail-closed check. The two explicit
//     operator actions ARE the human gate: the agent may PROPOSE candidates, but only
//     the operator placing them in the approved file makes one binding.
//
//   - selfaccMain (`nilcore selfacc`) — the operator's review surface: `propose` turns
//     a goal (+ optional plan) into the contract-first criteria the agent would assert,
//     and `check` runs the meta-check over an approved file so the operator sees exactly
//     which candidates are admissible (and why a rejected one is not) BEFORE trusting it.
//
// Invariants this layer preserves (it adds NO permissive path of its own):
//   - I2: a self-acceptance check can only ADD a criterion that must ALSO pass; it
//     resolves a claim to Pass only on an affirmative sandboxed exit 0, else
//     Unverifiable. It never turns the build verifier's verdict green and runs only
//     AFTER it (any red claim still reddens the whole verdict).
//   - I3: candidates are inert data; no secret is read here or handed to a check beyond
//     what the sandbox already exposes.
//   - I4: every admitted candidate runs ONLY as a sandbox command (selfacc.Admit
//     rejects any host/in-process marker); there is no host-side fallback.
//   - I7: every field of a candidate (id, command, rationale) is treated as data —
//     validated structurally, never interpreted as an instruction.
//
// DEFAULT-OFF: with NILCORE_SELFACC unset (or no file), registerSelfAcceptance is a
// no-op and the verify path is byte-identical. FAIL-CLOSED: a malformed/unreadable
// approved file is an ERROR that reddens the evidence verdict (via the caller), never a
// silent fall-through to the generic-only registry.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/planner"
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

// registerSelfAcceptance is the run-time wiring entry. It is a no-op unless the operator
// has opted in (selfAcceptanceEnv) AND named an approved file (selfAcceptanceFileEnv).
// It admits each candidate and registers the admissible ones into reg, returning the
// bound verifier ids. A NON-admissible candidate is SKIPPED (with a stderr note) — never
// registered — so an untrusted candidate can never bind. A malformed/unreadable file is
// an error (the caller reddens the verdict), never a silent skip.
func registerSelfAcceptance(reg *evverify.Registry) ([]string, error) {
	if strings.TrimSpace(os.Getenv(selfAcceptanceEnv)) == "" {
		return nil, nil // default-off
	}
	cands, err := loadApprovedCandidates(os.Getenv(selfAcceptanceFileEnv))
	if err != nil {
		return nil, err
	}
	var registered []string
	for _, c := range cands {
		if aerr := selfacc.Admit(c); aerr != nil {
			// Fail-closed: a rejected candidate stays untrusted and simply never binds.
			fmt.Fprintf(os.Stderr, "nilcore selfacc: skipping un-admissible candidate %q: %v\n", c.VerifierID, aerr)
			continue
		}
		id, rerr := selfacc.Register(reg, c)
		if rerr != nil {
			// Defense in depth (Register re-admits): treat as a hard error so a
			// supposedly-admitted candidate that fails to bind never silently vanishes.
			return nil, fmt.Errorf("registering self-acceptance verifier %q: %w", c.VerifierID, rerr)
		}
		registered = append(registered, id)
	}
	return registered, nil
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
	fmt.Println("\nThese are INERT proposals. To make any one binding, author a sandbox-command")
	fmt.Println("candidate for it, review it with `nilcore selfacc check`, place it in your")
	fmt.Printf("approved file, and run with %s=1 %s=<file>.\n", selfAcceptanceEnv, selfAcceptanceFileEnv)
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
	fmt.Println("\nAll candidates admissible. With NILCORE_SELFACC=1 and this file named by")
	fmt.Println("NILCORE_SELFACC_FILE, each binds a sandboxed, fail-closed check at verify time.")
}
