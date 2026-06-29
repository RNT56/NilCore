# Self-authored acceptance — the closed loop (`selfacc`)

> **What this is:** during a run, once the project's own verifier passes, the agent's
> OWN acceptance checks — authored for *this* goal — must ALSO pass before the work is
> judged done. The agent raises its own bar; each proposed check is gated (and the gate
> amortizes over time); the verifier of record still rules. The loop closes:
> **propose → author → gate → bind → run → fold into the verdict → earn trust.**
>
> **Status:** SHIPPED, **default-off** (`NILCORE_SELFACC`). Read §5 (risks) before enabling.

## 1. Why this is novel (and not just "relocated work")

The static v1 had a real flaw: it asked the operator to pre-author a file of checks, which
mostly *moved* the work rather than removing it. The deepened version fixes that on two axes:

1. **Runtime, in-the-loop.** The agent derives acceptance checks for the actual goal *during
   the run* and is then held to them by the same verdict that gates the commit — not a static
   config divorced from the work.
2. **The gate amortizes.** A self-check is gated like any boundary action. The FIRST times a
   given check class appears, a human approves it (or a headless run denies it). Once it has
   **earned trust** — verifier-green N times within the operator envelope — graduated
   auto-approval (`graapprove`) admits it without asking again. Review is paid **once per
   check-pattern**, then reused. That is the answer to "doesn't the gate just relocate work":
   it does the first few times, by design, and then it doesn't.

The result is genuinely new: **self-authored, runtime, verified, gated, trust-amortized
acceptance** — most valuable exactly where there's no human to write or review checks per run
(the autonomy daemon, swarm), because a proven self-check class auto-approves within the fence.

## 2. The loop (where each piece lives)

| Step | Where | What happens |
|---|---|---|
| **floor** | `agent.executeSingle` → `env.Verifier.Check` | the project's verifier runs first and **governs** (I2). Self-acceptance is consulted ONLY if it's green. |
| **propose+author** | `cmd/nilcore/selfacc.go` `authorSelfAcceptCandidates` | one bounded model call authors up to `N` sandbox-command checks for the goal (untrusted data — I7). Operator pre-approved checks (`NILCORE_SELFACC_FILE`) are added too. |
| **admit** | `selfacc.Admit` (leaf) | the I4 meta-check: sandbox command only; un-admissible ⇒ skipped, never bound. |
| **gate** | `policy.GateStructured(BindSelfAuthored)` via `orch.Approver` | each AGENT check is approved (attended human / headless deny / `graapprove` auto-approve on earned trust). Operator-file checks are pre-approved and skip the gate. |
| **bind+run** | `selfacc.CheckFunc` → `box.Exec` | each approved check runs INSIDE the worktree box; exit 0 = holds, else not. |
| **fold** | `agent.executeSingle` | the run is done only if EVERY bound check passed; a failure reddens the verdict. It can only **ADD** to the bar. |
| **audit+trust** | `eventlog` + `emitBoundaryOutcome` | `selfacc_propose/gate/bind/skip` events (I5); each gated check's pass/fail emits a `boundary_outcome` so the class earns (or loses) trust. |

The hook is an opt-in `agent.SelfAcceptFunc` on the orchestrator. **nil ⇒ byte-identical.**

## 3. The invariants (why it's safe)

- **I2 — the verifier of record still rules.** The floor verifier runs first and a red floor
  is never consulted for self-acceptance (the hook is only called when `rep.Passed`). A
  self-check can therefore only ever **raise** the bar — it cannot turn a red build green. A
  bound check passes a criterion only on an affirmative sandbox **exit 0**; anything else
  (non-zero, sandbox error, no box) is *not a pass*.
- **I3 — no ambient authority.** Candidates are inert data; nothing secret is read here or
  handed to a check beyond what the sandbox already exposes.
- **I4 — sandboxed.** `Admit` rejects any host/in-process marker; every check runs only via
  `box.Exec`. There is no host-side fallback.
- **I5 — append-only audit.** Every propose/gate/bind/skip + the trust `boundary_outcome` is a
  metadata-only appended event.
- **I7 — data, not instructions.** The goal and every authored field (id/command/rationale)
  are carried as data, validated structurally, never interpreted.
- **the gate.** `BindSelfAuthored` is `Irreversible`; headless deny-defaults; auto-approval is
  earned-trust + operator-envelope + blast-fenced only (`graapprove` presets `standard`
  MinSuccesses 15, `trusted` 25 — a *higher* bar than open-pr, since the agent is judging
  itself). `prod`/`main`/`release` are structurally denied as for every class.

## 4. Operator workflow

