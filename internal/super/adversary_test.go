package super

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/agent/bus"
	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/roster"
	"nilcore/internal/sandbox"
	"nilcore/internal/spawn"
)

// Package-level adversary regression suite (P6-T01, docs/MULTI-AGENT.md §1/§14).
// Each test is one failing-then-passing guard over a named mitigation. The suite
// is HERMETIC: every model/verifier/spawn/sandbox is a scripted fake, no network,
// temp dirs only. The mitigations covered HERE (super package) are:
//
//   - combined-chain eventlog.Verify over a two-subagent run (one shared *Log →
//     one hash chain that validates end to end);
//   - the structured promote-gate: a reversible throwaway merge is never gated;
//     only a deliberate policy.PromoteToBase reaches the approver, and a nil
//     approver default-denies (no ambient authority for the one irreversible step);
//   - the un-sandboxed-worker guard reached through the real spawn seam: the only
//     way the supervisor builds a worker is roster.NewWorker, which never yields a
//     backend.Native with a nil Box (closes adversary R1);
//   - secret-redaction across summary/bus/log: a credential a subagent emits in a
//     summary or a bus finding never lands in the append-only event log.
//
// The bus-injection/cycle/TTL caps and integration green-alone/red-combined denial
// have their own adversary guards in internal/agent/bus and internal/integrate.

// recordingSandbox is a sandbox.Sandbox that satisfies the interface without ever
// executing anything — the adversary tests assert WIRING, they never run a shell.
// It is the multi-agent equivalent of the roster package's fakeBox, kept local so
// this file is self-contained.
type recordingSandbox struct{}

func (recordingSandbox) Exec(context.Context, string) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (recordingSandbox) ExecWithEnv(context.Context, string, map[string]string) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (recordingSandbox) Workdir() string { return "/work" }

// openChainLog opens a fresh hash-chained event log on disk and returns it plus
// its path, so a test can run a cohort against the shared *Log and then call
// eventlog.Verify on the exact file the whole tree wrote to.
func openChainLog(t *testing.T) (*eventlog.Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, path
}

