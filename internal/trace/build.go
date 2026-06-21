package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"nilcore/internal/eventlog"
)

// build.go replays the JSONL event log and assembles the causal tree. It mirrors
// the inspect package's replay-then-verify discipline: scan every line into a
// neutral structure, then ask eventlog.Verify (the one authority on chain
// integrity) for the trustworthiness verdict. The crucial difference from a
// readiness probe is the fail-closed behaviour: a broken chain does NOT abort
// the build — we still hand the operator the structure to debug — but it marks
// every node untrusted and stamps the loud CHAIN BROKEN verdict (I5).

// rawEvent mirrors the on-disk JSONL shape written by eventlog. We read only the
// fields the trace projects; eventlog owns the hash/prev chain, and Verify
// re-derives those independently from the same file, so the builder never needs
// to touch them.
type rawEvent struct {
	Time    time.Time      `json:"time"`
	Seq     uint64         `json:"seq"`
	Task    string         `json:"task"`
	Kind    string         `json:"kind"`
	Backend string         `json:"backend"`
	Detail  map[string]any `json:"detail"`
}

// Build reconstructs the causal trace for one task from the log at logPath. If
// taskID is "" or "*" it builds a single trace over ALL tasks merged in Seq
// order (use BuildAll to split per task instead). It always runs
// eventlog.Verify and records the verdict in ChainVerified; on a broken chain it
// still returns a structural trace, every node marked untrusted, with the
// CHAIN BROKEN verdict (I5 — fail closed on trustworthiness, never on
// visibility).
func Build(logPath, taskID string) (*Trace, error) {
	events, err := scan(logPath)
	if err != nil {
		return nil, err
	}

	all := taskID == "" || taskID == "*"
	filtered := events
	if !all {
		filtered = filtered[:0]
		for _, e := range events {
			if e.Task == taskID {
				filtered = append(filtered, e)
			}
		}
	}

	chainOK := eventlog.Verify(logPath) == nil
	tr := assemble(filtered, taskID, chainOK)
	return tr, nil
}

// BuildAll splits the log into one Trace per distinct task, each ordered by Seq,
// returned in first-seen order so the output is stable. The chain is verified
// once over the whole file (it is a single chain), so every returned trace
// shares the same ChainVerified verdict — a break anywhere taints the lot,
// which is the correct, conservative reading of a hash chain.
func BuildAll(logPath string) ([]*Trace, error) {
	events, err := scan(logPath)
	if err != nil {
		return nil, err
	}
	chainOK := eventlog.Verify(logPath) == nil

	order := []string{}
	byTask := map[string][]rawEvent{}
	for _, e := range events {
		if _, seen := byTask[e.Task]; !seen {
			order = append(order, e.Task)
		}
		byTask[e.Task] = append(byTask[e.Task], e)
	}

	traces := make([]*Trace, 0, len(order))
	for _, task := range order {
		traces = append(traces, assemble(byTask[task], task, chainOK))
	}
	return traces, nil
}

// scan parses every non-empty line of the log into rawEvents, ordered by Seq.
// A parse error is returned (a log we cannot read is not a log we can explain);
// chain integrity is a separate, later question answered by eventlog.Verify.
func scan(logPath string) ([]rawEvent, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("opening event log: %w", err)
	}
	defer f.Close()

	var events []rawEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e rawEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("event %d: parsing line: %w", n, err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading event log: %w", err)
	}

	// Seq is the authoritative order (the chain anchors against reordering); sort
	// by it so a log read out of order still tells the story in the right order.
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
	return events, nil
}