```sh
# Attended run: the agent proposes checks for the goal; you approve/deny each at the gate,
# and approved checks must pass for the run to be judged done.
NILCORE_SELFACC=1 nilcore run -dir ./repo -goal "ship a /health endpoint that returns ok"

# Cap how many checks the agent may author per run (default 5):
NILCORE_SELFACC=1 NILCORE_SELFACC_MAX=3 nilcore run ...

# Pre-approve a stable check set you authored (skips the gate — the file IS your approval):
NILCORE_SELFACC=1 NILCORE_SELFACC_FILE=$PWD/approved.json nilcore run ...

# Amortized auto-approval for unattended autonomy (a proven (id+command) self-check
# auto-approves within the envelope; first appearances + any command change still gate).
# Unattended self-acceptance REQUIRES an operator auto-approval envelope with a
# `bind-self-authored` clause — without one, a headless run can approve nothing and the
# feature self-disables. Simplest: a named preset (the `standard`/`trusted` presets both
# include the bind-self-authored clause):
NILCORE_AUTOAPPROVE_PRESET=standard NILCORE_SELFACC=1 nilcore serve   # + NILCORE_AUTONOMY
# Or hand-author the envelope in your nilcore config (an explicit `auto_approve` block
# always wins over the preset):
#   { "auto_approve": { "classes": [
#       { "type": "bind-self-authored", "allow_branches": ["*"],
#         "deny_branches": ["main","master","release/*","prod*"],
#         "min_successes": 15, "min_sample": 15, "recency_days": 7, "max_per_day": 3 } ] } }

# Review aids (read-only — run nothing, change no verdict):
nilcore selfacc propose -goal "..." -plan plan.json   # see the contract-first criteria
nilcore selfacc check   -file approved.json           # meta-check a candidate file
```

`approved.json` shape: `{"candidates":[{"verifier_id":"candidate.health_ok","command":"curl -fsS localhost:8080/health | grep -q ok","rationale":"the endpoint returns ok"}]}`.

## 5. Risks — read before enabling

1. **A weak check is a weak bar.** The sandbox + fail-closed rules stop a check from *cheating*
   (no host code, no pass without exit 0), but cannot make a *trivial* check meaningful. If
   you approve `command:"true"`, you approved a check that asserts nothing. Review each
   command like a test you're committing — and remember that approving it repeatedly is what
   teaches the system to auto-approve it.
2. **Earned-trust amortization cuts both ways.** Auto-approval means a check class you approved
   N times now binds without asking. That's the point — but it's why the bar is high
   (15/25 successes) and scoped to the **(verifier id + command)** pair — change the command
   and it re-gates, so an earned id can never auto-approve a different command. A self-check still only ever ADDS to
   the bar, so an auto-approved weak check is at worst a no-op; the danger it cannot create is
   a *lowered* bar.
3. **It raises the bar, so a flaky/wrong check blocks good work.** A self-check that's wrong in
   the failing direction reddens otherwise-good runs. Safe (fail-closed), still a footgun —
   `nilcore selfacc check` and dry-run before trusting.
4. **The operator file must stay yours.** Keep `NILCORE_SELFACC_FILE` at an absolute path
   *outside* the disposable worktree. (Enforced: a file that resolves inside the worktree is
   refused fail-closed, since a run could have written it — but keep it elsewhere anyway.) A
   malformed/unreadable file you DID set reddens the run rather than silently dropping your bar.
5. **Authoring is a model call.** Enabling `selfacc` adds one bounded model call per
   *green* run (skipped on red runs and when off). It reads only the goal; it never sees secrets.

## 6. What it does NOT do

- It does **not** let the agent mark its own work done — the floor verifier governs; a
  self-check only adds a criterion that must *also* pass.
- It does **not** auto-bind un-gated agent checks — every agent check is gated; only earned
  trust (your repeated approvals) lets a class auto-approve later.
- It does **not** run on the host or in-process — ever (I4).
- It is **not** on by default and is byte-identical when off.
- It currently applies to the **single-run** path. A run whose floor failed and was
  recovered by a best-of-N race, and the supervised/decompose path, do not (yet) run the
  extra self-acceptance bar — the floor verifier still governs them; self-acceptance only
  ever *adds*, so skipping it there is safe, just not yet covered.

## 7. Bottom line

The agent can now raise its own acceptance bar at run time, you decide which of its checks to
trust, that trust amortizes so you're not asked twice for the same proven check, and the
verifier of record still rules. That's the version with real value — especially for unattended
autonomy, where self-defined acceptance behind an earned-trust fence is something no static
config can give you.