// readLogEvents closes the log and decodes every appended event from disk, so a
// test can assert what the combined chain recorded (kinds + redaction).
func readLogEvents(t *testing.T, log *eventlog.Log, path string) []eventlog.Event {
	t.Helper()
	if err := log.Close(); err != nil {
		t.Fatalf("close log: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var out []eventlog.Event
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e eventlog.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// --- (5) combined-chain eventlog.Verify over a two-subagent run --------------

// A full supervised run — two subagents spawned (one depending on the other),
// awaited, integrated, and finished — writes EVERY step (super_*, subagent_*,
// integration_* via the noop seam, bus_*) onto ONE shared *eventlog.Log. The
// resulting single hash chain must validate end to end with eventlog.Verify (I5:
// the whole tree shares one Log so one Verify covers the combined chain). This is
// the headline acceptance: "eventlog.Verify passes on a multi-agent run's
// combined chain."
func TestCombinedChainVerifies(t *testing.T) {
	log, path := openChainLog(t)

	// A real bus on the SAME log, so bus_* events interleave into the one chain
	// alongside the supervisor's super_*/subagent_* events. A subagent shares a
	// fenced finding mid-run; that bus_send/bus_deliver is part of the chain too.
	b := bus.New(log, 8, 0)

	s := baseSup(&scriptModel{}, passVerifier{})
	s.Log = log
	s.Bus = b

	// Spawning is synchronous in the supervisor; the spawn seam shares a finding on
	// the bus to prove a peer-to-peer bus event is folded into the same chain.
	s.Spawn = func(ctx context.Context, spec SubagentSpec) spawn.Result {
		_ = b.Send(ctx, bus.Message{
			Sender: spec.ID, To: []bus.AgentID{bus.Supervisor}, Kind: bus.KindFinding,
			TTL: 4, Payload: "subagent " + spec.ID + " is green",
		})
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID, Summary: "ok"}
	}
	var order []string
	s.Integrate = noopIntegrate("integrate/tip", &order)

	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "lib"})),
		textResp(toolUse("u2", "spawn_subagent", SubagentSpec{ID: "super.t2", Role: roster.RoleImplementer, Goal: "app", DependsOn: []string{"super.t1"}})),
		textResp(toolUse("u3", "await_results", map[string]any{})),
		textResp(toolUse("u4", "integrate", map[string]any{})),
		textResp(toolUse("u5", "finish", map[string]string{"summary": "shipped"})),
	}}
	s.Model = m

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := s.Run(ctx, "build the thing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Done || out.Reason != "converged" {
		t.Fatalf("want a converged run, got Done=%t reason=%q", out.Done, out.Reason)
	}
	if out.Spawned != 2 {
		t.Fatalf("want 2 subagents spawned, got %d", out.Spawned)
	}

	events := readLogEvents(t, log, path)
	// The chain must actually carry the multi-agent steps (not be trivially short):
	// two subagent_spawn, two subagent_report, the supervisor turns, and at least
	// one bus event from the shared finding.
	gotKinds := map[string]int{}
	for _, e := range events {
		gotKinds[e.Kind]++
	}
	for _, want := range []string{"super_start", "subagent_spawn", "subagent_report", "super_integrate", "super_verify", "super_done"} {
		if gotKinds[want] == 0 {
			t.Errorf("combined chain missing %q event (kinds=%v)", want, gotKinds)
		}
	}
	if gotKinds["subagent_spawn"] != 2 {
		t.Errorf("want 2 subagent_spawn events, got %d", gotKinds["subagent_spawn"])
	}
	if gotKinds["bus_send"] == 0 && gotKinds["bus_deliver"] == 0 {
		t.Errorf("expected the shared bus finding to be recorded on the combined chain; kinds=%v", gotKinds)
	}

	// THE acceptance: the single combined chain validates end to end. A multi-agent
	// run that broke the chain (a dropped/reordered/forged event) would fail here.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("eventlog.Verify failed on the multi-agent run's combined chain: %v", err)
	}
}

// A combined chain that is tampered with — a single body byte flipped in one
// recorded event — must FAIL eventlog.Verify. This is the "failing" half of the
// guard: it proves the Verify check above is load-bearing, not vacuous, over a
// real multi-agent run's chain.
func TestCombinedChainDetectsTamper(t *testing.T) {
	log, path := openChainLog(t)
	s := baseSup(&scriptModel{}, passVerifier{})
	s.Log = log
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}
	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "x"})),
		textResp(toolUse("u2", "finish", map[string]string{"summary": "done"})),
	}}
	s.Model = m
	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Untampered: the chain verifies.
	if err := eventlog.Verify(path); err != nil {
		t.Fatalf("baseline chain should verify: %v", err)
	}

	// Tamper with the middle of the file (alter a recorded detail) WITHOUT fixing
	// the hash — exactly what an attacker editing history would do.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected a multi-event chain, got %d lines", len(lines))
	}
	// Tamper with a recorded event WITHOUT recomputing its hash — exactly what an
	// attacker editing history would do. We decode the second event, mutate its
	// Task field (a covered field), and re-encode it, leaving the stored Hash stale
	// so the chain no longer self-verifies. This is robust regardless of which kind
	// landed on line 1 (no reliance on a specific detail being present).
	var tampered eventlog.Event
	if err := json.Unmarshal([]byte(lines[1]), &tampered); err != nil {
		t.Fatalf("decode event to tamper: %v", err)
	}
	tampered.Task = tampered.Task + "-tampered" // change covered content; keep stale Hash
	reenc, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	lines[1] = string(reenc)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eventlog.Verify(path); err == nil {
		t.Fatal("eventlog.Verify accepted a tampered multi-agent chain — I5 violated")
	}
}

// --- (4) structured promote-gate: reversible never gated, only Promote gated --

