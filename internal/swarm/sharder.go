package swarm

// sharder.go — goal → []Shard (SW-T11).
//
// A Sharder turns a single operator goal into the concrete set of swarm units. Three
// strategies cover the product's three entry shapes, and each is a SEPARATE, hermetic
// transform so the cmd wiring can pick one without the others' dependencies:
//
//   - ListSharder: the operator already knows the units (a `--shard-file` or an
//     inline list). It is a pure, deterministic, NO-MODEL fan-out — N lines become N
//     namespaced shards. The Kind/Pack/Role routing is supplied by the CALLER as plain
//     fields (never imported from internal/swarm/preset — that would cycle, since
//     preset imports swarm), so this file stays a leaf.
//
//   - PlanSharder: a goal that needs decomposition. It mirrors planner.Plan EXACTLY —
//     JSON-only parse, then planner.Validate — and is fail-closed: an unparseable or
//     invalid plan is an ERROR, never a silent empty set (an empty set would look like
//     "nothing to do" and exit green over an unplanned goal). The plan's DependsOn
//     edges become Shard.Deps so the runner's DAG honors the planned ordering.
//
//   - FailureSharder: the "fix the red tests" run (the `fix` preset). It runs
//     verify.Detect once, executes the detected command in the sandbox once, parses the
//     failing test names from the output, and emits ONE shard per failure — the "one test
//     failure" granularity the swarm requeues against. When parsing yields nothing
//     actionable it falls back to a single whole-suite shard (documented below) rather
//     than a misleading empty set.
//
// Determinism. Every sharder emits shards in a stable order (list order, plan order,
// detected-failure order) so a re-run plans the same set — meaningful for golden tests
// and for a resume that must line up with the persisted rows.

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/model"
	"nilcore/internal/planner"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// Sharder is the goal→shards seam. runID namespaces the produced shard IDs
// ("swarm-<runID>-<n>", the dash-delimited form shardID emits) so two runs over one
// store never collide. An implementation
// that needs a model or a sandbox holds it as a field (PlanSharder/FailureSharder);
// ListSharder needs neither.
type Sharder interface {
	Shards(ctx context.Context, goal, runID string) ([]Shard, error)
}

// shardID builds the canonical namespaced shard identity "swarm-<runID>-<n>" — the
// one place the convention lives, shared by every sharder so the prefix the Queue
// filters on is produced identically everywhere. The separator is '-' (not '/') ON
// PURPOSE: a shard owns ONE artifact written at .nilcore/artifacts/<shard id>.json and
// the convergence model keys artifact id == shard id, but artifact.validID rejects any
// id containing a path separator. A '/'-delimited id would make the per-shard
// artifact read/write/verify silently fail (the artifact would never land in the
// collate root requeue.Scan reads), so the id must itself be a valid single-component
// artifact id. runID is a fixed-length slug, so no run's prefix is a prefix of another.
func shardID(runID string, n int) string {
	return fmt.Sprintf("swarm-%s-%d", runID, n)
}

// ListSharder fans an operator-supplied list into shards with NO model call. Lines is
// the raw list (one unit per line; blank lines and lines beginning with '#' are
// dropped so a commented `--shard-file` is usable). Kind/Pack/Role/Tier are the
// routing fields the CALLER supplies as plain strings — this leaf never imports the
// preset package that would otherwise resolve them (preset → swarm is the only
// allowed direction). Every produced shard carries the same routing (the list is one
// homogeneous batch); per-line routing is a future extension, intentionally not built
// here.
type ListSharder struct {
	Lines []string
	Kind  artifact.Kind
	Pack  string
	Role  string
	Tier  string
}

// Shards turns each non-blank, non-comment line into one queued shard in list order.
// The goal is carried verbatim as each shard's Goal when a line is empty of its own
// instruction — but for a list, the LINE is the per-shard goal and Input, and the
// run-level goal is contextual only. A list with no usable lines yields zero shards
// (the controller surfaces checked=0 ⇒ exit 1); it is not an error here, because an
// empty list is a legitimate (if useless) operator input, distinct from a PARSE
// failure which only the model-driven sharders can have.
func (s ListSharder) Shards(_ context.Context, goal, runID string) ([]Shard, error) {
	out := make([]Shard, 0, len(s.Lines))
	n := 0
	for _, raw := range s.Lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, Shard{
			ID:    shardID(runID, n),
			Input: line,
			Goal:  shardGoal(goal, line),
			Kind:  s.Kind,
			Pack:  s.Pack,
			Role:  s.Role,
			Tier:  s.Tier,
			State: ShardQueued,
		})
		n++
	}
	return out, nil
}

// shardGoal composes a per-shard goal from the run-level goal and the unit line. When
// a run-level goal is set it frames the unit ("<goal>: <line>") so the worker has the
// run's intent and the specific unit; with no run goal the line is the goal. This is
// harness-authored control text built from operator input — the line is DATA, never
// an instruction the harness itself acts on (I7 is enforced at the worker boundary,
// not here; this only shapes the prompt text the worker receives).
func shardGoal(goal, line string) string {
	if strings.TrimSpace(goal) == "" {
		return line
	}
	return goal + ": " + line
}

