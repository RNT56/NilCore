package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"nilcore/internal/model"
	"nilcore/internal/summarize"
)

// fakeStore is an in-memory Store: a map keyed by conversation ID. It records
// every save so a test can assert exactly one bounded-state blob (no transcript)
// was written, and can inject a load/save error to exercise the best-effort path.
type fakeStore struct {
	mu       sync.Mutex
	saved    map[string]string // id -> detail JSON
	goals    map[string]string // id -> goal label
	saves    int
	loadErr  error
	saveErr  error
	notFound bool // force LoadConversation to report not-found
}

func newFakeStore() *fakeStore {
	return &fakeStore{saved: map[string]string{}, goals: map[string]string{}}
}

func (f *fakeStore) SaveConversation(_ context.Context, id, goal, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saves++
	f.saved[id] = detail
	f.goals[id] = goal
	return nil
}

func (f *fakeStore) LoadConversation(_ context.Context, id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return "", false, f.loadErr
	}
	if f.notFound {
		return "", false, nil
	}
	d, ok := f.saved[id]
	return d, ok, nil
}

func (f *fakeStore) detail(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saved[id]
}

func (f *fakeStore) saveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

// sampleState is a representative bounded WorkState used across the round-trip
// tests: a non-trivial summary, an active driver, a branch, and a last outcome.
func sampleState() WorkState {
	return WorkState{
		Summary: summarize.ContextSummary{
			Goal:        "ship the URL shortener",
			Constraints: []string{"stdlib only", "tests pass"},
			Decisions:   []string{"chose net/http", "in-memory map store"},
			Remaining:   "add a rate limiter",
		},
		Active:      RouteProject,
		Branch:      "integration-tip",
		LastOutcome: "verify passed; 12 tests green",
		Mode:        ModePlan,
	}
}

// --- encode/decode round-trip (the wire shape is total and stable) -------------

func TestWorkStateEncodeDecodeRoundTrip(t *testing.T) {
	in := sampleState()
	out := encodeState(in).decode()

	if out.Summary.Goal != in.Summary.Goal {
		t.Errorf("Goal = %q, want %q", out.Summary.Goal, in.Summary.Goal)
	}
	if strings.Join(out.Summary.Constraints, "|") != strings.Join(in.Summary.Constraints, "|") {
		t.Errorf("Constraints = %v, want %v", out.Summary.Constraints, in.Summary.Constraints)
	}
	if strings.Join(out.Summary.Decisions, "|") != strings.Join(in.Summary.Decisions, "|") {
		t.Errorf("Decisions = %v, want %v", out.Summary.Decisions, in.Summary.Decisions)
	}
	if out.Summary.Remaining != in.Summary.Remaining {
		t.Errorf("Remaining = %q, want %q", out.Summary.Remaining, in.Summary.Remaining)
	}
	if out.Active != in.Active {
		t.Errorf("Active = %v, want %v", out.Active, in.Active)
	}
	if out.Branch != in.Branch {
		t.Errorf("Branch = %q, want %q", out.Branch, in.Branch)
	}
	if out.LastOutcome != in.LastOutcome {
		t.Errorf("LastOutcome = %q, want %q", out.LastOutcome, in.LastOutcome)
	}
	if out.Mode != in.Mode {
		t.Errorf("Mode = %v, want %v", out.Mode, in.Mode)
	}
}

// A pinned mode is a SAFETY posture (e.g. /plan = "do not write my repo"), so it
// MUST survive a restart: a conversation that resumes must come back in the same
// mode rather than silently defaulting to full-capability ModeAuto. This asserts
// the round-trip through the store and Restore, and that every mode name maps back.
func TestModePersistsAcrossRestore(t *testing.T) {
	for _, m := range []Mode{ModeAuto, ModeDiscuss, ModePlan, ModeExecute} {
		store := newFakeStore()

		// First session: pin the mode and persist its bounded state.
		s1 := New("convo-mode", "local", "/repo", nil)
		s1.Store = store
		s1.SetMode(m)
		s1.persist(context.Background(), s1.snapshotState())

		// Second session (a "restart"): restore and confirm the mode came back.
		s2 := New("convo-mode", "local", "/repo", nil)
		s2.Store = store
		s2.Restore(context.Background())
		if got := s2.CurrentMode(); got != m {
			t.Errorf("restored mode = %v, want %v", got, m)
		}
	}
}

func TestModeFromStringMapsEveryName(t *testing.T) {
	cases := map[string]Mode{
		"auto":    ModeAuto,
		"discuss": ModeDiscuss,
		"plan":    ModePlan,
		"execute": ModeExecute,
		"":        ModeAuto, // empty: router decides
		"bogus":   ModeAuto, // unknown/forward-compat: never an unexpected pin
		"PLAN":    ModeAuto, // case-sensitive: unknown -> auto
	}
	for name, want := range cases {
		if got := ModeFromString(name); got != want {
			t.Errorf("ModeFromString(%q) = %v, want %v", name, got, want)
		}
		// And every real mode renders to a name that maps back (round-trip).
		if want != ModeAuto && ModeFromString(want.String()) != want {
			t.Errorf("round-trip %v -> %q -> %v", want, want.String(), ModeFromString(want.String()))
		}
	}
}