// The supervisor's only irreversible step is a structured policy.PromoteToBase via
// policy.GateStructured. This guard asserts the structural property the whole
// design rests on (adversary major #5): a reversible throwaway integration
// merge/reset — described with the very words the legacy substring Classify trips
// on — is NEVER auto-gated, while a deliberate PromoteToBase IS gated (denied with
// a nil approver, allowed only by an approving one).
func TestStructuredPromoteGateOnlyGatesPromote(t *testing.T) {
	// The reversible integration steps the integrator performs, phrased exactly the
	// way the free-text classifier would flag them. They must never reach the gate.
	reversible := []string{
		"git merge --no-ff task/super.t1 into the integration worktree",
		"git reset --hard 9a1f2c0 to roll back the red merge",
	}
	// Precondition: these strings genuinely trip the legacy substring classifier,
	// so the danger the structured path avoids is real (the guard is not vacuous).
	for _, s := range reversible {
		if policy.Classify(s) != policy.Irreversible {
			t.Fatalf("precondition: Classify(%q) should be irreversible", s)
		}
	}

	approver := &countingApprover{allow: true}
	// A reversible integration step carries NO GateAction, so it never invokes the
	// gate at all — we model that by simply never constructing one. The structural
	// claim is that the ONLY thing that gates is a deliberately-typed PromoteToBase.
	promote := policy.GateAction{Type: policy.PromoteToBase, Branch: "main",
		Detail: reversible[0]} // even a promote whose Detail echoes "merge" gates by Type
	if !policy.GateStructured(promote, approver) {
		t.Fatal("a structured PromoteToBase must consult the approver and pass when approved")
	}
	if approver.calls != 1 {
		t.Fatalf("approver consulted %d times, want exactly 1 (Type-driven, not text-driven)", approver.calls)
	}

	// A nil approver default-denies the irreversible promote — no ambient authority.
	if policy.GateStructured(promote, nil) {
		t.Fatal("PromoteToBase with a nil approver must default-deny (no ambient authority)")
	}

	// And the supervisor's Gate seam, when wired to GateStructured with a deny-all
	// approver, refuses the promote — proving the supervisor cannot self-promote.
	s := baseSup(&scriptModel{}, passVerifier{})
	s.Gate = func(a policy.GateAction) bool {
		return policy.GateStructured(a, denyAllApprover{})
	}
	if s.Gate(policy.GateAction{Type: policy.PromoteToBase, Branch: "main"}) {
		t.Fatal("supervisor Gate wired to a deny-all approver must refuse PromoteToBase")
	}
}

// --- (1) un-sandboxed-worker guard reached through the real spawn seam --------

// The ONLY way the multi-agent path builds a worker is roster.NewWorker, which
// always wires a non-nil Box (closes adversary R1). This guard reaches that
// constructor through the spawn seam the supervisor actually invokes: a SpawnFunc
// that builds the worker for each requested role and asserts no path produces a
// backend.Native with a nil Box (an un-sandboxed worker that could run a
// model-emitted shell command on the host, I4).
func TestSpawnSeamNeverBuildsUnsandboxedWorker(t *testing.T) {
	r := roster.NewDefault(scriptProvider{}, scriptProvider{}, policy.Egress{})

	var built int
	s := baseSup(&scriptModel{}, passVerifier{})
	s.Roster = r
	s.Spawn = func(_ context.Context, spec SubagentSpec) spawn.Result {
		// This is the production wiring shape: resolve the role, then build the
		// worker ONLY via roster.NewWorker. There is no other constructor.
		p, ok := r.Resolve(spec.Role)
		if !ok {
			t.Errorf("role %q did not resolve", spec.Role)
			return spawn.Result{ID: spec.ID, Passed: false}
		}
		w := roster.NewWorker(p, recordingSandbox{}, passVerifier{}, s.Log, scriptProvider{}, nil)
		if w.Box == nil {
			t.Errorf("role %q: NewWorker produced a worker with a nil Box (un-sandboxed) — R1 regression", spec.Role)
		}
		if w.CommandGuard == nil {
			t.Errorf("role %q: worker has a nil CommandGuard — every model-emitted shell must be guarded", spec.Role)
		}
		built++
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID}
	}

	// Spawn one worker of every role through the supervisor loop, then finish.
	roles := []roster.Role{roster.RoleResearcher, roster.RoleUnderstander, roster.RolePlanner, roster.RoleImplementer, roster.RoleReviewer}
	var resp []model.Response
	for i, role := range roles {
		id := "super.t" + string(rune('1'+i))
		resp = append(resp, textResp(toolUse("u"+id, "spawn_subagent", SubagentSpec{ID: id, Role: role, Goal: "g"})))
	}
	resp = append(resp, textResp(toolUse("uf", "finish", map[string]string{"summary": "done"})))
	m := &scriptModel{responses: resp}
	s.Model = m
	s.MaxAgents = len(roles)

	if _, err := s.Run(context.Background(), "goal"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if built != len(roles) {
		t.Fatalf("built %d workers, want %d (one per role through the spawn seam)", built, len(roles))
	}
}