// PlanSharder decomposes a goal into shards via the strong advisor model, mirroring
// planner.Plan's discipline exactly: ONE model call, JSON-only parse, then Validate.
// Model is the provider the call runs against (the planner tier). The routing fields
// applied to every produced shard are the caller-supplied defaults (Kind/Pack/Role/
// Tier) — the plan decides the WORK and the EDGES; the operator/preset decides the
// verifier pack and provider tier, kept as plain fields so this leaf never imports
// preset.
type PlanSharder struct {
	Model model.Provider
	Kind  artifact.Kind
	Pack  string
	Role  string
	Tier  string
}

// Shards runs the planner once and maps the validated tree to shards, carrying each
// PlanTask's DependsOn onto Shard.Deps (re-namespaced to the run's shard IDs) so the
// runner's DAG honors the planned ordering. It is FAIL-CLOSED: planShards returns an
// error for an unparseable or invalid plan, and Shards propagates it — an empty shard
// set is never produced from a goal that failed to plan, because that would
// masquerade as "converged, nothing to do".
func (s PlanSharder) Shards(ctx context.Context, goal, runID string) ([]Shard, error) {
	tree, err := planShards(ctx, s.Model, goal)
	if err != nil {
		return nil, err
	}
	return treeToShards(tree, runID, s.Kind, s.Pack, s.Role, s.Tier), nil
}

// TreeSharder maps a PRE-BUILT planner.Tree to shards WITHOUT a model call. It is the
// seam for a caller that already knows the DAG — e.g. `nilcore flows run`, whose
// agentic-flows agent_task nodes ARE the plan (goals + produces→requires edges): the
// flow's structure must become real Shard.Deps so the runner honors it (a dependent
// coded on the integrated tip of its dependency), instead of being flattened into an
// unordered goal list. Routing fields (Kind/Pack/Role/Tier) are the caller/preset
// defaults, exactly like PlanSharder.
type TreeSharder struct {
	Tree planner.Tree
	Kind artifact.Kind
	Pack string
	Role string
	Tier string
}

// Shards maps the pre-built tree to shards, carrying each task's DependsOn onto
// Shard.Deps (re-namespaced to run shard ids) — the SAME mapping PlanSharder uses, minus
// the model call. goal is ignored (the tree already carries the per-task goals).
func (s TreeSharder) Shards(_ context.Context, _, runID string) ([]Shard, error) {
	return treeToShards(s.Tree, runID, s.Kind, s.Pack, s.Role, s.Tier), nil
}

// treeToShards maps a validated planner.Tree onto run-namespaced shards, rewriting each
// PlanTask's DependsOn (plan-task ids) onto the depended-on shards' ids so the runner's
// DAG honors the planned ordering. Shared by PlanSharder (model-planned) and TreeSharder
// (pre-built), so both produce identical shard shapes.
func treeToShards(tree planner.Tree, runID string, kind artifact.Kind, pack, role, tier string) []Shard {
	// Map plan-task ids to the run-namespaced shard ids in declaration order, so a
	// DependsOn naming a plan-task id can be rewritten to the depended-on shard's id.
	idByPlan := make(map[string]string, len(tree.Tasks))
	for i, t := range tree.Tasks {
		idByPlan[t.ID] = shardID(runID, i)
	}

	out := make([]Shard, 0, len(tree.Tasks))
	for i, t := range tree.Tasks {
		var deps []string
		for _, d := range t.DependsOn {
			if mapped, ok := idByPlan[d]; ok {
				deps = append(deps, mapped)
			}
			// An unknown dep id is dropped: planner.Validate rejects a model-planned tree
			// referencing an undefined task, and the flows adapter derives edges only
			// between real agent_task nodes — so a dangling dep never reaches here.
		}
		out = append(out, Shard{
			ID:    shardID(runID, i),
			Input: t.Goal,
			Goal:  t.Goal,
			Kind:  kind,
			Pack:  pack,
			Role:  role,
			Tier:  tier,
			Deps:  deps,
			State: ShardQueued,
		})
	}
	return out
}

// planShards is the internal planner mirror: it asks the model to decompose goal and
// returns the VALIDATED tree, with the SAME parse-then-Validate discipline as
// planner.Plan. It is duplicated rather than calling planner.Plan directly because the
// swarm needs the raw Tree (to map DependsOn onto shard ids), and planner.Plan returns
// the Tree but the swarm's failure messages should name the swarm context. A nil model
// is a setup error, never a silent empty plan.
func planShards(ctx context.Context, m model.Provider, goal string) (planner.Tree, error) {
	if m == nil {
		return planner.Tree{}, fmt.Errorf("swarm sharder: PlanSharder requires a model")
	}
	tree, err := planner.Plan(ctx, m, goal)
	if err != nil {
		// planner.Plan already wraps parse/validate failures; re-wrap with the swarm
		// context so an operator sees WHICH stage refused. The error is propagated, so
		// the controller never proceeds with an empty shard set on a failed plan.
		return planner.Tree{}, fmt.Errorf("swarm sharder: plan goal: %w", err)
	}
	return tree, nil
}

