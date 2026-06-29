# Self-authored acceptance verifiers (`selfacc`) — capabilities & risks

> **What this is:** the agent can PROPOSE how its own work should be checked, and an
> operator can promote a proposed check into a real, sandboxed, fail-closed verifier
> that the existing verification spine then enforces. The agent writes the contract;
> the operator decides which clauses become binding; the verifier of record still rules.
>
> **Status:** SHIPPED and wired, **default-off**. Nothing changes until an operator opts
> in with `NILCORE_SELFACC=1` and names an approved-candidates file. Read §4 (risks)
> before you turn it on.

## 1. The idea (and why it's gated)

NilCore's first invariant of "done" is I2: *the verifier is the sole authority.* Normally
the operator (or a domain pack) defines the bar. But an under-specified goal often
arrives with **no acceptance pack** — and hand-writing every criterion doesn't scale.

`selfacc` lets the agent do the contract-first half itself: given a goal (and any plan it
produced), it **proposes** machine-checkable acceptance criteria, and can **author
candidate verifiers** — each just `{verifier_id, command, rationale}`, where `command` is
a single shell line run *inside the sandbox*.

The danger is obvious: an agent that writes its own test and then grades itself against it
has defeated the point of having a verifier. So the design draws a hard line:

- **Proposing is free. Binding is gated.** The agent can propose/author all day; none of
  it affects a verdict. A candidate becomes a real verifier **only** when an operator
  performs two explicit acts: set `NILCORE_SELFACC=1` *and* place the candidate in the
  file named by `NILCORE_SELFACC_FILE`. Those two acts **are** the human gate.
- **A self-check can only RAISE the bar, never lower it.** It runs *after* the build
  verifier and only resolves claims that *name* it; a self-check resolves a claim to
  `Pass` solely on an affirmative sandbox **exit 0**, and to `Unverifiable` otherwise. It
  can never turn a red build green, and a missing/rejected check is `Unverifiable`, never
  a silent pass (fail-closed).
- **A self-check is always sandboxed (I4).** The meta-check (`Admit`) rejects any
  candidate that hints at host/in-process execution; the only execution path is
  `sandbox.Exec`. There is no host-side fallback — a nil box is `Unverifiable`.

## 2. How it works (the pieces)

| Piece | Where | Role |
|---|---|---|
| `Propose(goal, tree)` | `internal/verify/selfacc/selfacc.go` | derive contract-first criteria from a plan (pure, no model call) |
| `Candidate{VerifierID, Command, Rationale}` | `internal/verify/selfacc/candidate.go` | an UNTRUSTED, model-authored verifier proposal (sandbox command only) |
| `Admit(c)` — the meta-check | `candidate.go` | the I4 gate: admits only a bounded sandbox command; rejects empty/over-long/control-byte/host-marker |
| `CheckFunc(c)` / `Register(reg, c)` | `candidate.go` / `resolve.go` | turn an *admitted* candidate into an evverify check; bind it (re-admits, defense-in-depth) |
| **`registerSelfAcceptance(reg)`** — the wiring | `cmd/nilcore/selfacc.go` | the operator-gated layer: opt-in + approved file ⇒ admit each ⇒ bind the admissible into the run's `evverify.Registry` |
| `nilcore selfacc propose\|check` | `cmd/nilcore/selfacc.go` | operator review surface (see proposed criteria; meta-check a file before trusting it) |

The wiring hangs off `evidenceRegistry()` (the same builder that adds domain packs), so a
bound self-check participates in the **identical** verified-artifact spine as every other
evidence check (`NILCORE_EVIDENCE_VERIFY`): schema shape-gate → per-claim in-box
verification → any red claim reddens the whole verdict.

## 3. Operator workflow

```sh
# 1. See what the agent would assert for a goal (read-only, inert):
nilcore selfacc propose -goal "ship a working /health endpoint" -plan plan.json

# 2. Author an approved file (you review every command — these run in the box):
cat > approved.json <<'JSON'
{ "candidates": [
  { "verifier_id": "candidate.health_ok",
    "command": "curl -fsS localhost:8080/health | grep -q ok",
    "rationale": "the endpoint returns ok" }
] }
JSON

# 3. Meta-check it BEFORE trusting it (non-zero exit if any candidate is un-admissible):
nilcore selfacc check -file approved.json

# 4. Turn it on for a verified run (both signals required):
NILCORE_EVIDENCE_VERIFY=1 NILCORE_SELFACC=1 NILCORE_SELFACC_FILE=$PWD/approved.json \
  nilcore run -dir ./repo -goal "..."
```

A claim whose `verifier` field names `candidate.health_ok` now resolves through that
sandboxed check. Any candidate the meta-check rejects is **skipped with a stderr note**
(never bound). A malformed/unreadable approved file is a **hard error that reddens the
evidence verdict** — like a bad pack list, it never silently falls back.

## 4. Risks — read before enabling

1. **You are trusting your own approved file, not the agent.** The whole safety model
   rests on the approved file being *operator-curated*. Keep it at a stable,
   operator-controlled path **outside the disposable worktree** (an absolute path), so a
   run can never write or edit its own approval. Never point `NILCORE_SELFACC_FILE` at a
   path the model can write.
2. **A weak self-check is a weak bar.** The sandbox + fail-closed rules stop a check from
   *cheating* (it can't run host code, can't pass without exit 0), but they cannot make a
   *trivial* command meaningful. If you approve `command: "true"`, you approved a check
   that asserts nothing. The agent can propose junk; **you** decide what binds — review
   each command like a test you're committing.
3. **It raises the bar, so a bad check can block good work.** A self-check can only add a
   criterion that must *also* pass. A flaky or wrong command therefore reddens otherwise-
   good runs. That's the safe direction (fail-closed), but it's still a footgun — meta-
   check and dry-run candidates before trusting them.
4. **Command injection is structurally blocked, not semantically.** `Admit` rejects NUL/
   control bytes, over-long commands, and host markers, and the command is data (never
   templated from claim text). But it is still a shell line you approved — apply the same
   scrutiny you would to any script that runs in your build box.
5. **Sandbox egress still applies.** A check that needs the network only reaches it
   through the box's egress policy (default-deny). A check assuming open egress will be
   `Unverifiable`, not silently skipped.

## 5. What it does **not** do

- It does **not** let the agent mark its own work done. Self-reports never decide a
  verdict (I2); only an admitted, operator-approved, sandboxed check can, and only by
  *adding* a criterion.
- It does **not** auto-register anything. There is no path by which a proposed candidate
  binds without the two explicit operator opt-ins.
- It does **not** run on the host or in-process — ever (I4).
- It is **not** on by default and is byte-identical when off.

## 6. Bottom line

`selfacc` closes the loop on *contract authorship* — the agent can raise its own bar —
while keeping every safety property that makes the bar trustworthy: the operator gates
what binds, the check is sandboxed and fail-closed, and the verifier of record still
rules. Turn it on when you want the agent to propose acceptance criteria you'll review;
keep the approved file under your control, and treat every approved command as a test you
own.
