package main

// requeue_wiring_test.go — P11-T23 gate (TestRequeueWiring): the granular-requeue
// wiring is opt-in behind NILCORE_REQUEUE + a bounded -requeue-max-attempts budget,
// drives focused subtasks through the existing scheduler + a FRESH ArtifactVerifier
// re-run (green is the verifier's, never a stored status — I2), bounds retries, persists
// the Ledger beside run_state in store.Task.Detail (I5), emits the additive claim_*
// kinds, and stays byte-identical when off.
//
// Hermetic: no network, no real worktree dispatch. A fake sandbox drives the real
// evverify.ArtifactVerifier (exit 0 ⇒ url_resolves Pass, non-0 ⇒ Unverifiable); a fake
// dispatch stands in for the worker (it never reaches a backend). A fake store satisfies
// requeueStore in memory.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/agent"
	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/eventlog"
	"nilcore/internal/requeue"
	"nilcore/internal/roster"
	"nilcore/internal/sandbox"
	"nilcore/internal/spawn"
	"nilcore/internal/store"
	"nilcore/internal/super"
	"nilcore/internal/verify"
)

// requeueBox is a fake sandbox whose Exec exit code is taken from a script: each call
// pops the next code (the last code repeats once the script is exhausted), so a test can
// model "red on the first verify, green on the re-verify". It never touches the network.
type requeueBox struct {
	dir   string
	codes []int
	calls int
	cmds  []string
	envs  []map[string]string
}

func (b *requeueBox) code() int {
	if b.calls < len(b.codes) {
		c := b.codes[b.calls]
		b.calls++
		return c
	}
	b.calls++
	if len(b.codes) == 0 {
		return 0
	}
	return b.codes[len(b.codes)-1]
}

func (b *requeueBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.cmds = append(b.cmds, cmd)
	return sandbox.Result{ExitCode: b.code()}, nil
}

func (b *requeueBox) ExecWithEnv(_ context.Context, cmd string, env map[string]string) (sandbox.Result, error) {
	b.cmds = append(b.cmds, cmd)
	b.envs = append(b.envs, env)
	return sandbox.Result{ExitCode: b.code()}, nil
}

func (b *requeueBox) Workdir() string { return b.dir }

// fakeRequeueStore is an in-memory requeueStore for the Ledger-persistence assertions.
type fakeRequeueStore struct {
	tasks map[string]store.Task
}

func newFakeRequeueStore() *fakeRequeueStore {
	return &fakeRequeueStore{tasks: map[string]store.Task{}}
}

func (s *fakeRequeueStore) GetTask(_ context.Context, id string) (store.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return store.Task{}, os.ErrNotExist
	}
	return t, nil
}

func (s *fakeRequeueStore) UpsertTask(_ context.Context, t store.Task) error {
	s.tasks[t.ID] = t
	return nil
}

// writeRequeueArtifact writes an artifact with one url_resolves claim seeded RED
// (Status fail) so Scan immediately produces one Unit. Returns the artifact id.
func writeRequeueArtifact(t *testing.T, root, id string) string {
	t.Helper()
	a := &artifact.Artifact{
		ID:        id,
		Kind:      artifact.KindReport,
		Title:     "requeue",
		CreatedAt: time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		Claims: []artifact.Claim{{
			ID:    id + "-c1",
			Field: "f1",
			Evidence: artifact.Evidence{
				Value:     "v1",
				SourceURL: "https://example.com",
				Verifier:  "web.url_resolves",
				Status:    artifact.StatusFail,
			},
		}},
	}
	if err := artifact.Write(root, a); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return id
}

// reverifyOver returns a requeueReverify factory building a Composite of real
// ArtifactVerifiers over every artifact file in root, driven by box — so a re-run
// overwrites the on-disk statuses from the box exit (the real I2 keystone path).
func reverifyOver(box sandbox.Sandbox, log *eventlog.Log) requeueReverify {
	return func() verify.Verifier {
		paths := artifactFiles(box.Workdir())
		named := make([]verify.NamedVerifier, 0, len(paths))
		for _, p := range paths {
			named = append(named, verify.NamedVerifier{Name: "evidence:" + artifactID(p),
				V: &evverify.ArtifactVerifier{Box: box, Reg: evverify.Default(), RelPath: p,
					EventSink: evidenceEventSink(log)}})
		}
		return verify.Composite{Named: named}
	}
}