// FailureSharder turns the red tests of a repo into one shard per failure — the
// "fix the failing test" entry shape. Box is the sandbox the detected verify command
// runs in; its Workdir() is the repo verify.Detect inspects. Kind/Pack/Role/Tier are
// the caller-supplied routing applied to every produced fix shard.
type FailureSharder struct {
	Box  sandbox.Sandbox
	Kind artifact.Kind
	Pack string
	Role string
	Tier string
}

// Shards runs verify.Detect once over the box workdir, executes the detected command
// once in the box, parses the failing test names from the combined output, and emits
// one shard per failure in detected order. Each shard's goal names exactly one failing
// test so the worker fixes one cell, and its Input carries the run goal for context.
//
// Documented fallback. Parsing test-runner output is inherently best-effort across
// ecosystems; when no failing test name can be extracted (an unrecognized runner, or a
// build failure with no per-test lines), Shards falls back to a SINGLE whole-suite
// shard ("fix the failing verify command") rather than an empty set — an empty set
// would falsely read as "no failures". A green run (the command passed) legitimately
// yields zero shards: there is nothing to fix.
func (s FailureSharder) Shards(ctx context.Context, goal, runID string) ([]Shard, error) {
	if s.Box == nil {
		return nil, fmt.Errorf("swarm sharder: FailureSharder requires a sandbox")
	}
	cmd := verify.Detect(s.Box.Workdir())
	res, err := s.Box.Exec(ctx, cmd)
	if err != nil {
		// A sandbox execution error (not a test failure — that is a non-zero exit) is a
		// real fault: we could not even RUN the suite, so we cannot enumerate failures.
		return nil, fmt.Errorf("swarm sharder: run verify %q: %w", cmd, err)
	}
	if res.ExitCode == 0 {
		// The suite is green: nothing to fix. Zero shards is the correct, honest result
		// here (distinct from a parse failure on a red suite).
		return nil, nil
	}

	failures := parseFailures(res.Stdout + "\n" + res.Stderr)
	if len(failures) == 0 {
		// Red suite but no per-test names extracted: emit one whole-suite shard so the
		// failure is still addressable (the documented fallback), never a silent empty.
		return []Shard{{
			ID:    shardID(runID, 0),
			Input: goal,
			Goal:  shardGoal(goal, "fix the failing verify command: "+cmd),
			Kind:  s.Kind,
			Pack:  s.Pack,
			Role:  s.Role,
			Tier:  s.Tier,
			State: ShardQueued,
		}}, nil
	}

	out := make([]Shard, 0, len(failures))
	for i, name := range failures {
		out = append(out, Shard{
			ID:    shardID(runID, i),
			Input: name,
			Goal:  shardGoal(goal, "fix failing test "+name),
			Kind:  s.Kind,
			Pack:  s.Pack,
			Role:  s.Role,
			Tier:  s.Tier,
			State: ShardQueued,
		})
	}
	return out, nil
}

// failPatterns matches a failing-test line across the common runners verify.Detect can
// select. Each capture group 1 is the test identity:
//
//   - Go:     "--- FAIL: TestFoo (0.01s)"  → TestFoo
//   - pytest: "FAILED tests/test_x.py::test_foo"  → tests/test_x.py::test_foo
//   - generic "FAIL: <name>" / "not ok 1 - <name>" (TAP)  → <name>
//
// The patterns are deliberately conservative — a line that does not match a known red
// shape contributes nothing — so a noisy log never invents a shard. New runners extend
// this list rather than loosening an existing pattern.
var failPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*--- FAIL:\s+(\S+)`),
	regexp.MustCompile(`^FAILED\s+(\S+)`),
	regexp.MustCompile(`^FAIL:\s+(\S+)`),
	regexp.MustCompile(`^not ok\s+\d+\s+-\s+(.+\S)`),
}

// parseFailures extracts distinct failing test identities from a runner's combined
// output, in first-seen order (deterministic). A test that appears on multiple lines
// (e.g. a Go subtest plus its parent) is de-duplicated so it yields ONE shard, not
// several. The scan is line-oriented and pattern-driven; anything not matching a known
// red shape is ignored.
func parseFailures(out string) []string {
	seen := make(map[string]bool)
	var names []string
	sc := bufio.NewScanner(strings.NewReader(out))
	// A test failure dump can be large; raise the line cap above bufio's 64KiB default
	// so a long assertion message never truncates a match mid-line.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		for _, re := range failPatterns {
			m := re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			name := strings.TrimSpace(m[1])
			if name == "" || seen[name] {
				break
			}
			seen[name] = true
			names = append(names, name)
			break // one pattern per line is enough
		}
	}
	return names
}
