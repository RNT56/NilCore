# EXT-01 — Managed cloud agent fleet (GATED execution plan)

> **STATUS: BLOCKED behind the §0 gate.** This is the *ready-when-the-gate-clears* blueprint for
> `docs/ROADMAP-EXTERNAL-INFRA.md` §2. Not a single `EXT-01-T##` task below is eligible work under
> `CLAUDE.md` §5 until a **human owner records the thesis decision** (§0 of the roadmap,
> `docs/ROADMAP-EXTERNAL-INFRA.md:11-21`) **and** a security review signs off the credential-proxy
> design. Reading order: `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/ROADMAP-EXTERNAL-INFRA.md`
> (§0 + §2) → `docs/SWARM.md` (the single-host projection this extends) → this file. If anything
> here conflicts with `CLAUDE.md`, `CLAUDE.md` wins and this plan stays unbuilt.

---

## Table of contents

- [§-1 Summary](#-1-summary)
- [§0 Gate-clearance criteria (EXT-01-specific)](#0-gate-clearance-criteria-ext-01-specific)
- [§1 Where this sits: the line `docs/SWARM.md` refuses to cross](#1-where-this-sits)
- [§2 Architecture](#2-architecture)
- [§3 The task DAG](#3-the-task-dag)
- [§4 Per-task specs](#4-per-task-specs)
- [§5 Wave map, critical path, serialization points](#5-wave-map-critical-path-serialization-points)
- [§6 Per-invariant ledger (I1–I7 + never-land)](#6-per-invariant-ledger)
- [§7 Module justifications](#7-module-justifications)
- [§8 Default-off byte-identical proof](#8-default-off-byte-identical-proof)
- [§9 Risks](#9-risks)

---

## §-1 Summary

EXT-01 is a **leasing control plane** that takes the verified swarm (`docs/SWARM.md`, single-host,
in-process) and projects one bounded pool across **many isolated remote workers** — provisioning a
fresh VM/microVM per shard, cloning the repo, running the agent loop unattended, persisting shard
state cross-host so a closed laptop never loses work, and returning a **reviewable, GATED** PR at
high concurrency. It **extends** rather than bypasses every invariant: the remote unit of work stays
exactly `backend.CodingBackend.Run(ctx, Task) (Result, error)` (I1) inside the remote sandbox (I4);
a **control-plane credential proxy** mints a scoped, short-lived in-sandbox token that a
prompt-injected remote agent cannot trade for the real fleet credential (I3); and **every remote
worker's artifact re-runs the LOCAL project verifier** before anything is even a merge candidate
(I2) — the integrator still never lands to base, promotion is one gated
`policy.GateAction{PromoteToBase}` through the human approver, nil-approver-deny preserved
(`internal/policy/gateaction.go:94-101`). The whole plan is **default-off and byte-identical** with
the feature absent, and is **blocked behind the §0 gate** until a human records the thesis decision.

---

## §0 Gate-clearance criteria (EXT-01-specific)

The generic gate is `docs/ROADMAP-EXTERNAL-INFRA.md:11-21`. EXT-01 is the **clearest identity change
on the roadmap** (`ROADMAP-EXTERNAL-INFRA.md:50`), so its bar is the highest. **All** of the
following must be **recorded in the PR that promotes the first `EXT-01-T##` row into `docs/TASKS.md`**
(itself a serialized contract change), and the security review must be linked from it:

- **G1 — Recorded thesis decision (non-delegable).** A named human owner records that NilCore's
  identity may expand from "one self-hosted Go binary" (`docs/ARCHITECTURE.md:81`) toward operating
  (or self-hosting) a managed fleet. This is the exact class of irreversible, outward-facing decision
  the whole design reserves for a human — it is *not* delegable to the agent.
- **G2 — Credential-proxy security review signed off.** A security review explicitly approves the
  design in [§2.3](#23-the-credential-proxy): a remote, possibly prompt-injected worker holds only a
  **scoped lease token** (push restricted to its own working branch, read scoped to the one repo,
  TTL-bounded), the control plane brokers it to the real credential, and **the real credential never
  leaves the control-plane host, never enters a worker image layer, never reaches the model** (I3,
  `docs/ARCHITECTURE.md:120-121`). The review must show the threat model: a worker that fully
  complies with an injection still cannot exfiltrate the fleet credential, push outside its branch,
  or reach a second tenant's worktree.
- **G3 — Verifier-still-governs proof.** The plan must demonstrate, in code seams, that **no remote
  self-report decides "done"**: the worker returns a typed artifact + branch; the **local** project
  verifier (`evverify.ArtifactVerifier`, already `var _ verify.Verifier`, the per-shard I2 gate from
  Phase 11/12) re-runs against the **re-fetched** worktree before the work is a merge candidate; the
  Integrator re-verifies on every merge and **never lands to base**
  (`internal/integrate/integrate.go:12-17`); promotion is **one** gated
  `policy.GateAction{PromoteToBase}` (`internal/policy/gateaction.go:28-31, 94-101`).
- **G4 — Dependency budget justified, CGO-free.** Every standing-infra interaction is the **stdlib
  net/http JSON-RPC** posture (the MCP precedent, `CLAUDE.md` §2.6) — **no cloud SDK module is
  added** to the core. Provisioning is delegated to an **operator-configured external provisioner
  command** (the `secrets.ExternalStore` hook precedent, `internal/secrets/external.go:10-18`), so a
  vendor's API surface lives outside the binary. Any module that *cannot* be avoided is justified in
  **both** the PR and the CHANGELOG and must keep `CGO_ENABLED=0` (`.github/workflows/release.yml`).
  See [§7](#7-module-justifications).
- **G5 — Default-off, opt-in, reversible to remove.** The default `nilcore` binary stays
  **byte-identical** with EXT-01 absent (proof in [§8](#8-default-off-byte-identical-proof)); `fleet`
  is one new subcommand reachable only when explicitly invoked; no existing dispatch arm or shared
  helper changes behavior; no existing package imports a fleet leaf.
- **G6 — The single-host swarm is the fallback, not removed.** EXT-01 is layered **above** the
  shipped `swarm` (`docs/SWARM.md`); with the fleet config absent, `--driver local` is the exact
  shipped single-host swarm path. EXT-01 never replaces the in-process pool — it is an opt-in
  *additional* `Driver`.

If any of G1–G6 cannot be met, EXT-01 stays on the roadmap, unbuilt.

---

## §1 Where this sits

`docs/SWARM.md` is the **single-host projection of a fleet** (`SWARM.md:54-60, 450-456`): a bounded
in-process pool over `internal/scheduler` (`scheduler.go:71-84`) and `internal/spawn` (the wave-DAG),
with **local-process-restart** resume over the **local** single-writer SQLite store
(`store.go:53` — `SetMaxOpenConns(1)`), and **no standing authority**. It *names* the line it does
not cross — multi-host shard dispatch, cross-host task-state, a remote control plane — and pins it as
`EXT-01` (`SWARM.md:453-456, 739-742`). EXT-01 is exactly that crossing, and only that crossing:

| Today (single-host swarm, sourced) | The line EXT-01 crosses |
|---|---|
| Shards run in-process; `scheduler` worker pool caps concurrency on one host (`scheduler.go:71-84`) | A **remote worker per shard** on its own VM/microVM, leased by a control plane |
| Shard state persists to the **local** SQLite store (`store.go:53`); resume is local-process-restart (`durability.go:148-229`) | Shard state persists **cross-host**, read by the control plane to re-lease an orphaned shard to a new VM |
| Secrets are **per-host** (`secrets.go:30-35`); the model never sees a key (I3) | A **scoped lease token** is brokered to a remote worker; the real fleet credential stays control-plane-side (the credential proxy) |
| The verifier runs in the same process that produced the work (`evverify`, the per-shard I2 gate) | The worker runs remotely; the **local** verifier re-runs over the re-fetched artifact before merge |
| Promotion is one gated `policy.GateAction{PromoteToBase}` (`gateaction.go:28-31`) | **Unchanged** — promotion stays exactly one gated action through the human approver |

EXT-01 keeps the swarm's `Shard`→`spawn.Subtask`→`backend.Task` mapping verbatim; it changes only
**where** a shard runs and **how** its state survives a host loss. The unit of remote work is still
`backend.CodingBackend.Run` (`ROADMAP-EXTERNAL-INFRA.md:56-62`).

---

## §2 Architecture

### 2.0 Package map (all new leaves; import sets)

```
internal/fleet/            ← the leasing control plane (NEW leaf, whole package = one Owns unit)
    lease.go               Lease{ShardID,WorkerID,Token,TTL,Branch,State}, LeaseStore iface, expiry/renew/reclaim
    plane.go               ControlPlane{Provisioner,Store,Proxy,Verifier,Integrate} — leases shards to workers
    driver.go              Driver iface (the seam swarm.Runner targets); RemoteDriver impl; LocalDriver = shipped pool
    state.go               cross-host shard state: ShardRecord marshaled into store.Task.Detail (durable, RMW)
    reclaim.go             orphan detection: a lease whose TTL lapsed → shard re-queued to a fresh worker
    deps_test.go           guards: imports model/provider/secrets/scheduler/spawn/integrate/policy/store/
                           eventlog/backend/swarm — NEVER agent/super/project; net/http client only (no server)
  imports: net/http, crypto/*, encoding/json (stdlib) + internal/{scheduler,spawn,store,eventlog,
           policy,backend,swarm,model,provider,secrets,meter,budget,worktree}

internal/fleet/credproxy/  ← the credential proxy (NEW sub-leaf, distinct dir, parallel-safe)
    proxy.go               CredentialProxy: mint scoped lease token; broker token→real cred control-plane-side
    token.go               ScopedToken{Repo,Branch,Exp,Sig} — HMAC-signed, stdlib crypto/hmac+sha256
    broker.go              Broker iface: scoped-token → real push credential (the git-push proxy endpoint)
  imports: net/http, crypto/hmac, crypto/sha256, crypto/rand, encoding/json (stdlib) +
           internal/secrets (the real cred source), internal/eventlog (audit)

internal/fleet/provisioner/ ← VM/microVM provisioning behind an operator command (NEW sub-leaf)
    provisioner.go         Provisioner iface { Provision(ctx, Spec) (Worker, error); Reclaim(ctx, WorkerID) }
    external.go            ExternalProvisioner — shells an operator-configured command (the ExternalStore pattern)
    spec.go                WorkerSpec{Image,Region,EgressAllow,Resources} — DATA, never a model command
  imports: os/exec, encoding/json (stdlib) + internal/sandbox (the WorkerSpec → sandbox flags mapping)

internal/fleet/worker/     ← the remote-side agent entrypoint (NEW sub-leaf)
    worker.go              RemoteWorker.Run: clone (scoped token) → buildStack-equivalent in-box → backend.Run
                           → write artifact JSON → push branch (scoped token) → report ShardResult
  imports: internal/{backend,roster,sandbox,worktree,artifact,model,provider,eventlog} — the same
           seams buildBackend/NewWorker already use; runs INSIDE the remote VM

cmd/nilcore/fleet.go       ← buildFleet + `nilcore fleet` flag surface (NEW file)
cmd/nilcore/fleet-worker.go← the `nilcore fleet-worker` hidden subcommand (the remote VM entrypoint)
```

`onboard.Config` gains one additive optional `Fleet *fleet.FleetConfig` (`json:"fleet,omitempty"`),
mirroring how Phase-12 added `Pool *pool.PoolConfig` (`SWARM.md:482, 556-561`) — a serialized
config-schema contract task (`EXT-01-T11`).

### 2.1 The leasing control plane (`internal/fleet`)

The control plane **replaces the in-process scheduler loop with a leasing loop** while keeping the
exact `scheduler.Task{ID, Run}` unit (`scheduler.go:23-26`) and the swarm's `Shard` type. Today
`swarm.Runner.RunPass` maps `[]Shard` onto `scheduler.New(K)` or `spawn.DAGScheduler{MaxConcurrent:K}`
(`SWARM.md:584-589`). EXT-01 introduces a `fleet.Driver` seam the runner targets:

```go
// driver.go — the ONE seam EXT-01 adds to the swarm runner.
type Driver interface {
    // RunShard executes one shard's work and returns its result. LocalDriver runs it
    // in-process (the shipped pool); RemoteDriver leases it to a remote worker. Both
    // honor ctx cancellation and the per-shard concurrency cap.
    RunShard(ctx context.Context, sh swarm.Shard, fn swarm.ShardFn) spawn.Result
}
```

- **`LocalDriver`** wraps the shipped in-process path verbatim → `--driver local` is byte-identical
  to today's swarm (G6).
- **`RemoteDriver`** does: `Plane.AcquireLease(shard)` → `Provisioner.Provision(spec)` → push the
  shard + a **scoped lease token** to the worker → poll/stream the worker's `ShardResult` → on
  return, **re-fetch the worker's branch into a local throwaway worktree** and run the **local**
  verifier (the I2 re-check) → set `spawn.Result.Passed/Branch` **only** on a local-green verdict →
  `Provisioner.Reclaim(workerID)`.

The control plane is a **leasing** loop, not a job-push loop: a lease has a TTL; if a worker host
dies (laptop closes / spot VM reclaimed), the lease lapses, `reclaim.go` detects the orphan, and the
shard is re-queued to a fresh worker — **at-least-once with verifier-idempotent merge** (a duplicate
green branch is a no-op-or-conflict the Integrator already handles, `integrate.go:1-17`). Concurrency
is still a **bounded pool**: the control plane runs at most `--concurrency K` outstanding leases,
reusing the `scheduler`/`spawn.DAGScheduler` cap discipline to bound provisioning.

### 2.2 Cross-host shard state (`internal/fleet/state.go`)

This is the durability machinery `docs/SWARM.md` deliberately left local-only. EXT-01 wires the
**unbuilt multi-agent `RunState`/`ResumePlan`** (`internal/agent/durability.go:148-306, 328-387`,
"a documented follow-on", `ROADMAP-EXTERNAL-INFRA.md:46`) for cross-host handoff:

- Each shard is a `store.Task` in a **fleet-distinct Status namespace** (`fleet-leased`,
  `fleet-running`, `fleet-verified`, `fleet-orphaned`) so the native `InFlight`/`InFlightSupervise`
  sweeps (`durability.go:67-78, 217-219`) never re-drive a fleet shard. The `ShardRecord` (lease
  holder, branch, attempt, TTL, verdict — **refs + verdict, never the artifact body**) is marshaled
  into `store.Task.Detail` via the same crash-atomic single-`UpsertTask` RMW the swarm queue uses
  (`SWARM.md:570-575`).
- `RunState.TipSHA` + `Node.State` (`durability.go:151-160, 125-149`) carry the integration tip and
  per-shard disposition. On a control-plane restart, `RunState.ResumePlan()`
  (`durability.go:356-387`) partitions {already-merged → replay/skip, un-merged-ready → re-lease},
  preserving the **no-work-lost / no-double-merge** guarantee — now across hosts, not just across a
  local process restart.
- **The store remains the single-writer serialization point.** v1 keeps the **local** SQLite store
  as the control-plane's authority (the control plane is one host; workers are stateless and report
  back). A *shared/networked* state store (multiple control-plane replicas) is **out of scope for
  EXT-01 v1** and named as a future sub-item (§9) — it would need its own §0 gate and an I6 module.

This **extends I5**: every lease grant, worker report, reclaim, and verdict is an append-only,
metadata-only event on the **same hash-chained log** (`eventlog`), so the fleet's cross-host activity
is as replayable as the single-host swarm's.

### 2.3 The credential proxy (`internal/fleet/credproxy`)

The load-bearing I3 design (`ROADMAP-EXTERNAL-INFRA.md:49, 59`): a remote, possibly prompt-injected
worker must never hold a credential it could exfiltrate. NilCore solves it the way the leaders do —
a **scoped credential proxy**:

```
control-plane host (holds the REAL fleet cred in secrets.SecretStore)
  │
  │  1. mint ScopedToken{Repo, Branch=shard-branch, Exp=lease TTL, Sig=HMAC(secret)}   (token.go)
  │     — the token is NOT a credential; it is an HMAC-signed capability claim, key-free
  ▼
remote worker (holds ONLY the scoped token; the model never sees even the token —
               it is injected per-run via box.ExecWithEnv by name, the P11 finance-pack pattern)
  │
  │  2. worker pushes its branch by calling the proxy git endpoint with the token
  ▼
credproxy.Broker (control-plane-side):  verify HMAC + scope (repo match? branch match? not expired?)
  │     → if valid: perform the push USING THE REAL cred, restricted to the claimed branch
  │     → if the token asks to push outside its branch / another repo / after TTL: REFUSE + audit event
  ▼
  the real fleet credential NEVER leaves the control-plane host, NEVER enters a worker image layer,
  NEVER reaches the model.  (I3, docs/ARCHITECTURE.md:120-121)
```

Key properties the security review (G2) must confirm:

- **The real credential lives only in `secrets.SecretStore`** on the control-plane host
  (`secrets.go:19-24`), resolved by name via the `provider.ResolveWith`-style `getenv` →
  SecretStore lookup (`provider.go:26-44`). The **`secrets.ExternalStore` hook**
  (`internal/secrets/external.go:10-18`) is the seam for a corporate Vault/KMS broker (the
  `EXT-06` overlap, deliberately reused, not rebuilt).
- **The scoped token is a capability, not a secret.** It carries no key material; it is an
  HMAC-`crypto/hmac`+`crypto/sha256`-signed `{repo, branch, exp}` claim (`token.go`, stdlib). Even
  fully leaked, it lets the holder push **only** to one branch of one repo until TTL — the blast
  radius is one shard's branch, which the Integrator re-verifies and which never lands to base.
- **The model never sees even the token.** It is injected into the worker's box per-run via
  `box.ExecWithEnv` **by name** — the exact P11 keyed-pack discipline (`SWARM.md:134, 443`) — so the
  worker process can use it for `git push` to the proxy without it ever entering the model context
  (I3) or the worker's prompt (I7).
- **The proxy is a default-deny gate.** An unsigned/expired/out-of-scope push is refused and emits an
  `fleet_push_denied` audit event. The proxy is a small stdlib `net/http` server **on the
  control-plane host only** (not in the worker), with no broad authority — it can only perform the
  one scoped git operation the validated token authorizes.

### 2.4 How each piece EXTENDS the invariants over the real seams

| Piece | Real seam it rides | How it EXTENDS (not bypasses) the invariant |
|---|---|---|
| `RemoteWorker.Run` | `backend.CodingBackend.Run` (frozen, `CLAUDE.md` I1) | The remote unit is still `Run(ctx,Task)(Result,error)`; the fleet wraps it, never widens it. Shard-extra data rides `swarm.Shard`; any remote terminal signal rides a **sentinel error** (`backend.ErrSuspended` precedent, `SWARM.md:441`). |
| Local re-verify in `RemoteDriver` | `evverify.ArtifactVerifier` (`var _ verify.Verifier`, the per-shard I2 gate, `SWARM.md:133`) | The worker's `Result.SelfClaimed` stays **advisory**; the **local** verifier overwrites self-claimed statuses against the **re-fetched** worktree before merge. No remote self-report ships work (I2). |
| `credproxy` | `secrets.SecretStore` + `box.ExecWithEnv` by name | Adds standing authority **only** as a scoped, TTL-bounded, gated capability; the real cred stays in the store, never to the model (I3). |
| `ExternalProvisioner` | `secrets.ExternalStore` shell-hook pattern (`external.go:48-63`) | Provisioning is an operator-configured command — no cloud SDK in the core (I6); the worker image is a **sandbox** (I4). |
| Cross-host state | `agent.Checkpoint`/`RunState`/`ResumePlan` (`durability.go`) + `store` single-writer | Reuses the built-but-unwired durable machinery; every transition is one crash-atomic `UpsertTask` and one append-only event (I5). |
| Promotion | `policy.GateAction{PromoteToBase}` (`gateaction.go:28-31, 94-101`) | **Unchanged**: one gated action, human approver, nil-approver-deny. The fleet constructs no new `GateActionType` and never auto-lands (never-land). |

---

## §3 The task DAG

**Namespace `EXT-01-T01 … EXT-01-T14`.** One task = one branch (`task/EXT-01-T0x`) = one PR. Owns
sets are pairwise-disjoint (package dir = unit of ownership). `internal/fleet` is **one package = one
Owns unit**, so its files form a serialized sub-chain (the one intra-package chain, exactly as
`internal/swarm` did, `SWARM.md:494-498`). **The whole table is BLOCKED until §0 (G1–G6) clears.**

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-01-T01 | Driver seam + LocalDriver (additive on swarm runner) | — | `internal/fleet/driver.go`, `internal/fleet/driver_test.go` | opens `internal/fleet`; reuses shipped swarm |
| EXT-01-T02 | Scoped token (HMAC capability) | — | `internal/fleet/credproxy/token.go`, `token_test.go` | new sub-leaf; stdlib crypto |
| EXT-01-T03 | Provisioner iface + ExternalProvisioner | — | `internal/fleet/provisioner/` | new sub-leaf; shell-hook pattern |
| EXT-01-T04 | Credential proxy + Broker (git-push proxy) | EXT-01-T02 | `internal/fleet/credproxy/proxy.go`, `broker.go`, `*_test.go` | sub-leaf; **solo security-review surface (G2)** |
| EXT-01-T05 | Lease + LeaseStore + TTL/renew/reclaim | EXT-01-T01 | `internal/fleet/` (whole pkg): `lease.go`, `reclaim.go`, `*_test.go` | serial in `internal/fleet` after T01 |
| EXT-01-T06 | Cross-host shard state (RunState wiring) | EXT-01-T05 | `internal/fleet/` (whole pkg): `state.go`, `state_test.go` | serial after T05; wires `durability.RunState` |
| EXT-01-T07 | ControlPlane (lease loop, bounded) | EXT-01-T06, EXT-01-T03, EXT-01-T04 | `internal/fleet/` (whole pkg): `plane.go`, `plane_test.go` | serial after T06 |
| EXT-01-T08 | RemoteDriver (lease→provision→re-verify locally) | EXT-01-T07 | `internal/fleet/` (whole pkg): `remote.go`, `remote_test.go` | serial after T07; the I2 re-check lives here |
| EXT-01-T09 | RemoteWorker entrypoint (clone→Run→artifact→push) | EXT-01-T02, EXT-01-T03 | `internal/fleet/worker/` | new sub-leaf; runs in the VM |
| EXT-01-T10 | Provider failover light-up (ordered `[]Provider`) | — | `internal/fleet/providers.go` (in `internal/fleet`) | **serial in `internal/fleet`** — folds into the T05→T08 chain |
| EXT-01-T11 | `onboard.Config.Fleet` field + Validate | EXT-01-T01 | `internal/onboard/onboard.go`, `onboard_test.go` | **contract (config schema) — serialized** |
| EXT-01-T12 | `nilcore fleet` + `buildFleet` + dispatch | EXT-01-T08, EXT-01-T09, EXT-01-T11, EXT-01-T10 | `cmd/nilcore/fleet.go`, `cmd/nilcore/main.go` | **serialized cmd-wiring (one `case "fleet"`)** |
| EXT-01-T13 | `nilcore fleet-worker` remote entrypoint dispatch | EXT-01-T09, EXT-01-T12 | `cmd/nilcore/fleet-worker.go`, `cmd/nilcore/main.go` | **serial after T12 (same main.go)** |
| EXT-01-T14 | Docs + CHANGELOG + roadmap promotion | EXT-01-T13 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs) — serialized last** |

> Owns-disjointness note: `internal/fleet` (the package) is held by **T01, then T05→T06→T07→T08→T10**
> as a serial sub-chain (package = unit of ownership, `CLAUDE.md` §5 rule 3). The three sub-leaves
> (`credproxy`, `provisioner`, `worker`) and the cmd files are distinct dirs, so they parallelize
> against that chain. T11/T12/T13/T14 touch serialized contract surfaces, each held by exactly one
> task. **T10 is folded into the `internal/fleet` chain** (it adds `providers.go` to the same package
> dir); it is scheduled between T08 and T12 in the wave map.

---

## §4 Per-task specs

> Every task is **BLOCKED until §0 clears.** Each is `make verify`-green in isolation, stdlib-only
> (I6), and adds no behavior to the default binary (§8). "Verify" lists the proof obligations beyond
> `make verify`.

### EXT-01-T01 — Driver seam + LocalDriver  · opens `internal/fleet`
- **Goal:** add the one `fleet.Driver` seam the swarm runner targets, plus `LocalDriver` wrapping the
  shipped in-process path so `--driver local` is byte-identical to today's swarm (G6). Establishes
  `internal/fleet` ownership.
- **Depends on:** — (reuses shipped `internal/swarm`, `internal/spawn`, `internal/scheduler`).
- **Owns:** `internal/fleet/driver.go`, `internal/fleet/driver_test.go`, `internal/fleet/deps_test.go`.
- **Acceptance:** `Driver interface { RunShard(ctx, swarm.Shard, swarm.ShardFn) spawn.Result }`;
  `LocalDriver` delegates to the shipped pool path with no behavior change; `deps_test.go` runs
  `go list -deps` and asserts `internal/fleet` imports **no** `agent`/`super`/`project` and **no**
  net/http **server** (a client is allowed; an `http.Server`/rpc/remote-DB import fails the test).
- **Verify:** `make verify`; `go test -race ./internal/fleet/`; LocalDriver result equals the shipped
  swarm path for a scripted `ShardFn`; deps guard green.
- **Notes:** the seam is additive — `swarm.Runner` gains an optional `Driver` field defaulting to
  `LocalDriver`, so the swarm package is **not** edited destructively (a one-field additive change
  routed through the swarm owner at promotion, or — preferred — the runner already accepts a
  `RunShard` func, in which case `LocalDriver` just satisfies it with **zero** swarm edit).

### EXT-01-T02 — Scoped token (HMAC capability)  · new sub-leaf
- **Goal:** an HMAC-signed, key-free capability token `{Repo, Branch, Exp}` a worker can hold without
  holding any credential.
- **Depends on:** —.
- **Owns:** `internal/fleet/credproxy/token.go`, `credproxy/token_test.go`.
- **Acceptance:** `ScopedToken{Repo, Branch, Exp time.Time}`; `Mint(tok, signingKey) string` →
  base64url `payload.sig` where `sig = HMAC-SHA256(signingKey, payload)` (`crypto/hmac`,
  `crypto/sha256`, `crypto/rand` for nonce, all stdlib); `Verify(s, signingKey) (ScopedToken, error)`
  rejects a bad sig / expired / malformed token with **closed** error codes; the token carries
  **no** key material and **no** model-authored field; a constant-time `hmac.Equal` compare.
- **Verify:** `make verify`; `go test -race ./internal/fleet/credproxy/`: round-trip; tampered
  payload → `ErrBadSig`; expired → `ErrExpired`; assert the marshaled token contains no substring of
  the signing key; fuzz `Verify` against random bytes (never panics, always a typed error).
- **Notes:** stdlib only (I6). The signing key is a fleet secret in `SecretStore`, never in the token.

### EXT-01-T03 — Provisioner iface + ExternalProvisioner  · new sub-leaf
- **Goal:** provision/reclaim a worker VM via an **operator-configured command**, so no cloud SDK
  enters the core (I6, G4).
- **Depends on:** —.
- **Owns:** `internal/fleet/provisioner/provisioner.go`, `external.go`, `spec.go`, `*_test.go`.
- **Acceptance:** `Provisioner interface { Provision(ctx, WorkerSpec) (Worker, error); Reclaim(ctx, workerID) error }`;
  `WorkerSpec{Image, Region, EgressAllow []string, Resources}` is **DATA** (never a model-emitted
  command); `ExternalProvisioner{Command, Args}` shells `Command Args... provision|reclaim` with the
  spec on **stdin as JSON** and the worker handle on stdout — the exact `secrets.ExternalStore.run`
  pattern (`external.go:48-63`), so a vendor adapter lives outside the binary; argv carries no
  secret; a nil/empty Command is a clean setup error (fail-closed).
- **Verify:** `make verify`; `go test ./internal/fleet/provisioner/...` with a stub command (a tiny
  test script echoing a canned handle): provision round-trips the spec; reclaim is idempotent; empty
  Command errors; spec marshals to stdin, never argv (assert no spec field in the command's argv).
- **Notes:** the `EgressAllow` maps to the worker sandbox's egress allowlist (default-deny, I4); the
  worker image **is** a sandbox (`docs/ARCHITECTURE.md:150` spectrum — container/namespace today,
  the future `EXT-08` microVM tier slots in here behind the same `WorkerSpec`).

### EXT-01-T04 — Credential proxy + Broker  · solo security-review surface (G2)
- **Goal:** the control-plane-side broker that validates a scoped token and performs the **one**
  scoped git operation (branch push) using the real credential — which never leaves the host.
- **Depends on:** EXT-01-T02.
- **Owns:** `internal/fleet/credproxy/proxy.go`, `broker.go`, `proxy_test.go`, `broker_test.go`.
- **Acceptance:** `Broker interface { PushScoped(ctx, ScopedToken, packfile io.Reader) error }`;
  `CredentialProxy` is a stdlib `net/http` server **on the control-plane host**; an inbound push
  request validates `token.Verify` **then** asserts the push targets the token's exact `Repo`/`Branch`
  and is not expired — any mismatch ⇒ **HTTP 403 + `fleet_push_denied` metadata-only audit event**
  (no token/cred in the event, I3/I5); a valid push resolves the real credential **by name** from
  `secrets.SecretStore` (`secrets.go:19-24`; `ExternalStore` hook supported, `external.go:10-18`) and
  performs the branch-restricted push; the real credential is **never** in a response, a log, or the
  token; the proxy holds **no** other authority (it can only do the one scoped push).
- **Verify:** `make verify`; `go test -race ./internal/fleet/credproxy/...`: valid token → push
  invoked with the real cred (a fake `SecretStore` records the lookup-by-name); out-of-scope branch →
  403, **no** cred lookup, audit event emitted; expired token → 403; a **redaction test** asserting
  no log/response/event ever contains the signing key or the real cred; an injection-phrase in a
  request field never appears in the audit detail (I7).
- **Notes:** **this is the G2 security-review artifact.** Solo task; its PR is the one the review signs
  off. The threat model in the spec: a fully-complying injected worker still cannot exfiltrate the
  cred, push outside its branch, or reach another tenant.

### EXT-01-T05 — Lease + LeaseStore + TTL/renew/reclaim  · serial in `internal/fleet`
- **Goal:** the leasing primitives — a TTL-bounded `Lease`, a `LeaseStore` over the local SQLite
  store, and orphan reclaim when a worker host dies.
- **Depends on:** EXT-01-T01.
- **Owns:** `internal/fleet/` (whole pkg): `lease.go`, `reclaim.go`, `lease_test.go`, `reclaim_test.go`.
- **Acceptance:** `Lease{ShardID, WorkerID, Token string, Acquired, TTL, Branch, State}` (closed
  `LeaseState`: `held/renewed/lapsed/released`); `LeaseStore{Acquire, Renew, Release, Lapsed(now)}`
  over `store.UpsertTask`/`TasksByStatus` in a **fleet-distinct Status namespace**
  (`fleet-leased`/`fleet-released`) so native sweeps never touch it (`durability.go:67-78` posture);
  `Reclaim(now)` returns shards whose lease lapsed (TTL elapsed without renew) for re-leasing;
  **at-least-once** semantics documented (a reclaimed-then-completed shard's duplicate green branch is
  the Integrator's idempotent no-op/conflict, `integrate.go:1-17`).
- **Verify:** `make verify`; `go test ./internal/fleet/` with `:memory:`/temp `store.Open`: acquire
  round-trips; renew extends TTL; `Lapsed` returns only past-TTL un-renewed leases; reclaim re-queues
  an orphan; namespace isolation (a fleet-leased row absent from native `InFlight`).
- **Notes:** the single SQLite writer is the serialization point (`store.go:53`) — one row write per
  lease transition, never per token.

### EXT-01-T06 — Cross-host shard state (RunState wiring)  · serial after T05
- **Goal:** persist shard state cross-host by wiring the **built-but-unwired** multi-agent
  `RunState`/`ResumePlan` (`durability.go:148-306, 328-387`) into the fleet, so a control-plane
  restart re-leases only un-merged shards with zero lost progress.
- **Depends on:** EXT-01-T05.
- **Owns:** `internal/fleet/` (whole pkg): `state.go`, `state_test.go`.
- **Acceptance:** `ShardRecord{ShardID, Branch, Attempt, LeaseTTL, Verdict}` (refs + verdict,
  **never** the artifact body) marshaled into `store.Task.Detail` via crash-atomic single-`UpsertTask`
  RMW; the fleet uses `agent.RunState{TipSHA, Nodes}` + `ResumePlan()` (`durability.go:356-387`)
  verbatim to partition {merged→replay, un-merged-ready→re-lease, skip}; resume recomputes the open
  set from persisted records (no double-merge guard preserved); `Log.Err()` polled — a broken chain
  HALTS the control plane (I5).
- **Verify:** `make verify`; `go test ./internal/fleet/`: a crash mid-run + a fresh ControlPlane
  re-leases only un-merged shards (call-count map proves merged shards not re-leased); TipSHA threads
  forward; `ResumePlan` reuse asserted (the partition matches `durability_test` cases).
- **Notes:** v1 state authority is the **local** SQLite store (control plane = one host). A shared
  state store across control-plane replicas is out of scope (§9) — a future sub-item with its own
  gate + I6 module.

### EXT-01-T07 — ControlPlane (bounded lease loop)  · serial after T06
- **Goal:** the leasing loop — at most `--concurrency K` outstanding leases, provisioning a worker per
  lease, brokering the scoped token, polling the worker, reclaiming orphans — reusing the
  `scheduler`/`spawn.DAGScheduler` cap discipline to bound provisioning.
- **Depends on:** EXT-01-T06, EXT-01-T03, EXT-01-T04.
- **Owns:** `internal/fleet/` (whole pkg): `plane.go`, `plane_test.go`.
- **Acceptance:** `ControlPlane{Provisioner, LeaseStore, Proxy, Verifier, Integrate, Budget, Log}`;
  `Run(ctx, []swarm.Shard, K) (Outcome, error)` leases ≤ K at once (peak-in-flight ≤ K, the
  `scheduler` invariant, `scheduler.go:139-144`); a shard's worker failure/orphan is a **recorded**
  result, never a plane abort (the `scheduler` one-task-failure-isolation contract, `scheduler.go:21`);
  honors `DependsOn` via `spawn.DAGScheduler` for code shards (`SWARM.md:587`); `ctx` cancel stops new
  leases and reclaims outstanding workers; every transition is a metadata-only event (I5).
- **Verify:** `make verify`; `go test -race ./internal/fleet/`: 300 shards @ K=40 → peak outstanding
  leases ≤ 40, all 300 leased; a worker-erroring lease recorded not fatal; ctx cancel reclaims;
  DAG A→B releases B only after A verified.
- **Notes:** bounded pool, single host — the EXT-01 boundary is **concurrency of leases**, not a
  removal of the cap. The provisioning cap reuses the proven `scheduler` cap.

### EXT-01-T08 — RemoteDriver (lease → provision → local re-verify)  · serial after T07; the I2 re-check
- **Goal:** the `Driver` that runs a shard remotely and **re-runs the local verifier over the
  re-fetched worktree** before the shard is a merge candidate — the per-shard I2 gate, now spanning
  hosts.
- **Depends on:** EXT-01-T07.
- **Owns:** `internal/fleet/` (whole pkg): `remote.go`, `remote_test.go`.
- **Acceptance:** `RemoteDriver.RunShard`: `Plane` leases → worker runs → on `ShardResult` return,
  **fetch the worker's branch into a local throwaway worktree** and run the **local**
  `evverify.ArtifactVerifier` (the shipped per-shard gate, `SWARM.md:133`) against the **re-fetched**
  artifact; `spawn.Result.Passed/Branch` set **only** on a local-green verdict; the worker's
  `Result.SelfClaimed` stays **advisory** (never decides done, I2); a worker that returns
  green-but-locally-red is a **recorded fail** (and requeued by the swarm controller); a network/lease
  error ⇒ recorded fail, re-leasable.
- **Verify:** `make verify`; `go test -race ./internal/fleet/` (fake provisioner + fake worker +
  stub verifier): worker-claims-green-but-local-verifier-red ⇒ `Passed:false` (the keystone I2 test);
  worker-green-and-local-green ⇒ `Passed:true` + branch set; lease error ⇒ recorded fail; the local
  verifier is invoked over the re-fetched tree (assert the fetch happened before the check).
- **Notes:** **this is the G3 proof in code.** No remote self-report ships work; the local verifier is
  the sole authority, re-checking re-fetched bytes (so a lying worker cannot fake a green artifact).

### EXT-01-T09 — RemoteWorker entrypoint  · new sub-leaf (runs in the VM)
- **Goal:** the remote-side agent: clone with the scoped token, run the loop in-box, write the
  artifact, push the branch via the proxy, report the result — using the **same** seams
  `buildBackend`/`NewWorker` use today (I1/I4).
- **Depends on:** EXT-01-T02, EXT-01-T03.
- **Owns:** `internal/fleet/worker/worker.go`, `worker/worker_test.go`.
- **Acceptance:** `RemoteWorker.Run(ctx, Shard, scopedToken)`: clone the repo using the scoped token
  (injected via env **by name**, never in the model context, I3); build the stack in-box (the
  `buildBackend(name,…,box,verifier,…)` analogue — `main.go:1402`); run `backend.CodingBackend.Run`
  (frozen, I1); write the artifact JSON to `.nilcore/artifacts/<id>.json` (`roster.ArtifactRelPath`,
  `roster.go:72-78`); push the branch by calling the proxy with the token; return a `ShardResult`
  (branch ref + advisory self-claim); the worker **never** logs the token/cred; runs entirely inside
  the remote sandbox (I4).
- **Verify:** `make verify`; `go test ./internal/fleet/worker/...` (fake clone/push + scripted
  backend): `Run` produces an artifact file + a branch ref; push goes through the proxy endpoint, not
  a direct remote; the token never appears in the worker's emitted logs/prompt (assert redaction).
- **Notes:** the worker is `backend.Run` wrapped — I1 holds (`ROADMAP-EXTERNAL-INFRA.md:56-62`). It is
  shipped **inside the same binary** (the `fleet-worker` subcommand, T13), so the worker image is just
  `nilcore` — no separate artifact, keeping G5's reversible-to-remove property.

### EXT-01-T10 — Provider failover light-up  · serial in `internal/fleet`
- **Goal:** light up the **multi-provider** routing `model.Resilient` already supports but is passed a
  single element today (`main.go:1252`; `resilience.go:105-141`) — a fleet routes across providers for
  failover under load (`ROADMAP-EXTERNAL-INFRA.md:56`).
- **Depends on:** — (folds into the `internal/fleet` chain between T08 and T12).
- **Owns:** `internal/fleet/providers.go`, `internal/fleet/providers_test.go`.
- **Acceptance:** `BuildProviders(specs []string, cred, opts) (*model.Resilient, error)` resolves an
  **ordered** `[]model.Provider` via `provider.ResolveWith(spec, cred)` (`provider.go:26-44`) and wraps
  with `model.NewResilient(providers, model.Options{Jitter, BreakerThreshold, CallTimeout, MaxRetries})`
  (`resilience.go:124-141`) — **large `Jitter`** for a fleet (avoid a synchronized 429 retry storm,
  the documented `resilience.go:33-35` rationale); the cred resolver is the env→SecretStore lookup
  (never a key to a decorator, I3); a single-spec list is byte-identical to today's wiring.
- **Verify:** `make verify`; `go test -race ./internal/fleet/`: p1 rate-limited → p2 succeeds
  (failover); single-spec == today's behavior; cred resolved by name (fake cred records the lookup);
  Jitter applied (deterministic-clock test).
- **Notes:** **zero new module** — `model.Resilient` is shipped; EXT-01 only passes it the ordered list
  the swarm passed a singleton.

### EXT-01-T11 — `onboard.Config.Fleet` field + Validate  · contract (config schema), serialized
- **Goal:** additively extend `onboard.Config` with one optional `Fleet *fleet.FleetConfig`
  (`json:"fleet,omitempty"`) + a Validate clause, v1-config-compatible (the exact `Pool` precedent,
  `SWARM.md:556-561`).
- **Depends on:** EXT-01-T01.
- **Owns:** `internal/onboard/onboard.go`, `internal/onboard/onboard_test.go`.
- **Acceptance:** default-zero so every existing config parses unchanged under
  `DisallowUnknownFields`; `Validate()` gains a fleet clause (provisioner command present when fleet
  enabled; provider specs valid; positive concurrency; loud error otherwise); old configs without
  `fleet` parse; a config with `fleet` round-trips parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip with `Fleet` set; old config
  parses; `Validate` rejects an enabled fleet with no provisioner command.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface),
  treated as a contract surface (the `SWARM.md:556-561` posture). `onboard → fleet` is downward (no
  cycle).

### EXT-01-T12 — `nilcore fleet` + `buildFleet` + dispatch  · serialized cmd-wiring
- **Goal:** the operator front door — the `fleet` subcommand, `buildFleet` (the `buildSwarm`/`buildStack`
  analogue), and the **one** new dispatch case (`case "fleet"`) + usage line in `main.go`.
- **Depends on:** EXT-01-T08, EXT-01-T09, EXT-01-T11, EXT-01-T10.
- **Owns:** `cmd/nilcore/fleet.go` (new), `cmd/nilcore/main.go` (one `case "fleet"` + usage line — the
  only edit to an existing file).
- **Acceptance:** `registerFleetFlags` parses `--driver local|remote` (default **local**, the G6
  byte-identical fallback), `--goal`, `--agents N`, `--concurrency K`, `--passes`, `--budget`,
  `--provider` (repeatable, ordered, the T10 failover list), `--provisioner-cmd`, `--lease-ttl`,
  `--region`, `--image`, `--egress-allow`, `--report`; `buildFleet(deps) (assembly, error)` composes,
  in order: `loadBoot` → `openLog` → `budget.New()`+`SetGlobalCeiling` → `secrets.Detect`/configured
  store → `credproxy.New` (real cred from the store) → `provisioner.NewExternal(cmd)` →
  `fleet.BuildProviders(specs)` → `fleet.NewControlPlane{…}` → `fleet.RemoteDriver` (or `LocalDriver`)
  → the shipped swarm `Runner`/`Controller` pointed at the chosen `Driver`; the final clean tip
  promotes through **one** `policy.GateAction{PromoteToBase}` (optional `route.Review` first; **nil
  approver default-denies**, `gateaction.go:94-101`); add `case "fleet"` + the usage line; **no** other
  arm or shared helper edited; unknown `--driver`/missing provisioner is FATAL at startup.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, fake provisioner + fake worker):
  default `--driver local` == shipped swarm; `--driver remote` with no provisioner → `buildFleet`
  error; nil-approver promotion is **denied** (the never-land test); single shared `*budget.Ledger`;
  an **import-graph test** asserting no existing package imports `internal/fleet*`; an **init()-free
  test** asserting the new leaves have no global-side-effect `init()`.
- **Notes:** **serialized cmd-wiring** — one of the two tasks editing `main.go` (T13 is the other,
  serial after). Default binary byte-identical (one new arm + usage line; all logic in new files).

### EXT-01-T13 — `nilcore fleet-worker` remote entrypoint  · serial after T12 (same main.go)
- **Goal:** the hidden subcommand the remote VM boots into — `nilcore fleet-worker` invokes
  `RemoteWorker.Run` — so the worker image is just the shipped binary (no separate artifact, G5).
- **Depends on:** EXT-01-T09, EXT-01-T12.
- **Owns:** `cmd/nilcore/fleet-worker.go` (new), `cmd/nilcore/main.go` (one `case "fleet-worker"`).
- **Acceptance:** `case "fleet-worker"` parses the shard descriptor + scoped token from env/stdin
  (token **by name**, never argv, I3), builds the in-box stack, runs `RemoteWorker.Run`, reports the
  `ShardResult`; it is **not** advertised in the default usage (it is an internal entrypoint) but is
  reachable only when the control plane boots a worker into it; default dispatch path never reaches it.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...`: `fleet-worker` with a scripted shard runs
  `RemoteWorker.Run`; absent the arg the default path is unaffected; token never in argv/logs.
- **Notes:** serial after T12 because both edit `main.go` (the same serialized file). The worker arm
  is gated behind the explicit subcommand, so byte-identity holds (§8).

### EXT-01-T14 — Docs + CHANGELOG + roadmap promotion  · contract (docs), serialized last
- **Goal:** promote this plan into the canonical docs and ledger as the (gated) EXT-01 build.
- **Depends on:** EXT-01-T13.
- **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`,
  `CHANGELOG.md`, `README.md`.
- **Acceptance:** `docs/TASKS.md` gains the EXT-01 DAG rows + specs **prefixed with the §0
  gate-cleared note** (rows reuse the shipped swarm/durability/secrets seams, not rebuild);
  `docs/ARCHITECTURE.md` a "Managed cloud agent fleet (EXT-01, gated)" subsection (remote-`backend.Run`
  I1, local-re-verify I2, credential-proxy I3, in-box worker I4, cross-host append-only audit I5,
  stdlib-only I6, untrusted-worker-output-as-data I7, never-land preserved) + the new leaf rows in the
  layer-map with import sets; `docs/ROADMAP-EXTERNAL-INFRA.md` updates the EXT-01 section from "how it
  *would* be built" to "built behind the cleared gate, recorded by <owner> on <date>"; `CLAUDE.md` one
  repository-map line (no invariant text change — the invariants are unchanged, which is the point);
  `CHANGELOG.md` one `## [Unreleased]` entry per merged `EXT-01-T0x`; `README.md` the `nilcore fleet`
  usage + the default-off note + the honest caveats (§9).
- **Verify:** `make verify` (docs don't break the build); markdown pass; manual review that the
  layer-map import sets match each leaf's actual `go list -deps`.
- **Notes:** **serialized — contract files.** Lands last. The §0 gate record (G1–G6) is the
  precondition for this PR even existing.

---

## §5 Wave map, critical path, serialization points

A fleet of agents (dogfooding, post-gate) executes in ordered **waves**; every task in a wave has all
deps merged to `main` and a pairwise-disjoint Owns set. `internal/fleet` is **one package = one Owns
unit**, so its files serialize (the one intra-wave chain, exactly the `internal/swarm` precedent).

```
GATE  (§0 — NOT a task; a recorded human thesis decision G1 + security-review G2)
  └── nothing below is eligible work until this is recorded in the promoting PR

WAVE 1  (4 concurrent — no-dep new leaves/sub-leaves, each independently make-verify-green)
  ├── EXT-01-T01  internal/fleet/driver.go            (opens internal/fleet)
  ├── EXT-01-T02  internal/fleet/credproxy/token.go
  ├── EXT-01-T03  internal/fleet/provisioner/
  └── EXT-01-T09  internal/fleet/worker/              (deps T02,T03 — schedule once both land; see note)

WAVE 2  (2 concurrent)
  ├── EXT-01-T04  internal/fleet/credproxy/proxy.go   (T02)   ← SOLO security-review surface (G2)
  └── EXT-01-T05  internal/fleet/lease.go             (T01)   ← opens the internal/fleet serial chain

WAVE 3  (1 — serial in internal/fleet)
  └── EXT-01-T06  internal/fleet/state.go             (T05)   wires durability.RunState cross-host

WAVE 4  (1 — serial in internal/fleet)
  └── EXT-01-T07  internal/fleet/plane.go             (T06,T03,T04)

WAVE 5  (1 — serial in internal/fleet)
  └── EXT-01-T08  internal/fleet/remote.go            (T07)   ← the I2 local-re-verify (G3 proof)

WAVE 6  (2 concurrent)
  ├── EXT-01-T10  internal/fleet/providers.go         (folds into the chain — serial after T08)
  └── EXT-01-T11  internal/onboard/onboard.go         (T01)   ← SERIAL pt: config-schema contract

WAVE 7  (1 — SERIAL pt: cmd-wiring)
  └── EXT-01-T12  cmd/nilcore/fleet.go + main.go      (T08,T09,T11,T10)   ← sole main.go editor (case "fleet")

WAVE 8  (1 — SERIAL pt: cmd-wiring, same main.go)
  └── EXT-01-T13  cmd/nilcore/fleet-worker.go + main.go (T09,T12)         ← case "fleet-worker"

WAVE 9  (1 — SERIAL pt: docs contract)
  └── EXT-01-T14  docs/* + CLAUDE.md + README.md + CHANGELOG.md (T13)     ← sole docs editor
```

> Wave-1 note: `EXT-01-T09` (worker) depends on T02+T03, both wave-1 leaves. It is parallel-safe
> (distinct dir) but cannot *merge* until its deps merge; in practice it runs concurrently and rebases.
> If strict wave discipline is preferred, schedule T09 in wave 2 alongside T04/T05.

**Peak concurrency = 4 (wave 1).** **Critical path (longest dependency chain) — 8 sequential merges:**

```
EXT-01-T01 → T05 → T06 → T07 → T08 → T12 → T13 → T14
```

(`internal/fleet`'s serial sub-chain T05→T06→T07→T08, then the cmd-wiring T12→T13, then docs T14.)

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/fleet` package dir — T01 opens; T05→T06→T07→T08→T10 serialize as sibling files
   (package = unit of ownership; per-file sub-packages would risk a cycle with the cmd layer, so the
   chain is the correct trade, exactly the `internal/swarm` rationale, `SWARM.md:683-685`).
2. `internal/fleet/credproxy/proxy.go` — T04 only (**the G2 security-review surface**).
3. `internal/onboard/onboard.go` — T11 only (config schema).
4. `cmd/nilcore/main.go` — T12 then T13 (serial; both edit the one dispatch file).
5. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — T14 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; the `internal/fleet` sub-chain
is strictly increasing IDs; `onboard → fleet` and `cmd → fleet` are downward. **Foundation-before-
orchestration holds:** the control plane (T07) literally cannot compile until the lease primitives
(T05) and cross-host state (T06) exist; the remote driver's I2 re-check (T08) cannot exist until the
plane (T07) does.

---

## §6 Per-invariant ledger

Every invariant holds **by reuse + scoped extension**, not by new mechanism — and EXT-01 is allowed
*only* because each added authority is scoped, gated, credential-stored, and never handed to the model
(`ROADMAP-EXTERNAL-INFRA.md:212`).

| Invariant | How EXT-01 preserves it (seam-cited) |
|---|---|
| **I1** frozen `backend.CodingBackend` | The remote unit of work is **still** `Run(ctx, Task) (Result, error)` inside the worker (`RemoteWorker.Run`, T09; `ROADMAP-EXTERNAL-INFRA.md:56-62`). `backend.Task`/`Result`/the interface are **untouched**. Shard-extra data rides `swarm.Shard`; a remote terminal signal rides a **sentinel error** (`backend.ErrSuspended` precedent), never a `Result` field. |
| **I2** verifier sole authority | The worker's `Result.SelfClaimed` is **advisory**. `RemoteDriver` (T08) **re-fetches the worker's branch and re-runs the LOCAL `evverify.ArtifactVerifier`** (the shipped per-shard gate, `SWARM.md:133`) before the shard is a merge candidate; `Passed/Branch` set **only** on a local-green verdict. The Integrator re-verifies every merge and **never lands to base** (`integrate.go:1-17`). No remote self-report ships work (G3). |
| **I3** no ambient authority | The **real fleet credential lives only in `secrets.SecretStore`** on the control-plane host (`secrets.go:19-24`; `ExternalStore` Vault/KMS hook, `external.go:10-18`), resolved by name (`provider.go:26-44`). A worker holds **only** a scoped, TTL-bounded, key-free HMAC token (T02), injected per-run via `box.ExecWithEnv` **by name** (the P11 keyed-pack discipline, `SWARM.md:134`) — **never to the model**, never to the worker's prompt. The credential proxy (T04) is default-deny: an out-of-scope/expired token push is refused + audited. A fully-injected worker cannot exfiltrate the cred or push outside its branch (G2). |
| **I4** sandboxed execution | The worker runs the loop **inside the remote VM/microVM sandbox** (the `docs/ARCHITECTURE.md:150` isolation spectrum; the future `EXT-08` microVM slots in behind the same `WorkerSpec`, T03). Provisioning is an operator command (no cloud SDK on the host); per-run secrets inject via the sandbox env by name. The control plane's structured I/O (state, leases) is host-side but store-confined. |
| **I5** append-only hash-chained audit | Every lease grant, worker report, reclaim, push-allow/deny, and verdict is a metadata-only event on the **same `eventlog` hash chain** (cross-host activity is as replayable as single-host). `Log.Err()` is polled — a broken chain **HALTS** the control plane (T06). History is never mutated. |
| **I6** zero-dep core | All new leaves are **stdlib-only** (`net/http` client + `crypto/hmac`/`sha256`/`rand` + `encoding/json` + `os/exec`); `CGO_ENABLED=0` (`.github/workflows/release.yml`); **no `go.mod` change**. Provisioning is shelled out (the `ExternalStore` pattern), so no vendor SDK enters the binary. See [§7](#7-module-justifications). |
| **I7** untrusted input is data | A remote worker's output (artifact JSON, branch, self-claim) is **untrusted** — re-verified locally over re-fetched bytes (I2), and `guard.Wrap`'d before ever entering a prompt. Only verifier-set `Status`/`Detail` are trusted; the worker's prose/self-claim never becomes a controlling instruction. The proxy never echoes a request field into an audit detail (T04 redaction test). |
| **Never-land** | Promotion onto base is **exactly one** gated `policy.GateAction{PromoteToBase}` through the human approver; a **nil approver default-denies** (`gateaction.go:94-101`). The fleet constructs **no** new `GateActionType` and **never auto-lands** — the Integrator returns a throwaway tip (`integrate.go:12-17`), and EXT-01 changes nothing about the promotion path. |

---

## §7 Module justifications

**Stdlib only — no `go.mod` change.** EXT-01 adds **zero** modules to the core, by deliberate design
choices that keep the §0 G4 / I6 budget at zero:

| Capability that *could* pull a module | How EXT-01 avoids it (stays stdlib, CGO-free) |
|---|---|
| Cloud VM/microVM provisioning (AWS/GCP/Firecracker SDK) | **Shelled to an operator-configured command** (`ExternalProvisioner`, T03 — the `secrets.ExternalStore` shell-hook pattern, `external.go:48-63`). The vendor adapter lives **outside** the binary; the core speaks JSON over stdin/stdout. |
| Cross-host RPC / control-plane transport | **stdlib `net/http` + JSON** (the MCP-over-JSON-RPC precedent, `CLAUDE.md` §2.6 — "the MCP client is **not** a module"). The credential proxy and worker-report transport are `net/http` client/server, no gRPC, no framework. |
| Token signing / capability tokens | **stdlib `crypto/hmac` + `crypto/sha256` + `crypto/rand`** (the codebase hand-rolls crypto to stay stdlib — PBKDF2 in `internal/secrets/file.go`, the `ROADMAP-EXTERNAL-INFRA.md:18` precedent). **No JWT library.** |
| Cross-host shared state DB | **Out of scope for v1** (the local single-writer SQLite store is the authority; control plane = one host). A shared store would be a future sub-item with **its own §0 gate** and an explicitly-justified, CGO-free driver. |

If a future EXT-01 sub-item (multi-replica control plane, §9) genuinely requires a module, it is
justified in **both** the PR and the CHANGELOG and must keep `CGO_ENABLED=0` — but **v1 is `go.mod`-
clean**, which is itself a gate-clearance proof point (G4).

---

## §8 Default-off byte-identical proof

The default `nilcore` binary is **byte-identical** with EXT-01 absent — proven, not asserted (the
exact `SWARM.md:458-463` discipline):

1. **No existing package imports a fleet leaf.** An import-graph test (T12) runs `go list -deps` over
   every shipped package and asserts none imports `internal/fleet`, `internal/fleet/credproxy`,
   `internal/fleet/provisioner`, or `internal/fleet/worker`. The only importer is `cmd/nilcore`, and
   only from the new `fleet.go`/`fleet-worker.go` files.
2. **No global side-effect `init()`.** An `init()`-free test (T12) asserts the new leaves contain no
   `init()` with global side effects, so merely **linking** the package cannot change behavior.
3. **The default dispatch path reaches neither new arm.** `main.go` gains exactly two arms
   (`case "fleet"`, `case "fleet-worker"`) + one usage line. The default invocation (`nilcore` /
   `nilcore run` / `nilcore swarm`) reaches none of them — byte-identity is established exactly as the
   shipped `schedule`/`watch`/`swarm` cases are, and exactly as `--driver local` is the shipped
   single-host swarm path (G6).
4. **`--driver local` is the shipped swarm.** `LocalDriver` (T01) wraps the in-process pool with no
   behavior change, so even when `fleet` is invoked with the local driver, it is the byte-identical
   single-host swarm — the remote path is reached **only** with an explicit `--driver remote` + a
   configured provisioner.
5. **Config compatibility.** `onboard.Config.Fleet` is `omitempty` and default-zero (T11), so every
   existing `config.json` parses unchanged under `DisallowUnknownFields` — no config migration, fully
   reversible to remove (G5).

---

## §9 Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | **Credential exfiltration by a prompt-injected remote worker** (the headline I3 risk). | The credential proxy (T04): the worker holds only a scoped, TTL-bounded, key-free token; the real cred never leaves the control-plane host; the proxy is default-deny + audited. Blast radius of a fully-leaked token is one branch of one repo, which the verifier re-checks and which never lands to base. **G2 security review is a gate precondition.** |
| R2 | **A lying worker returns a green artifact it did not actually verify** (I2 erosion across hosts). | `RemoteDriver` (T08) re-fetches the worker's branch and re-runs the **local** verifier over the **re-fetched bytes** — the worker's self-claim is advisory and a worker cannot fake a local-green verdict it does not actually pass. Keystone test: worker-claims-green-but-local-red ⇒ `Passed:false`. |
| R3 | **Cross-host state corruption / double-merge on orphan reclaim** (at-least-once delivery). | `RunState`/`ResumePlan` reuse (T06, `durability.go:356-387`) — the no-double-merge guard partitions {merged→replay, un-merged-ready→re-lease}; a duplicate green branch is the Integrator's idempotent no-op/conflict (`integrate.go:1-17`). Crash-mid-run test proves merged shards are not re-leased. |
| R4 | **Thesis drift: the "tiny self-hosted binary" identity dilutes into a managed SaaS** (`ROADMAP-EXTERNAL-INFRA.md:7, 50`). | The §0 gate (G1) is a recorded, non-delegable human decision; the single-host swarm stays the default (G6); default-off byte-identity (§8); `--driver local` always available. EXT-01 is an opt-in *additional* driver, never a replacement. |
| R5 | **A cloud-SDK dependency sneaks in** (I6 budget breach). | Provisioning is shelled to an operator command (T03); transport is stdlib `net/http`+JSON; tokens are stdlib crypto. v1 is `go.mod`-clean (G4 / §7). A `deps_test` + the CHANGELOG-justification rule catch any addition. |
| R6 | **Multi-replica control plane needs a shared/networked state store** (a second host reading shard state). | **Explicitly out of scope for EXT-01 v1.** v1's control plane is one host over the local single-writer SQLite store. A shared store is a named **future sub-item** requiring its own §0 gate + a justified CGO-free module — never slipped in under v1. |
| R7 | **The credential proxy itself becomes a single point of standing authority / failure.** | The proxy holds **one** scoped capability (a branch push) and lives only on the control-plane host; it is default-deny; its authority is the minimal git operation, not broad cloud access. A proxy outage stops *new* pushes (workers fail closed + re-lease), never lands unverified work. |
| R8 | **Orphan storms** (many spot VMs reclaimed at once → re-lease thrash). | The control plane is a **bounded** lease pool (≤ K outstanding, the `scheduler` cap, T07); reclaim re-queues into the same bounded pool; `model.Resilient` large `Jitter` (T10) prevents synchronized retry storms (`resilience.go:33-35`). |
| R9 | **A worker reaches another tenant's worktree / repo.** | Each worker is a fresh isolated VM/sandbox (I4) with a per-shard scoped token whose `Repo`/`Branch` are HMAC-bound; the proxy refuses any cross-repo/cross-branch push (T04). No shared filesystem between workers. |

---

*EXT-01 is the clearest identity change on the roadmap, and this plan exists so that **if** the §0
gate ever clears, the fleet is built the way the rest of NilCore earns trust: scoped authority through
the credential proxy, the human gate on every land, the SecretStore holding every key, and a local
verifier that still has the only vote on "done." Until a human records that decision, every row above
is unbuilt — by design.*