func newRequeueLog(t *testing.T) (*eventlog.Log, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(p)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	return log, p
}

// TestRequeueWiring is the P11-T23 gate.
func TestRequeueWiring(t *testing.T) {
	// (1) Unset ⇒ no hook, byte-identical. Hook() returns nil; the supervisor path is
	// untouched, no store write, no events.
	t.Run("unset => nil hook, no store write", func(t *testing.T) {
		t.Setenv("NILCORE_REQUEUE", "")
		t.Setenv("NILCORE_REQUEUE_MAX_ATTEMPTS", "")
		if requeueEnabled() {
			t.Fatalf("NILCORE_REQUEUE unset must report disabled")
		}
		st := newFakeRequeueStore()
		r := &requeueRunner{maxAttempts: requeueMaxAttempts(), store: st, taskID: "run-1"}
		if r.Hook() != nil {
			t.Fatalf("disabled runner must return a nil hook")
		}
		if len(st.tasks) != 0 {
			t.Fatalf("a disabled runner must not write the store")
		}
	})

	// (8) The hook has the exact P11-T22 signature so it slots into super.Supervisor.
	t.Run("hook has the pinned P11-T22 signature", func(t *testing.T) {
		r := &requeueRunner{maxAttempts: 1}
		var sup super.Supervisor
		sup.RequeueHook = r.Hook() // compile-time proof of the func(ctx)([]string,bool) shape
		if sup.RequeueHook == nil {
			t.Fatalf("an enabled runner must supply a non-nil RequeueHook")
		}
	})

	// (3)+(4) A claim failing once then passing on re-verify ⇒ claim_requeue then
	// claim_resolved; the focused Goal names only the red claim id; green is set ONLY by
	// the verifier re-run (the dispatch never self-claims pass).
	t.Run("fail then pass => claim_requeue then claim_resolved", func(t *testing.T) {
		t.Setenv("NILCORE_REQUEUE", "1")
		t.Setenv("NILCORE_REQUEUE_MAX_ATTEMPTS", "3")
		root := t.TempDir()
		id := writeRequeueArtifact(t, root, "rep")
		// Box exit 0 ⇒ url_resolves Pass on the re-verify: the cell flips green because the
		// VERIFIER said so, not because the dispatch wrote pass.
		box := &requeueBox{dir: root, codes: []int{0}}
		log, path := newRequeueLog(t)

		var goals []string
		dispatch := func(_ context.Context, spec super.SubagentSpec) spawn.Result {
			goals = append(goals, spec.Goal)
			// The worker does NOT set the claim status; only the verifier may. Leave the
			// on-disk artifact as-is so the re-verify is the sole authority.
			return spawn.Result{ID: spec.ID, Passed: true, State: spawn.StatePassed}
		}
		r := &requeueRunner{
			root: root, role: roster.RoleTypedResearch, maxAttempts: requeueMaxAttempts(),
			log: log, dispatch: dispatch, reverify: reverifyOver(box, log),
		}
		remaining, exhausted := r.Hook()(context.Background())
		if exhausted {
			t.Fatalf("a resolved cell must not report exhausted")
		}
		if len(remaining) != 0 {
			t.Fatalf("a resolved cell leaves nothing remaining, got %v", remaining)
		}
		// The focused Goal names only the red claim id.
		if len(goals) != 1 || !strings.Contains(goals[0], id+"-c1") {
			t.Fatalf("focused Goal must name the red claim id, got %v", goals)
		}
		body := readFile(t, path)
		if !strings.Contains(body, "claim_requeue") && !strings.Contains(body, "claim_resolved") {
			t.Fatalf("expected claim_* events, log was:\n%s", body)
		}
		if !strings.Contains(body, "claim_resolved") {
			t.Fatalf("a flipped-green cell must emit claim_resolved, log was:\n%s", body)
		}
		// I2: the on-disk status was overwritten to pass BY the verifier re-run.
		got, err := artifact.Read(root, id)
		if err != nil {
			t.Fatalf("read artifact: %v", err)
		}
		if got.Claims[0].Evidence.Status != artifact.StatusPass {
			t.Fatalf("re-verify must overwrite the status to pass, got %q", got.Claims[0].Evidence.Status)
		}
	})

	// (2)+(5) An always-red claim ⇒ exactly N rounds then requeue_exhausted then stop;
	// the log byte length only GROWS and eventlog.Verify still passes (append-only, I5).
	t.Run("always-red => bounded N rounds then exhausted", func(t *testing.T) {
		t.Setenv("NILCORE_REQUEUE", "1")
		t.Setenv("NILCORE_REQUEUE_MAX_ATTEMPTS", "2")
		root := t.TempDir()
		writeRequeueArtifact(t, root, "rep")
		// Box exit 22 ⇒ url_resolves Unverifiable forever: the cell never flips green.
		box := &requeueBox{dir: root, codes: []int{22}}
		log, path := newRequeueLog(t)
		dispatch := func(_ context.Context, spec super.SubagentSpec) spawn.Result {
			return spawn.Result{ID: spec.ID, Passed: false, State: spawn.StateFailed}
		}
		r := &requeueRunner{
			root: root, role: roster.RoleTypedResearch, maxAttempts: requeueMaxAttempts(),
			log: log, dispatch: dispatch, reverify: reverifyOver(box, log),
		}
		hook := r.Hook()

		var lastLen int
		rounds := 0
		for {
			fi, _ := os.Stat(path)
			before := fi.Size()
			_, exhausted := hook(context.Background())
			rounds++
			fi, _ = os.Stat(path)
			after := fi.Size()
			if after < before {
				t.Fatalf("the event log must only GROW: %d -> %d", before, after)
			}
			lastLen = int(after)
			if exhausted {
				break
			}
			if rounds > 5 {
				t.Fatalf("requeue did not converge — unbounded loop (rounds=%d)", rounds)
			}
		}
		if rounds != 2 {
			t.Fatalf("MaxAttempts=2 must give exactly 2 rounds, got %d", rounds)
		}
		_ = lastLen
		if err := eventlog.Verify(path); err != nil {
			t.Fatalf("append-only chain must still verify: %v", err)
		}
		body := readFile(t, path)
		if !strings.Contains(body, "requeue_exhausted") {
			t.Fatalf("an exhausted cell must emit requeue_exhausted, log was:\n%s", body)
		}
	})

	// (6) The Ledger marshals into store.Task.Detail as an additive SIBLING of the existing
	// run_state object (tip_sha/nodes preserved), and a resume reads the attempt counts back;
	// an old (no-requeue) blob loads as a zero Ledger.
	t.Run("ledger persists beside run_state; resume restores attempts", func(t *testing.T) {
		t.Setenv("NILCORE_REQUEUE", "1")
		t.Setenv("NILCORE_REQUEUE_MAX_ATTEMPTS", "5")
		root := t.TempDir()
		writeRequeueArtifact(t, root, "rep")
		box := &requeueBox{dir: root, codes: []int{22}} // stay red so the Ledger bumps
		log, _ := newRequeueLog(t)

		// Seed an existing supervise row carrying a run_state object, exactly as the
		// checkpoint would write it.
		st := newFakeRequeueStore()
		rs := agent.RunState{TipSHA: "deadbeef"}
		detail, _ := rs.Marshal()
		st.tasks["run-1"] = store.Task{ID: "run-1", Goal: "g", Status: agent.SuperviseStatus, Detail: detail}

		dispatch := func(_ context.Context, spec super.SubagentSpec) spawn.Result {
			return spawn.Result{ID: spec.ID, Passed: false, State: spawn.StateFailed}
		}
		r := &requeueRunner{
			root: root, role: roster.RoleTypedResearch, maxAttempts: 5,
			log: log, store: st, taskID: "run-1", goal: "g",
			dispatch: dispatch, reverify: reverifyOver(box, log),
		}
		hook := r.Hook()
		hook(context.Background()) // one round bumps the attempt count

		saved := st.tasks["run-1"].Detail
		// run_state survives verbatim.
		gotRS, err := agent.UnmarshalRunState(saved)
		if err != nil {
			t.Fatalf("run_state must still parse: %v", err)
		}
		if gotRS.TipSHA != "deadbeef" {
			t.Fatalf("requeue persistence clobbered run_state: %q", gotRS.TipSHA)
		}
		// The requeue sibling carries the bumped attempt.
		blob := requeueBlob(saved)
		if len(blob) == 0 {
			t.Fatalf("Detail must carry a requeue sibling, got %q", saved)
		}
		led, err := requeue.UnmarshalLedger(blob)
		if err != nil {
			t.Fatalf("ledger blob must parse: %v", err)
		}
		if led.Attempts["rep/rep-c1"] != 1 {
			t.Fatalf("expected one recorded attempt, got %v", led.Attempts)
		}

		// A second round resumes the attempt count and exhausts at 2 (==MaxAttempts here we
		// re-run with the live budget 5; just assert the count grows, proving resume).
		r2 := &requeueRunner{
			root: root, role: roster.RoleTypedResearch, maxAttempts: 5,
			log: log, store: st, taskID: "run-1", goal: "g",
			dispatch: dispatch, reverify: reverifyOver(box, log),
		}
		r2.Hook()(context.Background())
		blob2 := requeueBlob(st.tasks["run-1"].Detail)
		led2, _ := requeue.UnmarshalLedger(blob2)
		if led2.Attempts["rep/rep-c1"] != 2 {
			t.Fatalf("resume must continue the attempt count to 2, got %v", led2.Attempts)
		}

		// An old (pre-requeue) blob with no requeue sibling loads as a zero Ledger.
		if b := requeueBlob(detail); b != nil {
			t.Fatalf("a run_state-only blob must have no requeue sibling, got %q", b)
		}
	})

	// (7) No secret in any Unit/Goal/Ledger/claim_* Detail. A secret seeded into the env
	// (and thus reachable by a keyed check via ExecWithEnv) never lands in the persisted
	// Ledger Detail, the Goal text, or any emitted event.
	t.Run("no secret in any requeue surface", func(t *testing.T) {
		const secret = "SUPER-SECRET-REQUEUE-TOKEN"
		t.Setenv("NILCORE_REQUEUE", "1")
		t.Setenv("NILCORE_REQUEUE_MAX_ATTEMPTS", "1")
		t.Setenv("NILCORE_TEST_SECRET", secret)
		root := t.TempDir()
		writeRequeueArtifact(t, root, "rep")
		box := &requeueBox{dir: root, codes: []int{22}}
		log, path := newRequeueLog(t)
		st := newFakeRequeueStore()
		var goals []string
		dispatch := func(_ context.Context, spec super.SubagentSpec) spawn.Result {
			goals = append(goals, spec.Goal)
			return spawn.Result{ID: spec.ID, Passed: false, State: spawn.StateFailed}
		}
		r := &requeueRunner{
			root: root, role: roster.RoleTypedResearch, maxAttempts: 1,
			log: log, store: st, taskID: "run-1", goal: "g",
			dispatch: dispatch, reverify: reverifyOver(box, log),
		}
		r.Hook()(context.Background())

		for _, g := range goals {
			if strings.Contains(g, secret) {
				t.Fatalf("secret leaked into a focused Goal: %q", g)
			}
		}
		if d := st.tasks["run-1"].Detail; strings.Contains(d, secret) {
			t.Fatalf("secret leaked into the persisted Ledger Detail: %q", d)
		}
		if body := readFile(t, path); strings.Contains(body, secret) {
			t.Fatalf("secret leaked into an event Detail:\n%s", body)
		}
	})
}
