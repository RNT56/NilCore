package roster

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/eventlog"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
)

// stubPeer satisfies backend.Peer (Tools + Dispatch) without a real bus, so a
// worker-construction test can pass a non-nil peer and assert the sandbox is never
// traded away for it. It is the smallest valid Peer; it exercises no bus traffic.
type stubPeer struct{}

func (stubPeer) Tools() []model.Tool { return nil }
func (stubPeer) Dispatch(context.Context, string, json.RawMessage) (string, error) {
	return "", nil
}

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

// A peer-equipped worker (the multi-agent spawn shape: a subagent that can talk to
// the supervisor on the bus) must STILL be sandboxed — wiring a bus peer never
// trades away the Box. This guards the adversary R1 path for the on-bus worker
// specifically: there is no constructor that yields a Native with a nil Box, peer
// or no peer. The peer surface is the bus tools only (asymmetry lives in the peer,
// tested in internal/agent/bus); here we assert the sandbox is never dropped.
func TestPeerWorkerStillSandboxed(t *testing.T) {
	r := defaultRoster()
	for _, role := range r.Roles() {
		p, _ := r.Resolve(role)
		w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, stubPeer{})
		if w.Box == nil {
			t.Errorf("role %q: a peer-equipped worker has a nil Box — R1 regression", role)
		}
		if w.Peer == nil {
			t.Errorf("role %q: the peer was dropped during construction", role)
		}
		if w.CommandGuard == nil {
			t.Errorf("role %q: a peer-equipped worker has a nil CommandGuard", role)
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
	// The original five roles must still be present and unchanged (snapshot); the
	// typed-research role (P11-T15) is an additive sixth.
	five := []Role{RoleResearcher, RoleUnderstander, RolePlanner, RoleImplementer, RoleReviewer}
	for _, role := range five {
		if _, ok := r.Resolve(role); !ok {
			t.Errorf("default roster missing role %q", role)
		}
	}
	want := append(append([]Role{}, five...), RoleTypedResearch)
	if got := len(r.Roles()); got != len(want) {
		t.Errorf("default roster has %d roles, want %d", got, len(want))
	}
	for _, role := range want {
		if _, ok := r.Resolve(role); !ok {
			t.Errorf("default roster missing role %q", role)
		}
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

// --- CV-T04: read-only roles get real tools, still write-free ---------------

// The understander's registry carries the codeintel tool on top of read/search,
// and codeintel returns a real bundle over a temp repo (hermetic, host-side).
func TestUnderstanderHasCodeintelTool(t *testing.T) {
	r := defaultRoster()
	p, ok := r.Resolve(RoleUnderstander)
	if !ok {
		t.Fatal("understander not resolvable")
	}
	w := NewWorker(p, fakeBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	if !w.Tools.Has("codeintel") {
		t.Fatal("understander registry is missing the codeintel tool")
	}
	for _, rt := range []string{"read", "search"} {
		if !w.Tools.Has(rt) {
			t.Errorf("understander lost base read tool %q", rt)
		}
	}

	// The tool returns a structurally-coherent bundle over a temp repo.
	dir := t.TempDir()
	src := "package p\nfunc helper() {}\nfunc Run() { helper() }\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := w.Tools.Dispatch(context.Background(), "codeintel", dir, json.RawMessage(`{"query":"Run"}`))
	if err != nil {
		t.Fatalf("codeintel dispatch: %v", err)
	}
	if !strings.Contains(out, "Run") || !strings.Contains(out, "helper") {
		t.Errorf("bundle should include Run and its neighbor helper:\n%s", out)
	}
}

// The researcher's worker carries the sandboxed web_fetch tool, bound to ITS box —
// and dispatching it runs through that box (never a host-side fetch, I4).
func TestResearcherHasSandboxedWebFetch(t *testing.T) {
	r := defaultRoster()
	p, ok := r.Resolve(RoleResearcher)
	if !ok {
		t.Fatal("researcher not resolvable")
	}
	box := &recordingBox{}
	w := NewWorker(p, box, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	if !w.Tools.Has("web_fetch") {
		t.Fatal("researcher registry is missing the web_fetch tool")
	}

	out, err := w.Tools.Dispatch(context.Background(), "web_fetch", "/work", json.RawMessage(`{"url":"https://docs.example.com/x"}`))
	if err != nil {
		t.Fatalf("web_fetch dispatch: %v", err)
	}
	if box.lastCmd == "" {
		t.Fatal("web_fetch did not run through the worker's box (host-side bypass — I4 regression)")
	}
	if !strings.Contains(box.lastCmd, "https://docs.example.com/x") {
		t.Errorf("web_fetch did not fetch the requested URL in the box: %q", box.lastCmd)
	}
	if !strings.Contains(out, "UNTRUSTED DATA") {
		t.Errorf("web_fetch result was not fenced as untrusted (I7):\n%s", out)
	}
}

// Only the researcher gets web_fetch — the other read-only roles (deny-all egress)
// must NOT carry it, and none of the read-only roles ever gains a write tool.
func TestWebFetchOnlyOnResearcherAndNoWriteTools(t *testing.T) {
	r := defaultRoster()
	for _, role := range r.Roles() {
		p, _ := r.Resolve(role)
		w := NewWorker(p, &recordingBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)

		// The researcher and the typed-research role (P11-T15) carry web_fetch; no
		// other role does.
		wantsFetch := role == RoleResearcher || role == RoleTypedResearch
		hasFetch := w.Tools.Has("web_fetch")
		if wantsFetch && !hasFetch {
			t.Errorf("role %q must carry web_fetch", role)
		}
		if !wantsFetch && hasFetch {
			t.Errorf("role %q must NOT carry web_fetch", role)
		}
		// The structural guarantee: read-only roles carry NO write/git-write tools,
		// even after the new codeintel/web_fetch tools were wired in.
		if role.ReadOnly() {
			for _, wt := range []string{"write", "edit", "git"} {
				if w.Tools.Has(wt) {
					t.Errorf("read-only role %q gained a write tool %q after CV-T04 wiring", role, wt)
				}
			}
		}
	}
}

// The shared profile registry is never mutated when a per-worker web_fetch is
// wired: two researcher workers get distinct registries, and the catalog's own
// registry never sees web_fetch (otherwise every worker would race on one box).
func TestWebFetchDoesNotMutateSharedRegistry(t *testing.T) {
	r := defaultRoster()
	p, _ := r.Resolve(RoleResearcher)
	// The static profile registry must NOT carry web_fetch (it is wired per-worker).
	if p.Tools.Has("web_fetch") {
		t.Error("the catalog profile registry leaked a web_fetch tool (should be per-worker)")
	}
	w1 := NewWorker(p, &recordingBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	w2 := NewWorker(p, &recordingBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	if w1.Tools == w2.Tools {
		t.Error("two researcher workers share one registry — the per-worker clone is broken")
	}
	// And the shared profile registry STILL has no web_fetch after constructing
	// workers (in-place mutation would have leaked it onto the catalog).
	if p.Tools.Has("web_fetch") {
		t.Error("constructing a worker mutated the shared profile registry")
	}
}

// --- P11-T15: the typed-research role (write-capable, evidence-verified) -----

// The typed-research role is resolvable, write-capable (NOT read-only), carries the
// write+edit tools, names the fixed artifact path + spine shape in its prompt, and
// narrows to --network none under a deny-all tree. The original five roles stay
// unchanged. (P11-T15 acceptance.)
func TestTypedResearchProfile(t *testing.T) {
	r := defaultRoster()

	p, ok := r.Resolve(RoleTypedResearch)
	if !ok {
		t.Fatal("typed-research role is not resolvable")
	}

	// Write-capable: NOT read-only, and the role/profile ReadOnly agree.
	if p.ReadOnly {
		t.Error("typed-research profile must NOT be ReadOnly (it writes the artifact)")
	}
	if RoleTypedResearch.ReadOnly() {
		t.Error("RoleTypedResearch.ReadOnly() must be false (write-capable)")
	}

	// hasWriteTool true: the worker's registry carries write+edit (and git).
	w := NewWorker(p, &recordingBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
	if !hasWriteTool(w.Tools) {
		t.Error("typed-research worker must advertise write tools (hasWriteTool false)")
	}
	for _, wt := range []string{"write", "edit"} {
		if !w.Tools.Has(wt) {
			t.Errorf("typed-research worker missing write tool %q", wt)
		}
	}

	// System prompt names the fixed artifact path and the spine Claim/Evidence shape.
	for _, sub := range []string{".nilcore/artifacts/<id>.json", "Claim", "Evidence", "source_url", "status"} {
		if !strings.Contains(p.System, sub) {
			t.Errorf("typed-research System prompt missing %q", sub)
		}
	}
	// The prompt path must equal the documented shared constant.
	if !strings.Contains(p.System, ArtifactRelPath) {
		t.Errorf("typed-research System prompt path does not match ArtifactRelPath %q", ArtifactRelPath)
	}

	// EgressFor under a deny-all tree => empty allowlist (--network none); narrow-only.
	if eg := EgressFor(p, policy.Egress{}); !eg.Empty() {
		t.Errorf("typed-research under deny-all tree got network %v (must be --network none)", eg.Allowed)
	}

	// The original five roles are still present and unchanged in shape.
	for _, role := range []Role{RoleResearcher, RoleUnderstander, RolePlanner, RoleImplementer, RoleReviewer} {
		if _, ok := r.Resolve(role); !ok {
			t.Errorf("original role %q vanished after adding typed-research", role)
		}
	}
}

// recordingBox is a hermetic sandbox.Sandbox that records the last command it ran,
// so a roster test can prove web_fetch dispatched THROUGH the worker's box.
type recordingBox struct{ lastCmd string }

func (b *recordingBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.lastCmd = cmd
	return sandbox.Result{Stdout: "<html>ok</html>", ExitCode: 0}, nil
}
func (b *recordingBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *recordingBox) Workdir() string { return "/work" }

// containsExact reports whether xs contains s verbatim.
func containsExact(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// TestReadOnlyHelperAgreesWithWriteProfiles is the B4-swarm.5 guard: Role.ReadOnly()
// must agree with the Profile's ReadOnly field for the write-capable preset roles
// (auditor, ui) as well as the pre-existing write roles (implementer, typed-research).
// Before the fix the helper reported auditor/ui as read-only (a latent footgun: a caller
// trusting the helper would mis-wire a read-only worker that cannot emit its artifact).
// Profile.ReadOnly stays the structural source of truth NewWorker reads; this asserts the
// convenience helper no longer DISAGREES with it for any write role.
func TestReadOnlyHelperAgreesWithWriteProfiles(t *testing.T) {
	cases := []struct {
		role    Role
		profile Profile
	}{
		{RoleAuditor, AuditorProfile(fakeProvider{"exec"}, policy.Egress{})},
		{RoleUI, UIProfile(fakeProvider{"exec"}, policy.Egress{})},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			if tc.profile.ReadOnly {
				t.Fatalf("%s profile must be write-capable (ReadOnly:false)", tc.role)
			}
			if tc.role.ReadOnly() {
				t.Errorf("%s.ReadOnly() = true, want false — helper disagrees with the write Profile (footgun)", tc.role)
			}
			// And the structural wiring honors it: the worker carries write tools.
			w := NewWorker(tc.profile, &recordingBox{}, fakeVerifier{}, &eventlog.Log{}, fakeProvider{"exec"}, nil)
			if !hasWriteTool(w.Tools) {
				t.Errorf("%s worker must advertise write tools (it emits a spine artifact)", tc.role)
			}
		})
	}
	// Belt-and-suspenders: the four write roles are the ONLY roles the helper reports
	// writable, and every default-roster role still agrees with its Profile.
	writable := map[Role]bool{RoleImplementer: true, RoleTypedResearch: true, RoleAuditor: true, RoleUI: true}
	for _, role := range []Role{RoleResearcher, RoleUnderstander, RolePlanner, RoleImplementer, RoleReviewer, RoleTypedResearch, RoleAuditor, RoleUI} {
		if got, want := role.ReadOnly(), !writable[role]; got != want {
			t.Errorf("%s.ReadOnly() = %v, want %v", role, got, want)
		}
	}
}
