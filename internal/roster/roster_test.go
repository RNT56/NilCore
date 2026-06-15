package roster

import (
	"context"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// --- hermetic fakes (no network, no container) ----------------------------

// fakeBox is a sandbox.Sandbox that records nothing and runs nothing — the
// worker tests never execute, they only assert the wiring NewWorker produced.
type fakeBox struct{}

func (fakeBox) Exec(context.Context, string) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (fakeBox) ExecWithEnv(context.Context, string, map[string]string) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}
func (fakeBox) Workdir() string { return "/work" }

// fakeProvider is a minimal model.Provider for wiring an advisor without a call.
type fakeProvider struct{ id string }

func (f fakeProvider) Model() string { return f.id }
func (f fakeProvider) Complete(context.Context, string, []model.Message, []model.Tool, int) (model.Response, error) {
	return model.Response{}, nil
}

type fakeVerifier struct{}

func (fakeVerifier) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: true}, nil
}

// defaultRoster builds the standard five-role roster with fake providers and a
// research allowlist that overlaps the tree egress (so intersection is testable).
func defaultRoster() *Roster {
	research := policy.Egress{Allowed: []string{"docs.example.com", "github.com", "*.wikipedia.org"}}
	return NewDefault(fakeProvider{"exec"}, fakeProvider{"strong"}, research)
}

// --- NewWorker always sandboxes (closes R1) -------------------------------

func TestNewWorkerNeverNilBox(t *testing.T) {
	r := defaultRoster()
	box := fakeBox{}
	log := &eventlog.Log{}
	for _, role := range r.Roles() {
		p, ok := r.Resolve(role)
		if !ok {
			t.Fatalf("role %q not resolvable", role)
		}
		w := NewWorker(p, box, fakeVerifier{}, log, fakeProvider{"exec"}, nil)
		if w == nil {
			t.Fatalf("role %q: NewWorker returned nil", role)
		}
		if w.Box == nil {
			t.Errorf("role %q: worker has a nil Box (un-sandboxed) — R1 regression", role)
		}
		if w.CommandGuard == nil {
			t.Errorf("role %q: worker has a nil CommandGuard", role)
		}
	}
}

// --- read-only roles: no write/git-write tools ----------------------------

func TestReadOnlyRegistryHasNoWriteTools(t *testing.T) {
	r := defaultRoster()
	for _, role := range r.Roles() {
		p, _ := r.Resolve(role)
		w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
		readOnly := role.ReadOnly()
		if p.ReadOnly != readOnly {
			t.Errorf("role %q: profile.ReadOnly=%v, role.ReadOnly()=%v — mismatch", role, p.ReadOnly, readOnly)
		}
		for _, wt := range []string{"write", "edit", "git"} {
			has := w.Tools.Has(wt)
			if readOnly && has {
				t.Errorf("read-only role %q advertises write/git-write tool %q", role, wt)
			}
			if role == RoleImplementer && !has {
				t.Errorf("implementer must have write tool %q", wt)
			}
		}
		// Read-only roles must still have the read tools they need.
		if readOnly {
			for _, rt := range []string{"read", "search"} {
				if !w.Tools.Has(rt) {
					t.Errorf("read-only role %q is missing read tool %q", role, rt)
				}
			}
		}
	}
}

// A profile that mistakenly carries a write registry on a read-only role must be
// hard-substituted with the write-free set by NewWorker (structural, not trust).
func TestReadOnlyProfileWithWriteRegistryIsOverridden(t *testing.T) {
	p := Profile{
		System:   "x",
		Tools:    writeToolset(), // wrong: a write registry on a read-only role
		ReadOnly: true,
	}
	w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	for _, wt := range []string{"write", "edit", "git"} {
		if w.Tools.Has(wt) {
			t.Errorf("read-only worker still has write tool %q despite a write registry profile", wt)
		}
	}
}

// --- read-only command policy denies in-tree writes -----------------------

func TestReadOnlyCommandPolicyDeniesInTreeWrites(t *testing.T) {
	r := defaultRoster()
	denied := []string{
		"echo hi > file.go",
		"echo hi >> file.go",
		"cat x | tee out.txt",
		"sed -i 's/a/b/' main.go",
		"mv a.go b.go",
		"cp src.go dst.go",
		"git commit -m wip",
		"git add -A",
		"git reset --hard",
		"pip install requests",
		"npm install left-pad",
		"go install ./...",
	}
	for _, role := range r.Roles() {
		if !role.ReadOnly() {
			continue
		}
		p, _ := r.Resolve(role)
		w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
		for _, cmd := range denied {
			if ok, _ := w.CommandGuard(cmd); ok {
				t.Errorf("read-only role %q allowed in-tree write command %q", role, cmd)
			}
		}
		// A read inspection command must still pass (read-only != no-ops).
		for _, ok := range []string{"git status", "git diff", "go build ./...", "ls -la"} {
			if allowed, reason := w.CommandGuard(ok); !allowed {
				t.Errorf("read-only role %q denied benign command %q: %s", role, ok, reason)
			}
		}
	}
}

// The implementer keeps the default (looser) policy: it may write in-tree and
// commit, but the destructive/host-boundary defaults still apply.
func TestImplementerCommandPolicy(t *testing.T) {
	r := defaultRoster()
	p, _ := r.Resolve(RoleImplementer)
	w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	for _, ok := range []string{"echo x > main.go", "git commit -m wip", "sed -i s/a/b/ x"} {
		if allowed, reason := w.CommandGuard(ok); !allowed {
			t.Errorf("implementer denied write command %q: %s", ok, reason)
		}
	}
	for _, bad := range []string{"rm -rf /", "git push origin main", "sudo rm x"} {
		if allowed, _ := w.CommandGuard(bad); allowed {
			t.Errorf("implementer allowed destructive/irreversible command %q", bad)
		}
	}
}

