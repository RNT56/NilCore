// Package roster is the role system for the multi-agent supervisor
// (docs/MULTI-AGENT.md §2). A role is a *configuration over the one
// backend.Native loop* — not a new code path — across four axes: system prompt,
// tool set, model tier, and egress + command policy. The package exports the
// catalog (Role constants, Profile, Roster.Resolve) and the single safe
// constructor NewWorker (worker.go), which is the ONLY way to build a subagent.
//
// Capability is a property of *wiring*, never of prompt obedience (I7): a
// read-only role is handed a tools.Registry without write/git-write tools and a
// tightened policy.CommandPolicy that denies in-tree writes — so even a
// compromised model cannot mutate the tree, because the tools to do so are not
// registered. Per-role egress only ever *narrows* the tree's allowlist
// (intersect, never a superset), and deny-all roles get an empty allowlist that
// the sandbox renders as `--network none`. All of this is enforced structurally
// by NewWorker, not by trusting the role to behave.
package roster

import (
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/tools"
)

// Role names the five subagent specializations (docs/MULTI-AGENT.md §2). Three
// of the five wrap behaviors that already exist (planner→planner.Plan,
// reviewer→route.Review, implementer→the native worker); researcher and
// understander are the only new read-only wiring.
type Role string

const (
	// RoleResearcher does web/doc research. Read-only; egress is a research
	// allowlist (the only non-implementer role permitted any network).
	RoleResearcher Role = "researcher"
	// RoleUnderstander maps an existing repo. Read-only; deny-all egress.
	RoleUnderstander Role = "understander"
	// RolePlanner builds a contract-first task tree. Read-only; deny-all egress.
	RolePlanner Role = "planner"
	// RoleImplementer writes code in an isolated worktree. Full write tools;
	// registries-only egress (DefaultEgress).
	RoleImplementer Role = "implementer"
	// RoleReviewer cross-model reviews a diff. Read-only; deny-all egress.
	RoleReviewer Role = "reviewer"
	// RoleTypedResearch does evidence-verified research: it investigates with web
	// access AND writes a spine Artifact JSON (claims + provenance) to the worktree
	// for the ArtifactVerifier to assert over (Phase 11, P11-T15). Unlike
	// RoleResearcher it is WRITE-capable — it must emit the artifact file via the
	// write/edit tools — so it is NOT a read-only role; the supervisor merges only
	// the verifier-set claim statuses, never the worker's prose self-report (I2/I7).
	RoleTypedResearch Role = "typed-research"
)

// ArtifactRelPath is the fixed, out-of-band worktree location the typed-research
// role writes its spine Artifact to. It MUST equal the artifact spine's fixed
// RelPath (`internal/artifact` persists to `.nilcore/artifacts/<id>.json`); the
// `<id>.json` suffix is appended by the worker per artifact. This constant is the
// documented shared anchor named in the role's System prompt so the worker and the
// verifier agree on one path without roster importing the artifact leaf.
const ArtifactRelPath = ".nilcore/artifacts/<id>.json"

// Profile is the per-role configuration NewWorker turns into a sandboxed worker.
// Every field is a capability lever; none is a suggestion the model may ignore.
//
//   - System is the role system prompt (intent, not capability).
//   - Tools is the registry the worker advertises. Read-only roles get a
//     registry WITHOUT write/git-write tools — the structural read-only guarantee.
//   - Model is the role's advisor tier (strong for planner/reviewer, executor
//     for the rest); the executor provider is passed to NewWorker separately. It
//     is expected to be already metered (meter.Provider, §7).
//   - Command tightens policy.CommandPolicy.Check for read-only roles (deny
//     in-tree writes, git push/commit, package installs) on top of the default.
//   - Egress is the role's network allowlist BEFORE intersection with the tree;
//     NewWorker intersects it with the tree egress so it can only narrow.
//   - ReadOnly marks the structural read-only roles (an assertion NewWorker
//     enforces by handing them a write-free registry).
//   - MaxSteps is the per-worker tool-call ceiling (a termination rail, §6).
//   - WantsWebFetch wires the sandboxed web_fetch tool (tools.WebFetchTool) into
//     this role's registry at NewWorker time. The fetch runs INSIDE the worker's
//     box under the role's egress (never a host-side fetch, I4), so the tool must
//     be bound to the box — which does not exist when the static catalog is built.
//     Only the researcher sets it (the only read-only role with research egress).
//     The tool is read-only/non-mutating, so the write-free guarantee still holds.
type Profile struct {
	System        string
	Tools         *tools.Registry
	Model         model.Provider
	Command       func(string) (allowed bool, reason string)
	Egress        policy.Egress
	ReadOnly      bool
	MaxSteps      int
	WantsWebFetch bool
}

// Roster is the immutable catalog of role profiles. Build it with New (or
// NewDefault for the standard five) and resolve by role.
type Roster struct {
	profiles map[Role]Profile
}