// --- (6) secret-redaction across summary / bus / log -------------------------

// A credential a subagent emits — in its Result.Summary AND in a bus finding —
// must never be persisted to the append-only event log (I3/I5). The redactor runs
// on every Detail map before write; this guard sends a token through both seams of
// a real run and asserts it is absent from the whole combined chain on disk.
func TestSecretRedactedAcrossSummaryBusAndLog(t *testing.T) {
	const secret = "sk-ABCDEF0123456789abcdef" // a sk-prefixed token shape redact() masks

	log, path := openChainLog(t)
	b := bus.New(log, 8, 0)

	s := baseSup(&scriptModel{}, passVerifier{})
	s.Log = log
	s.Bus = b
	s.Spawn = func(ctx context.Context, spec SubagentSpec) spawn.Result {
		// Leak the secret on the bus (a finding) — the bus logs metadata only, but
		// even if a body leaked, redact must catch the token.
		_ = b.Send(ctx, bus.Message{
			Sender: spec.ID, To: []bus.AgentID{bus.Supervisor}, Kind: bus.KindFinding,
			TTL: 4, Payload: "found a key: " + secret,
		})
		// And leak it in the worker's summary, which flows back into the supervisor
		// context fenced — and into any log Detail derived from the report.
		return spawn.Result{ID: spec.ID, Passed: true, Branch: "task/" + spec.ID,
			Summary: "I configured the deploy with token " + secret}
	}

	m := &scriptModel{responses: []model.Response{
		textResp(toolUse("u1", "spawn_subagent", SubagentSpec{ID: "super.t1", Role: roster.RoleImplementer, Goal: "x"})),
		textResp(toolUse("u2", "finish", map[string]string{"summary": "done with " + secret})),
	}}
	s.Model = m

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Run(ctx, "wire up the deploy with "+secret); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// No event Detail on the persisted combined chain may contain the raw secret —
	// across super_*, subagent_*, and bus_* events. The redactor is the backstop.
	for _, e := range readLogEvents(t, log, path) {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), secret) {
			t.Fatalf("event %q leaked the secret into the append-only log: %s", e.Kind, raw)
		}
	}
}

// --- local hermetic fakes ----------------------------------------------------

// scriptProvider is a no-op model.Provider for wiring roster workers/advisors in
// the spawn-seam guard (the worker never runs; only its wiring is asserted).
type scriptProvider struct{}

func (scriptProvider) Model() string { return "fake-exec" }
func (scriptProvider) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{}, nil
}

// countingApprover records how many times it was consulted so a test can assert
// the gate is Type-driven (one approve per structured action), and denyAllApprover
// always refuses — the default-deny stand-in for "no real approver wired".
type countingApprover struct {
	allow bool
	calls int
}

func (c *countingApprover) Approve(string) bool { c.calls++; return c.allow }

type denyAllApprover struct{}

func (denyAllApprover) Approve(string) bool { return false }