// --- egress: deny-all → network none; never a superset of the tree --------

func TestDenyAllRolesProduceEmptyEgress(t *testing.T) {
	r := defaultRoster()
	tree := policy.DefaultEgress() // a non-empty tree allowlist
	for _, role := range []Role{RoleUnderstander, RolePlanner, RoleReviewer} {
		p, _ := r.Resolve(role)
		eg := EgressFor(p, tree)
		if !eg.Empty() {
			t.Errorf("deny-all role %q produced non-empty egress %v (must be --network none)", role, eg.Allowed)
		}
	}
}

func TestEgressNeverSupersetOfTree(t *testing.T) {
	r := defaultRoster()
	// A tree that allows only a couple of hosts — narrower than DefaultEgress and
	// narrower than the researcher's research allowlist.
	tree := policy.Egress{Allowed: []string{"github.com", "proxy.golang.org"}}
	for _, role := range r.Roles() {
		p, _ := r.Resolve(role)
		eg := EgressFor(p, tree)
		// Every host the worker may reach must also be reachable under the tree —
		// the intersection can never widen past the tree (R9).
		for _, host := range eg.Allowed {
			if isWildcard(host) {
				continue // wildcard survival is checked structurally below
			}
			if !tree.Allow(host) {
				t.Errorf("role %q egress allows %q which the tree denies — superset escape", role, host)
			}
		}
		// And no wildcard may survive that the tree did not carry verbatim.
		for _, host := range eg.Allowed {
			if isWildcard(host) && !containsExact(tree.Allowed, host) {
				t.Errorf("role %q egress kept wildcard %q absent from the tree", role, host)
			}
		}
	}
}

// The researcher narrows to exactly the hosts both it and the tree allow.
func TestResearcherEgressIntersection(t *testing.T) {
	r := defaultRoster()
	p, _ := r.Resolve(RoleResearcher)
	// Tree allows github.com (overlaps research) + crates.io (research denies it).
	tree := policy.Egress{Allowed: []string{"github.com", "crates.io"}}
	eg := EgressFor(p, tree)
	if !eg.Allow("github.com") {
		t.Error("researcher should reach github.com (allowed by both role and tree)")
	}
	if eg.Allow("crates.io") {
		t.Error("researcher must not reach crates.io (tree allows, research role does not)")
	}
	if eg.Allow("docs.example.com") {
		t.Error("researcher must not reach docs.example.com (role allows, tree does not)")
	}
}

// Empty tree egress denies everyone — even the researcher and implementer.
func TestEmptyTreeDeniesAll(t *testing.T) {
	r := defaultRoster()
	for _, role := range r.Roles() {
		p, _ := r.Resolve(role)
		if eg := EgressFor(p, policy.Egress{}); !eg.Empty() {
			t.Errorf("role %q got network with an empty tree allowlist: %v", role, eg.Allowed)
		}
	}
}

// --- each worker gets its OWN advisor instance ----------------------------

func TestEachWorkerOwnsItsAdvisor(t *testing.T) {
	r := defaultRoster()
	p, _ := r.Resolve(RoleImplementer)
	box := fakeBox{}
	log := &eventlog.Log{}
	w1 := NewWorker(p, box, fakeVerifier{}, log, fakeProvider{"exec"}, nil)
	w2 := NewWorker(p, box, fakeVerifier{}, log, fakeProvider{"exec"}, nil)
	if w1.Advisor == nil || w2.Advisor == nil {
		t.Fatal("workers with a profile model should each get an advisor")
	}
	if w1.Advisor == w2.Advisor {
		t.Error("two workers share the same *advisor.Advisor — the per-subagent ceiling is broken (concurrency fix)")
	}
}

// A profile with no model gets no advisor and the loop stays unescalated.
func TestNoModelMeansNoAdvisor(t *testing.T) {
	p := Profile{System: "x", ReadOnly: true} // no Model
	w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	if w.Advisor != nil {
		t.Error("worker with no profile model should have a nil advisor")
	}
	if w.EscalateAfter != 0 {
		t.Errorf("EscalateAfter should be 0 with no advisor, got %d", w.EscalateAfter)
	}
}

// --- catalog behavior ------------------------------------------------------

func TestResolveUnknownRole(t *testing.T) {
	r := defaultRoster()
	if _, ok := r.Resolve(Role("nonexistent")); ok {
		t.Error("Resolve returned ok for an unknown role")
	}
	var nilR *Roster
	if _, ok := nilR.Resolve(RolePlanner); ok {
		t.Error("nil roster Resolve should report not-ok")
	}
}

func TestDefaultRosterHasAllFiveRoles(t *testing.T) {
	r := defaultRoster()
	want := []Role{RoleResearcher, RoleUnderstander, RolePlanner, RoleImplementer, RoleReviewer}
	for _, role := range want {
		if _, ok := r.Resolve(role); !ok {
			t.Errorf("default roster missing role %q", role)
		}
	}
	if got := len(r.Roles()); got != len(want) {
		t.Errorf("default roster has %d roles, want %d", got, len(want))
	}
}

// New copies the input map so the catalog is immutable after construction.
func TestNewCopiesProfiles(t *testing.T) {
	in := map[Role]Profile{RolePlanner: {System: "orig", ReadOnly: true}}
	r := New(in)
	in[RolePlanner] = Profile{System: "mutated"}
	if p, _ := r.Resolve(RolePlanner); p.System != "orig" {
		t.Errorf("roster reflected post-construction mutation: %q", p.System)
	}
}

// containsExact reports whether xs contains s verbatim.
func containsExact(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