// New builds a roster from an explicit profile set. The map is copied so the
// caller cannot mutate the catalog after construction.
func New(profiles map[Role]Profile) *Roster {
	cp := make(map[Role]Profile, len(profiles))
	for r, p := range profiles {
		cp[r] = p
	}
	return &Roster{profiles: cp}
}

// Resolve returns the profile for role, and false if the role is not in the
// catalog. The caller must check ok before building a worker — there is no
// silent fallback to a more-privileged role.
func (r *Roster) Resolve(role Role) (Profile, bool) {
	if r == nil {
		return Profile{}, false
	}
	p, ok := r.profiles[role]
	return p, ok
}

// Roles lists the roles in the catalog (unordered) — for enumeration in tests
// and wiring. It never exposes the underlying map.
func (r *Roster) Roles() []Role {
	if r == nil {
		return nil
	}
	out := make([]Role, 0, len(r.profiles))
	for role := range r.profiles {
		out = append(out, role)
	}
	return out
}

// ReadOnly reports whether role is a structural read-only role (no write tools).
// The two write-capable roles are the implementer (writes code) and typed-research
// (writes a spine Artifact JSON, P11-T15); every other role is read-only.
func (role Role) ReadOnly() bool {
	return role != RoleImplementer && role != RoleTypedResearch
}

// readToolset returns the read-only structured tool set: read + search ONLY. The
// GitTool is deliberately excluded even though it offers status/diff/log, because
// the same tool also does add/commit (a git-write surface), and read-only means
// no path to mutate the tree — capability is structural, not prompt-gated. A
// read-only role inspects history through the sandboxed `run` shell (git status,
// git diff, git log), which the tightened CommandPolicy still permits.
//
// The set itself now lives in internal/tools (tools.ReadOnly), shared with the
// conversational front door's read-only modes; this wrapper keeps the role names
// stable.
func readToolset() *tools.Registry {
	return tools.ReadOnly()
}

// understanderToolset is the read-only set for the code-understanding role: the
// read/search pair PLUS the codeintel tool (CV-T04). codeintel is a host-side,
// read-only adapter over internal/codeintel — it parses the worktree into an
// ephemeral in-memory graph and returns a structurally-coherent context bundle,
// with no write, no execution, and no network. It is NOT a write/edit/git tool, so
// adding it keeps the role's write-free structural guarantee intact (NewWorker's
// hasWriteTool check still passes). The web_fetch tool is deliberately absent here:
// the understander has deny-all egress, and codeintel is purely local.
//
// The set itself now lives in internal/tools (tools.ReadOnlyWithCodeintel), shared
// with the chat front door's Plan/Discuss modes.
func understanderToolset() *tools.Registry {
	return tools.ReadOnlyWithCodeintel()
}

// writeToolset is the implementer's full set: the standard read/write/edit/
// search/git registry (tools.Default).
func writeToolset() *tools.Registry {
	return tools.Default()
}

// readOnlyCommandPolicy tightens the default command denylist for read-only
// roles. The list itself now lives in internal/policy (policy.ReadOnlyCommandPolicy),
// shared with the conversational front door's read-only modes; this wrapper keeps
// the role-wiring call site stable. See policy.ReadOnlyCommandPolicy for the
// rationale (the command-plane mirror of a write-free registry — defense in depth,
// never the only write boundary).
func readOnlyCommandPolicy() policy.CommandPolicy {
	return policy.ReadOnlyCommandPolicy()
}

// intersectEgress returns the allowlist a role actually gets: only hosts BOTH
// the role and the tree permit. It is a narrowing operation by construction — the
// result can never allow a host the tree denies (no superset escape, R9). A
// deny-all role (empty role allowlist) yields an empty result regardless of the
// tree, which the sandbox renders as `--network none`.
//
// Wildcard handling is intentionally conservative: an entry survives only if it
// is also covered by the other side. A literal host survives if the other side
// allows that exact host (literal match) or a wildcard suffix covering it; a
// wildcard entry survives only if the other side carries the identical wildcard
// (we never widen a wildcard against a narrower peer). When in doubt we drop the
// entry — narrowing is always the safe direction.
func intersectEgress(role, tree policy.Egress) policy.Egress {
	// A deny-all on either side is deny-all for the worker.
	if role.Empty() || tree.Empty() {
		return policy.Egress{}
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range role.Allowed {
		if covers(tree, e) && !seen[e] {
			out = append(out, e)
			seen[e] = true
		}
	}
	return policy.Egress{Allowed: out}
}

// covers reports whether allowlist e permits everything entry would permit, i.e.
// whether keeping entry under e never widens beyond e. A literal entry is covered
// if e.Allow(entry) is true (literal or wildcard match). A wildcard entry is
// covered only by the identical wildcard in e — covering a wildcard with a
// narrower literal would widen it, which we refuse.
func covers(e policy.Egress, entry string) bool {
	if isWildcard(entry) {
		for _, pat := range e.Allowed {
			if pat == entry {
				return true
			}
		}
		return false
	}
	return e.Allow(entry)
}

func isWildcard(s string) bool { return len(s) > 1 && s[0] == '*' && s[1] == '.' }