// assemble turns an ordered event slice (already filtered to the relevant
// task(s)) into a Trace: it derives each node's Title/Why via the why.go table,
// groups consecutive model_call -> tool_exec(s) into step nodes, clusters race
// outcomes, tallies counts, and computes the verdict. chainOK gates every
// trustworthiness signal.
func assemble(events []rawEvent, taskID string, chainOK bool) *Trace {
	tr := &Trace{
		Task:          taskID,
		ChainVerified: chainOK,
		Counts:        map[string]int{},
	}
	if taskID == "" || taskID == "*" {
		tr.Task = "(all tasks)"
	}

	c := &ctx{}
	var steps []Step

	for i := 0; i < len(events); i++ {
		e := events[i]
		tr.Counts[e.Kind]++

		// Capture the goal from the first task_start we see.
		if e.Kind == "task_start" && tr.Goal == "" {
			tr.Goal = detailStr(e.Detail, "goal")
		}

		switch e.Kind {
		// A model turn opens a step: it and the tool execs that immediately
		// follow it collapse into one node, so the tree reads as "thought, then
		// did N things" rather than a flat wall of events.
		case "model_call":
			step := node(e, c)
			j := i + 1
			for j < len(events) && events[j].Kind == "tool_exec" {
				tr.Counts[events[j].Kind]++
				step.Children = append(step.Children, node(events[j], c))
				j++
			}
			i = j - 1
			steps = append(steps, step)

		// A run of race_outcome events is one decision: "raced N backends, the
		// verifier picked the winner". Cluster them under a synthetic node.
		case "race_outcome":
			j := i
			var kids []Step
			passedCount := 0
			for j < len(events) && events[j].Kind == "race_outcome" {
				if j != i {
					tr.Counts[events[j].Kind]++
				}
				if p, _ := detailBool(events[j].Detail, "passed"); p {
					passedCount++
				}
				kids = append(kids, node(events[j], c))
				j++
			}
			n := j - i
			i = j - 1
			cluster := Step{
				Kind:     "race_cluster",
				Time:     e.Time,
				Seq:      e.Seq,
				Title:    fmt.Sprintf("raced %s", plural(n, "backend")),
				Children: kids,
			}
			if passedCount > 0 {
				cluster.Why = "ran the candidates in parallel; the verifier picked a backend whose result passed the checks"
			} else {
				cluster.Why = "ran the candidates in parallel; none passed the verifier — the run escalates"
			}
			steps = append(steps, cluster)

		// Everything else is its own node (verify, gate, advisor, integrate, …).
		default:
			steps = append(steps, node(e, c))
		}
	}

	// Finalize: when the chain did not verify, stamp EVERY node (and child)
	// untrusted in one place, so node() can stay pure of chain state and there is
	// a single, auditable point where the "do not trust this" mark is applied.
	if !chainOK {
		markUntrusted(steps)
	}

	tr.Steps = steps
	tr.Verdict = verdict(events, chainOK)
	return tr
}

// markUntrusted stamps the untrusted flag through a step tree in place. Called
// only when the hash chain failed to verify, so the renderer can flag the whole
// trace as not-to-be-trusted (I5).
func markUntrusted(steps []Step) {
	for i := range steps {
		steps[i].Untrusted = true
		markUntrusted(steps[i].Children)
	}
}

// node projects one raw event into a Step, deriving its Title/Why from the
// why.go table and copying only allowlisted metadata into Detail. It leaves
// Untrusted false; the chain decision is applied in one place (markUntrusted),
// so node stays pure of chain state.
func node(e rawEvent, c *ctx) Step {
	title, why := annotate(e.Kind, e.Detail, c)
	return Step{
		Seq:     e.Seq,
		Time:    e.Time,
		Kind:    e.Kind,
		Backend: e.Backend,
		Title:   title,
		Why:     why,
		Detail:  safeDetail(e.Detail),
	}
}

// verdict computes the run's headline. It is CLEAN only when the chain verified
// (I5): on a broken chain it is always the loud CHAIN BROKEN string regardless
// of how the run appears to have ended, because a tampered log's apparent ending
// cannot be trusted. On a clean chain it summarizes the final verify/gate/
// integrate outcome.
func verdict(events []rawEvent, chainOK bool) string {
	if !chainOK {
		return brokenChainVerdict
	}
	if len(events) == 0 {
		return "no events for this task"
	}

	// Walk backwards for the most decisive terminal signal.
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		switch e.Kind {
		case "integration_merge", "integrate":
			return "integrated — the verified branch was merged"
		case "final_verify", "verify":
			if p, ok := detailBool(e.Detail, "passed"); ok {
				if p {
					return "verified — the project's checks passed"
				}
				return "NOT verified — the project's checks did not pass"
			}
		case "project_done":
			return "project complete"
		case "integration_rollback":
			return "rolled back — the merge failed re-verification"
		}
	}
	return "ended without a terminal verify/gate/integrate signal"
}