func TestRouteFromStringMapsEveryDispatchableRoute(t *testing.T) {
	cases := []struct {
		name string
		want Route
	}{
		{"native", RouteNative},
		{"supervise", RouteSupervise},
		{"project", RouteProject},
		{"chat", RouteChat},
		{"continue", RouteContinue},
		{"", RouteContinue},       // empty: fresh state
		{"bogus", RouteContinue},  // forward-compat / renamed: never mis-dispatch
		{"NATIVE", RouteContinue}, // case-sensitive: unknown -> continue
	}
	for _, tc := range cases {
		if got := routeFromString(tc.name); got != tc.want {
			t.Errorf("routeFromString(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// And every dispatchable route's String() must round-trip back to itself.
	for _, r := range []Route{RouteNative, RouteSupervise, RouteProject, RouteChat} {
		if got := routeFromString(r.String()); got != r {
			t.Errorf("round-trip %v: String=%q -> %v", r, r.String(), got)
		}
	}
}

// --- persist writes only bounded state, never a raw transcript -----------------

func TestPersistWritesBoundedStateNoTranscript(t *testing.T) {
	st := newFakeStore()
	s := New("conv-1", "local", "/repo", nil)
	s.Store = st

	// History carries a secret-looking transcript line that must NEVER be written.
	const secret = "TRANSCRIPT-SHOULD-NOT-PERSIST-xyz"
	s.History = []model.Message{userTurn(secret)}

	s.persist(context.Background(), sampleState())

	if st.saveCount() != 1 {
		t.Fatalf("save count = %d, want 1", st.saveCount())
	}
	blob := st.detail("conv-1")
	if blob == "" {
		t.Fatal("nothing persisted")
	}
	if strings.Contains(blob, secret) {
		t.Fatalf("persisted blob contains raw transcript text: %q", blob)
	}
	// The bounded fields ARE present.
	for _, want := range []string{"ship the URL shortener", "integration-tip", "project"} {
		if !strings.Contains(blob, want) {
			t.Errorf("persisted blob missing bounded field %q: %s", want, blob)
		}
	}
}

// --- nil Store is in-memory only: never panics, never persists ----------------

func TestPersistNilStoreIsInMemoryOnly(t *testing.T) {
	s := New("conv-nil", "local", "/repo", nil)
	// No Store wired.
	s.persist(context.Background(), sampleState()) // must not panic
	if restored := s.Restore(context.Background()); restored {
		t.Fatal("Restore with nil Store reported restored=true")
	}
	if err := s.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint with nil Store = %v, want nil", err)
	}
}

// --- store errors are best-effort: logged + swallowed, state left clean --------

func TestRestoreSwallowsStoreError(t *testing.T) {
	st := newFakeStore()
	st.loadErr = errors.New("db unavailable")
	s := New("conv-err", "local", "/repo", nil)
	s.Store = st

	if restored := s.Restore(context.Background()); restored {
		t.Fatal("Restore reported restored=true on a store error")
	}
	if s.State.Summary.Goal != "" {
		t.Fatalf("State mutated despite load error: %+v", s.State)
	}
}

func TestRestoreCorruptBlobStartsFresh(t *testing.T) {
	st := newFakeStore()
	st.saved["conv-bad"] = "{not valid json"
	s := New("conv-bad", "local", "/repo", nil)
	s.Store = st

	if restored := s.Restore(context.Background()); restored {
		t.Fatal("Restore reported restored=true on a corrupt blob")
	}
	if s.State.Summary.Goal != "" {
		t.Fatalf("State mutated despite corrupt blob: %+v", s.State)
	}
}

func TestPersistSwallowsSaveError(t *testing.T) {
	st := newFakeStore()
	st.saveErr = errors.New("disk full")
	s := New("conv-save-err", "local", "/repo", nil)
	s.Store = st

	// persist must not panic or propagate; the drive must not fail on a store fault.
	s.persist(context.Background(), sampleState())

	// Checkpoint (the explicit shutdown path) DOES surface the error.
	if err := s.Checkpoint(context.Background()); err == nil {
		t.Fatal("Checkpoint swallowed a save error; want it surfaced")
	}
}

// --- Restore on a never-seen conversation: clean fresh start -------------------

func TestRestoreNotFoundIsFreshStart(t *testing.T) {
	st := newFakeStore()
	st.notFound = true
	s := New("conv-new", "local", "/repo", nil)
	s.Store = st

	if restored := s.Restore(context.Background()); restored {
		t.Fatal("Restore reported restored=true for a never-seen conversation")
	}
}

// --- end-to-end: a terminal drive persists; New+Restore continues, not restarts -

func TestSessionWorkStateRoundTripsAndContinues(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()

	// First process: a drive terminates and folds a project work-state.
	rt := &fakeRouter{route: RouteProject}
	drv := newFakeDriver(DriveResult{
		Summary:  summarize.ContextSummary{Goal: "build the service", Remaining: "write a README"},
		Branch:   "tip-1",
		Outcome:  "verify passed",
		Verified: true,
	})
	s1 := New("conv-rt", "local", "/repo", nil)
	s1.Store = st
	s1.Router = rt
	s1.Drivers = Drivers{Project: drv}

	if err := s1.Turn(ctx, "build me a service"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	<-drv.started
	close(drv.release)
	s1.Wait()

	if st.saveCount() == 0 {
		t.Fatal("terminal drive did not persist the work-state")
	}
	if got := st.goals["conv-rt"]; got != "build the service" {
		t.Fatalf("persisted goal label = %q, want %q", got, "build the service")
	}

	// Second process: a fresh Session for the SAME conversation ID restores the
	// bounded state. The active driver pointer survives so a RouteContinue
	// follow-up re-enters the project driver — continue, not restart.
	s2 := New("conv-rt", "local", "/repo", nil)
	s2.Store = st
	if restored := s2.Restore(ctx); !restored {
		t.Fatal("Restore reported restored=false after a prior persist")
	}
	if s2.State.Summary.Goal != "build the service" {
		t.Fatalf("restored Goal = %q, want %q", s2.State.Summary.Goal, "build the service")
	}
	if s2.State.Active != RouteProject {
		t.Fatalf("restored Active = %v, want project", s2.State.Active)
	}
	if s2.State.Branch != "tip-1" {
		t.Fatalf("restored Branch = %q, want tip-1", s2.State.Branch)
	}
	if s2.State.LastOutcome != "verify passed" {
		t.Fatalf("restored LastOutcome = %q, want %q", s2.State.LastOutcome, "verify passed")
	}

	// The continue follow-up dispatches via the restored Active driver.
	contDrv := newFakeDriver(DriveResult{Verified: true})
	s2.Router = &fakeRouter{route: RouteContinue}
	s2.Drivers = Drivers{Project: contDrv}
	if err := s2.Turn(ctx, "now write the README"); err != nil {
		t.Fatalf("continue Turn: %v", err)
	}
	<-contDrv.started
	if in := contDrv.input(); in.Route != RouteContinue {
		t.Fatalf("continue drive Route = %v, want continue", in.Route)
	}
	close(contDrv.release)
	s2.Wait()
}

// --- delivery loop: the kept verified branch survives a restart -----------------

// TestKeptBranchRoundTrip is the delivery-loop persistence contract: a verified
// drive's KEPT branch (DriveResult.Branch → WorkState.Branch) is persisted, a
// restarted session restores it (so /diff and /apply keep working), and
// ClearKeptBranch — the /apply-landed epilogue — clears it durably so a second
// restart does not resurrect an already-merged branch.
func TestKeptBranchRoundTrip(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()

	drv := newFakeDriver(DriveResult{
		Summary:  summarize.ContextSummary{Goal: "fix the typo"},
		Branch:   "nilcore/kept/chat-local-1",
		Verified: true,
	})
	s1 := New("conv-kept", "local", "/repo", nil)
	s1.Store = st
	s1.Router = &fakeRouter{route: RouteNative}
	s1.Drivers = Drivers{Native: drv}
	if err := s1.Turn(ctx, "fix the typo"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	<-drv.started
	close(drv.release)
	s1.Wait()
	if got := s1.KeptBranch(); got != "nilcore/kept/chat-local-1" {
		t.Fatalf("KeptBranch after fold = %q, want the kept branch", got)
	}

	// Restart: the kept branch must come back with the bounded state.
	s2 := New("conv-kept", "local", "/repo", nil)
	s2.Store = st
	if !s2.Restore(ctx) {
		t.Fatal("Restore reported restored=false after a persisted kept branch")
	}
	if got := s2.KeptBranch(); got != "nilcore/kept/chat-local-1" {
		t.Fatalf("restored KeptBranch = %q, want the kept branch", got)
	}

	// /apply landed it: ClearKeptBranch clears AND persists the cleared state.
	s2.ClearKeptBranch(ctx)
	if got := s2.KeptBranch(); got != "" {
		t.Fatalf("KeptBranch after clear = %q, want empty", got)
	}
	s3 := New("conv-kept", "local", "/repo", nil)
	s3.Store = st
	s3.Restore(ctx)
	if got := s3.KeptBranch(); got != "" {
		t.Fatalf("KeptBranch resurrected after clear+restart: %q", got)
	}
}

// ClearKeptBranch with nothing carried is a no-op: no persist, no event.
func TestClearKeptBranchEmptyNoop(t *testing.T) {
	st := newFakeStore()
	s := New("conv-noop", "local", "/repo", nil)
	s.Store = st
	s.ClearKeptBranch(context.Background())
	if st.saveCount() != 0 {
		t.Fatalf("ClearKeptBranch on an empty branch persisted %d times, want 0", st.saveCount())
	}
}
