# EXT-06 — Centralized secret distribution across a fleet (GATED, docs-only plan)

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/SECRETS.md` → `docs/ROADMAP-EXTERNAL-INFRA.md` (§0 gate, §7 EXT-06) → `docs/SWARM.md` (the single-host projection of a fleet) → this file.

> **Status: BLOCKED behind the §0 gate.** This is an *implementation-ready plan*, not an eligible task. Not one line of `EXT-06-T##` may be written until the `docs/ROADMAP-EXTERNAL-INFRA.md` §0 gate clears **and** its parent items (`EXT-01` managed cloud fleet, `EXT-05` enterprise control plane) are themselves past the gate. EXT-06 has **no standalone reason to exist** — a central broker only earns its blast radius once there is a fleet to distribute to (`docs/ROADMAP-EXTERNAL-INFRA.md:154` "Tied to `EXT-01`/`EXT-05`; do not build standalone"). The plan is structured so that *when* the gate clears, a fleet of parallel agents can execute it under the `CLAUDE.md` §5 work-selection rule with zero collision.

---

## Table of contents

- [§0 The gate — what must be true before any EXT-06 task is written](#0-the-gate)
- [§1 Summary](#1-summary)
- [§2 As-is: the secret seam that already exists (reuse, do not rebuild)](#2-as-is)
- [§3 The architecture (broker-fronting `ExternalStore`, per-worker leases)](#3-the-architecture)
- [§4 The task DAG (EXT-06-T01 … EXT-06-T10)](#4-the-task-dag)
- [§5 Per-task specs](#5-per-task-specs)
- [§6 Parallel wave map & critical path](#6-parallel-wave-map--critical-path)
- [§7 Per-invariant ledger (I3 in full)](#7-per-invariant-ledger)
- [§8 Module justification (the broker client)](#8-module-justification)
- [§9 Default-off byte-identical proof](#9-default-off-byte-identical-proof)
- [§10 Risks (blast radius, lease compromise, credential-proxy interplay)](#10-risks)

---

## §0 The gate

EXT-06 is **not** an eligible `CLAUDE.md` §5 task. Before a single line is written, **all** of `docs/ROADMAP-EXTERNAL-INFRA.md:13-21` must hold and be recorded in the PR that promotes EXT-06 into `docs/TASKS.md` (itself a serialized contract change). Restated against EXT-06 concretely:

1. **A recorded thesis decision (a human owner).** A human has explicitly decided NilCore's identity may expand from "secrets are **per-host**" (`internal/secrets/secrets.go:26-35`; `docs/ROADMAP-EXTERNAL-INFRA.md:36`) toward "a central broker distributes scoped, short-lived credentials across a fleet." This decision is the gate; it is **not** delegable to the agent — distributing secrets to many hosts is exactly the irreversible, outward-facing class of action the whole design reserves for a human (`docs/ROADMAP-EXTERNAL-INFRA.md:15`).
2. **`EXT-01`/`EXT-05` are themselves past their gate.** EXT-06 has no consumer otherwise. The plan binds to the EXT-01 *credential proxy* (`docs/ROADMAP-EXTERNAL-INFRA.md:49,59`) and the EXT-05 *RBAC/tenancy* scope keys (`docs/ROADMAP-EXTERNAL-INFRA.md:140`); both must exist as designed surfaces before T05/T08 below can compose with them.
3. **Invariants survive, not bypassed (`I3` load-bearing).** EXT-06 must **extend** the existing `secrets.SecretStore` seam, never weaken it. The cardinal rule is unchanged and *amplified*: secrets injected per-run, **never on disk in plaintext, never logged, never in a prompt, never given to the model** (`docs/ARCHITECTURE.md:120-121`; `docs/SECRETS.md:3-7`). Central distribution multiplies a leak's blast radius (`docs/ROADMAP-EXTERNAL-INFRA.md:152`), so the bar is *higher*, not lower — short-lived + scoped + least-privilege leases are mandatory, not optional.
4. **The verifier still governs (`I2`); the integrator never lands.** No broker credential lets any remote worker bypass the local verifier or auto-land to base. The only base-branch land remains a gated `policy.GateAction{PromoteToBase}` through the human approver (`internal/policy/gateaction.go:25-30,86-94`; nil approver default-denies), and the Integrator never pushes (`internal/integrate/integrate.go:12-17`).
5. **Dependency budget justified (`I6`); default-off, byte-identical.** A broker client is a *new module surface* (`I6`) — it is justified in **both** PR and CHANGELOG, **prefers stdlib `net/http`** over any vendor SDK, and must keep `CGO_ENABLED=0` across the release matrix (§8). The default `nilcore` binary is **byte-identical** with the broker absent — the per-host backends (`keychain`/`file`/`env`) remain the default (§9).

> A **security review of the broker design** (lease scoping, rotation, revocation, the EXT-01 credential-proxy interplay, the redaction-on-log path) is a §0 sub-gate — mandated by the same posture EXT-01 requires for its credential proxy (`docs/ROADMAP-EXTERNAL-INFRA.md:53`). If any §0 item cannot be met, EXT-06 stays on the roadmap, unbuilt.

---

## §1 Summary

**What it is.** A central secret **broker** — HashiCorp Vault, a cloud KMS/secret-manager (AWS Secrets Manager / GCP Secret Manager / Azure Key Vault), or systemd-creds-over-a-control-plane — that hands each fleet worker a **scoped, short-lived, least-privilege lease** instead of a long-lived static key. It is the secret-distribution substrate beneath the `EXT-01` managed fleet and the `EXT-05` enterprise control plane (`docs/ROADMAP-EXTERNAL-INFRA.md:148`).

**The one thing that does not change.** The `secrets.SecretStore` interface — `Get/Set/Delete/Name` (`internal/secrets/secrets.go:19-24`) — is **untouched**. EXT-06 is built **entirely** by extending the already-existing `ExternalStore` hook (`internal/secrets/external.go:10-18`), which the comment there names as designed "for corporate secret managers (Vault, cloud KMS wrappers, etc.)." That hook is the seam; this is what it was built for. Everything in this plan is a **new leaf** (`internal/secrets/broker/`) or an **additive seam** in `cmd/nilcore` wiring — `secrets.go`, `env.go`, `keychain.go`, `file.go` are read-only references, never edited.

**The product line.** NilCore does not make centralized secrets safe by trusting the broker more. It makes them safe by **shrinking what any one lease can do and how long it lives** — least-privilege scope, a TTL measured in the lifetime of one run, host-side-only handling, and a redaction fence that holds even when the broker speaks. A worker (or a prompt-injected model inside it) that captures a lease captures a *narrow, already-expiring* capability, never the fleet's master credential. The model never sees the lease, ever (`docs/ARCHITECTURE.md:120-121`).

---

## §2 As-is

The single most important fact for this plan: **the broker seam is not net-new.** `internal/secrets/external.go` already exists, already implements `SecretStore`, and already shells to a user-configured command for exactly this purpose. EXT-06 reuses and extends it; it never rebuilds the secret boundary.

### 2.1 The shipped secret spine (reuse, do not rebuild)

| Package / symbol | What it gives EXT-06 |
|---|---|
| `secrets.SecretStore` (`internal/secrets/secrets.go:19-24`) | the credential boundary: `Get(name)`/`Set(name,value)`/`Delete(name)`/`Name()`. "Implementations must never log or otherwise expose a secret value; error messages reference the secret name only" (`secrets.go:17-18`). **Frozen for EXT-06 — the broker satisfies it, does not change it.** |
| `secrets.ExternalStore` (`internal/secrets/external.go:10-63`) | **THE seam.** Delegates `get`/`set`/`delete` to a configured command; "the value never appears in argv" (`external.go:14`) — get reads from stdout (`external.go:24-30`), set passes on stdin (`external.go:32-38`). Designed verbatim "for corporate secret managers (Vault, cloud KMS wrappers, etc.)" (`external.go:11`). EXT-06's broker store is the in-process Go analogue of this shell hook. |
| `secrets.EnvStore` / `KeychainStore` / `FileStore` (`env.go:8-13`, `keychain.go:13-15`, `file.go:25-29`) | the **per-host backends that remain the default** (§9). Read-only env; OS keychain; AES-256-GCM 0600 vault for headless hosts. EXT-06 adds a fourth backend **beside** these, never instead of them. |
| `secrets.Detect()` (`internal/secrets/secrets.go:30-35`) | the zero-config auto-pick (keychain → env). **Unchanged** — it never returns the broker; the broker is opt-in via explicit config, exactly as `FileStore`/`ExternalStore` are constructed directly today (`secrets.go:27-29`). |
| `chainStore` (`cmd/nilcore/main.go:1599-1660`) | the existing composition pattern: an ordered list of `SecretStore`s tried in turn (`Get` walks `stores`, first hit wins, `main.go:1639-1661`). **This is where the broker store is prepended when configured** — no new composition mechanism needed. |
| `provider.ResolveWith(spec, getenv)` (`internal/provider/provider.go:22-26`) | resolves a `provider:model` spec to a `model.Provider` using an **injected** key lookup (`getenv`), proven by `TestResolveWith` (`provider_test.go:145-162`). The fleet's per-run credential resolution already flows through an injectable seam — the broker lease feeds it, no contract change. |
| `policy.GateAction{PromoteToBase}` (`internal/policy/gateaction.go:25-30`) + the never-land Integrator (`internal/integrate/integrate.go:12-17,91`) | the gate EXT-06 must never let a broker credential bypass (§7, I2). |

### 2.2 What is genuinely new (the only code EXT-06 writes)

A single new leaf package `internal/secrets/broker/` (a `BrokerStore` satisfying `SecretStore` over stdlib `net/http`, a lease cache with TTL + rotation + revocation, a redaction-audited error path), an additive `onboard.Config.Broker` config field, and the `cmd/nilcore` wiring that prepends the broker store onto the existing `chainStore` when configured. Every piece is a new leaf or an additive seam; `secrets.go`/`env.go`/`keychain.go`/`file.go`/`external.go` and the frozen `backend.go` are untouched.

---

## §3 The architecture

The organizing principle: **the broker is a fourth `SecretStore` backend that returns *leases* instead of static values; the lease — not the key — is what reaches the fleet; the model is outside the boundary in every case.** This is the architecture the codebase already committed to in `external.go`; EXT-06 is the typed, in-process, lease-aware driver over that seam.

```
                          EXT-01 control plane assigns shard → host/worker  (scope = run/role/tenant)
                                                   │
                              ┌────────────────────┴───────────────────────┐
                              │  buildBrokerStore (cmd/nilcore wiring)      │  reads onboard.Config.Broker
                              └────────────────────┬───────────────────────┘
                                                   ▼
   secrets.Detect()/chainStore  ──►  [ BrokerStore , (keychain|file) , EnvStore ]   (broker PREPENDED only when configured)
                                                   │  (default-off ⇒ list is exactly today's per-host chain)
                                                   ▼
   BrokerStore.Get(name)  ──►  lease cache (TTL) ──hit?──► transient in-mem lease value (NEVER on disk)
                                       │ miss / expired
                                       ▼
   broker.Client.Lease(ctx, Scope{Run,Role,Tenant,Name}, TTL)  ──stdlib net/http──►  Vault / cloud-KMS
                                       │   (Bearer broker-auth header set host-side; renewable; revocable)
                                       ▼
   short-lived scoped lease  ──►  provider.ResolveWith(spec, getenv=broker-backed)  /  box.ExecWithEnv(per-run env)
                                       ▼
                          never a prompt, a message, model context, a log line, or a disk file  (I3)
                                       │
                          run ends / ctx cancels ──► BrokerStore.Revoke(leaseID)  (best-effort) ; cache zeroed
```

### 3.1 Extend `ExternalStore` to front Vault / cloud-KMS

`ExternalStore` already fronts a broker **via a shell command** (`external.go:48-63`): `Command Args... <op> <name>`, value on stdin/stdout, never in argv. EXT-06 adds the **in-process Go** equivalent so the fleet does not fork a CLI per secret on every worker:

- `internal/secrets/broker.BrokerStore` implements `SecretStore` (`var _ secrets.SecretStore`). `Get(name)` returns the **current lease value** for `name` (fetching/renewing as needed); `Set`/`Delete` either proxy to the broker's write API **or** return `secrets.ErrReadOnly` (the EXT-01 fleet posture is read-only distribution — workers consume, an operator provisions out-of-band; the choice is a config flag, default read-only).
- The transport is a thin `broker.Client` interface (`Lease`/`Renew`/`Revoke`) with **two stdlib implementations**: `VaultClient` (Vault's `/v1/...` HTTP+JSON API) and `KMSClient` (a cloud secret-manager REST API). Both speak `net/http` + `encoding/json` only (§8). The shell `ExternalStore` remains the **escape hatch** for any broker without a built-in client (systemd-creds, a bespoke wrapper) — zero new code, already shipped.

### 3.2 Short-lived, scoped, least-privilege per-worker leases

- **Scope.** A lease is requested for a `Scope{RunID, Role, Tenant, Name}` — the narrowest grant the EXT-01 shard / EXT-05 RBAC role needs. The broker policy (Vault role, KMS IAM binding) is configured **out-of-band by the operator** to map `(role,tenant)` → the minimal secret set. NilCore requests; the broker enforces least-privilege. A worker for shard X never receives a lease scoped to shard Y or to a secret its role does not need.
- **TTL.** Leases are short-lived — TTL measured in *the lifetime of one run/shard*, not days. The default TTL is the run deadline (or a configured ceiling, whichever is smaller). A long run **renews** (`Client.Renew`) rather than holding a long-lived grant; renewal is bounded by a `MaxTTL` after which the lease is dead and the run must re-lease (forcing the broker to re-authorize).
- **Least-privilege.** The lease carries only the secrets the scope demands; the model sees none of them; the host injects each per-run via the **existing** paths (provider auth header, `box.ExecWithEnv` env at spawn, `docs/SECRETS.md:54-61`).

### 3.3 Lease rotation & revocation

- **Rotation.** A `leaseCache` keyed by `(Scope,Name)` holds the value transiently in process memory with its expiry. A background renewer (one goroutine, ctx-bound) renews before expiry up to `MaxTTL`; on the broker rotating the underlying secret, the next `Renew`/`Lease` returns the new value transparently — callers always read "current." No rotation event ever touches disk or a log payload.
- **Revocation.** On run end / ctx cancel / a detected compromise signal, `BrokerStore.Revoke(leaseID)` calls the broker's revoke API (best-effort; a failure is logged *by lease-id only, never the value*) **and** zeroes the cache entry. The narrow scope + short TTL mean an un-revoked lease self-expires fast — revocation is defense-in-depth, not the only guard.

### 3.4 How it composes with the EXT-01 credential proxy

EXT-01 (`docs/ROADMAP-EXTERNAL-INFRA.md:49,59`) puts a **credential proxy** between a remote (possibly prompt-injected) worker and the *real* push token: the worker holds a scoped in-sandbox token, the proxy translates it to the real one and restricts pushes to the working branch. EXT-06 is the **upstream** of that proxy:

- The broker mints the **scoped lease**; the EXT-01 proxy is what the lease *authorizes the worker to talk to* for the one irreversible-adjacent capability (git push to the working branch). The two compose cleanly: broker = "what credential, how narrow, how long"; proxy = "even with that credential, what action is allowed, on what branch."
- A captured lease therefore yields at most: a narrow secret, already expiring, whose only push path runs through a proxy that refuses anything but the working branch — and base-branch land still requires the human gate (`gateaction.go:116-123`). **No single compromise reaches the fleet master credential or the base branch.**
- The lease is **never** handed to the model; the proxy token is **never** handed to the model. Both live host-side, injected per-run via `box.ExecWithEnv` / the provider auth header (`docs/SECRETS.md:54-61`).

### 3.5 The `SecretStore` interface unchanged

`BrokerStore` is *only* a `SecretStore`. It adds no method to the interface. Lease-specific operations (`Lease`/`Renew`/`Revoke`, scope) live on the concrete `BrokerStore` / `broker.Client`, reached by the wiring that constructs it — exactly as `FileStore.OpenFileVault` / `ExternalStore{Command,Args}` carry construction-time config the interface never sees (`secrets.go:27-29`). Every existing caller that holds a `SecretStore` (`chainStore`, `provider.ResolveWith`'s `getenv`, the meter/provider stacks) works unchanged. This is the load-bearing reuse: the entire fleet already talks to secrets through `Get(name)`, so fronting that with a broker is invisible above the seam.

---

## §4 The task DAG

**Namespace `EXT-06-T01 … EXT-06-T10`.** One task = one branch (`task/EXT-06-T0x`) = one PR. Owns sets are pairwise **disjoint** (package dir / single file = unit of ownership). The broker leaf + client foundation (T01–T04) lands **before** the wiring + composition tasks (T05–T08), per the same foundation-before-orchestration discipline as `docs/SWARM.md` §10. T00 is the §0 promotion (serialized contract); it gates everything.

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-06-T00 | §0 gate promotion + security-review record | EXT-01, EXT-05 past gate | `docs/TASKS.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `docs/SECRETS.md`, `CLAUDE.md` | **contract (docs) — serialized; the gate itself** |
| EXT-06-T01 | `broker.Client` interface + `Scope`/`Lease` types + lease cache | T00 | `internal/secrets/broker/client.go`, `lease.go`, `*_test.go`, `deps_test.go` | new leaf; no transport yet |
| EXT-06-T02 | `BrokerStore` (`SecretStore` over a `broker.Client`) + redaction-audited errors | T01 | `internal/secrets/broker/store.go`, `store_test.go` | new file in the leaf (serial after T01) |
| EXT-06-T03 | `VaultClient` (stdlib `net/http` Vault transport) | T01 | `internal/secrets/broker/vault.go`, `vault_test.go` | new file; ∥ T04 |
| EXT-06-T04 | `KMSClient` (stdlib `net/http` cloud secret-manager transport) | T01 | `internal/secrets/broker/kms.go`, `kms_test.go` | new file; ∥ T03 |
| EXT-06-T05 | Lease rotation/renewal/revocation lifecycle + background renewer | T02 | `internal/secrets/broker/lifecycle.go`, `lifecycle_test.go` | new file (serial after T02) |
| EXT-06-T06 | `onboard.Config.Broker` field + Validate clause | T01 | `internal/onboard/onboard.go`, `onboard_test.go` | **contract (config schema) — serialized** |
| EXT-06-T07 | redaction extension: broker lease values into the log/output scrubber set | T02 | `internal/redact/` (or the existing scrub seam), `*_test.go` | additive; sole owner for its duration |
| EXT-06-T08 | wiring: prepend `BrokerStore` onto `chainStore`; per-run lease lifecycle bound to run ctx | T03, T04, T05, T06, T07 | `cmd/nilcore/secrets_broker.go` (new), `cmd/nilcore/main.go` (assembleStore hook) | **serialized cmd-wiring** |
| EXT-06-T09 | EXT-01 credential-proxy interplay test + EXT-05 scope-key binding | T08, EXT-01 proxy, EXT-05 RBAC | `internal/secrets/broker/interplay_test.go`, wiring test in `cmd/nilcore` | composition proof; serial after T08 |
| EXT-06-T10 | Docs + CHANGELOG promotion (Phase 13 / EXT-06) | T09 | `docs/ARCHITECTURE.md`, `docs/SECRETS.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs) — serialized last** |

> **Correction folded in:** `internal/secrets/broker` is the **whole-package** Owns unit for T01/T02/T05 (sibling files in one dir), so the work-selection rule forbids two of them open at once — they form one serialized sub-chain (T01 → T02 → T05). T03/T04 are *also* sibling files in that dir; to keep them parallel they are declared **per-file** Owns with an explicit note that they may only land after T01 defines the shared `Client` interface and must not edit `client.go`/`lease.go` — the same per-file discipline `docs/SWARM.md` §10 uses for `packs.go`. If the parallel-agent protocol's "package = unit of ownership" must be honored strictly, fold T03+T04 into one task `EXT-06-T03 transports (vault+kms)` and the chain becomes T01 → T03 → T02 → T05 (the safer reading; the wave map §6 shows both).

---

## §5 Per-task specs

#### EXT-06-T00 — §0 gate promotion + security-review record · contract (docs), the gate itself
- **Goal:** record the human thesis decision, the EXT-01/EXT-05 prerequisite status, and the security-review sign-off; promote EXT-06 from this roadmap into `docs/TASKS.md` as an eligible phase. **This task IS the gate** (`docs/ROADMAP-EXTERNAL-INFRA.md:13-21`); no T01+ may begin until it merges.
- **Depends on:** EXT-01 and EXT-05 past their own §0 gates (`docs/ROADMAP-EXTERNAL-INFRA.md:154`).
- **Owns:** `docs/TASKS.md` (new Phase rows + the T01–T10 specs), `docs/ROADMAP-EXTERNAL-INFRA.md` (mark EXT-06 promoted; keep the boundary line), `docs/SECRETS.md` (a "centralized broker (EXT-06)" subsection placeholder), `CLAUDE.md` (repository-map line if a new pkg dir is added).
- **Acceptance:** the PR body records (a) the named human owner + the recorded thesis decision; (b) proof EXT-01/EXT-05 are past gate; (c) the security-review outcome covering lease scope/TTL/rotation/revocation/redaction/proxy-interplay; (d) the §7 invariant ledger restated; (e) the §8 module justification. `docs/TASKS.md` carries disjoint Owns for T01–T10. No code change.
- **Verify:** `make verify` (docs don't break the build); markdown-format pass; manual review that every §0 bullet is recorded.
- **Notes:** **serialized — contract files.** If any §0 item is unmet, this PR is **not opened**; EXT-06 stays unbuilt.

#### EXT-06-T01 — `broker.Client` interface + `Scope`/`Lease` types + lease cache
- **Goal:** the stdlib-only foundation: a transport-agnostic `Client` interface, the `Scope`/`Lease` value types, and a concurrency-safe TTL `leaseCache` — **no network, no `SecretStore` yet**, so the leaf is independently `make verify`-green.
- **Depends on:** EXT-06-T00.
- **Owns:** `internal/secrets/broker/` (opens the package): `client.go`, `lease.go`, `cache_test.go`, `deps_test.go`.
- **Acceptance:** `Client interface { Lease(ctx, Scope, ttl time.Duration) (Lease, error); Renew(ctx, Lease) (Lease, error); Revoke(ctx, leaseID string) error }`; `Scope{RunID, Role, Tenant, Name string}` (the narrowest grant; never carries a value); `Lease{ID string; value string (UNEXPORTED — never marshaled, never logged); Expiry time.Time; Renewable bool}` with a `Lease.String()`/`MarshalJSON` that emits **only** `id`/`expiry`/`scope` — **never the value** (compile-time guard: a test asserts `json.Marshal(lease)` contains neither the value nor `"value"`); a concurrency-safe `leaseCache{Get(key)(Lease,bool); Put(Lease); Expire(key); Zero()}` keyed by `(Scope,Name)`, returning miss on expiry; `Zero()` overwrites the in-mem value before drop (best-effort scrub).
- **Verify:** `make verify`; `go test -race ./internal/secrets/broker/`: cache hit/miss/expiry under -race (50 goroutines); `MarshalJSON`/`String` emit no value and no `"value"` key; `deps_test.go` runs `go list -deps` and asserts the leaf imports **only** stdlib + `internal/secrets` (the interface type) — **no** `net/http` yet, no `super`/`agent`/`project`, no `eventlog` (the leaf never logs; the wiring does, redacted).
- **Notes:** the `value` field is **unexported** by construction so it cannot leak through reflection-based marshaling or a struct dump (I3). No module (I6).

#### EXT-06-T02 — `BrokerStore` (`SecretStore` over a `broker.Client`) + redaction-audited errors
- **Goal:** the fourth backend — `BrokerStore` implements `secrets.SecretStore` (`var _ secrets.SecretStore`) over an injected `broker.Client` + the T01 cache, with the `secrets.go:17-18` error discipline (name-only, never the value).
- **Depends on:** EXT-06-T01. **Owns:** `internal/secrets/broker/store.go`, `store_test.go` (serial after T01 — same package dir).
- **Acceptance:** `BrokerStore{Client broker.Client; Scope Scope; cache *leaseCache; ReadOnly bool}`; `Name()` returns `"broker"`; `Get(name)` resolves `(Scope+name)` via cache-then-`Client.Lease`, returns the transient value (held only in the returned string + the cache, **never written to disk**); `Set`/`Delete` proxy to the broker write API when `ReadOnly==false`, else return `secrets.ErrReadOnly` (default `ReadOnly:true` — the fleet-distribution posture); **every error wraps the secret *name* only** (`fmt.Errorf("broker secret %q: %w", name, ...)`), mirroring `external.go:27`; a `Get` miss returns `secrets.ErrNotFound`; a transport error returns a sentinel that the wiring redacts (never the broker response body, which could echo a value).
- **Verify:** `make verify`; `go test -race ./internal/secrets/broker/`: `var _ secrets.SecretStore = (*BrokerStore)(nil)`; `Get` with a fake `Client` returns the leased value; cache short-circuits a second `Get` (fake records one `Lease` call); `ReadOnly:true` ⇒ `Set`/`Delete` return `ErrReadOnly`; a `Client` error path's `error.Error()` contains the **name** but **never** the value (table test with a value-shaped string); miss ⇒ `ErrNotFound`.
- **Notes:** this is the in-process analogue of `ExternalStore` (`external.go:10-63`) — same boundary, same name-only errors, no fork-per-secret.

#### EXT-06-T03 — `VaultClient` (stdlib `net/http` Vault transport)
- **Goal:** a `broker.Client` over HashiCorp Vault's HTTP API using **only** `net/http` + `encoding/json` (§8).
- **Depends on:** EXT-06-T01 (the `Client` interface). **Owns:** `internal/secrets/broker/vault.go`, `vault_test.go` (per-file Owns; must not edit `client.go`/`lease.go`; ∥ T04).
- **Acceptance:** `VaultClient{Addr string; auth func() string; HTTP *http.Client}` — `Lease`/`Renew`/`Revoke` call `POST {Addr}/v1/...` with the **broker-auth header set host-side** (`X-Vault-Token` from a `func() string` resolver, *never* a string field, *never* logged); JSON request/response parsed with `encoding/json`; the leased secret is read from the response **into the unexported `Lease.value`** and the response body is **not** returned to callers; an `http.Client` with a sane `Timeout` + ctx-honoring requests; a non-2xx returns a sentinel error that includes the **status code only**, never the body.
- **Verify:** `make verify`; `go test -race ./internal/secrets/broker/`: an `httptest.Server` scripting Vault lease/renew/revoke JSON → value lands in `Lease`, never in the returned error; auth header is set on every request (asserted by the test server) and never appears in any log/error captured by the test; ctx-cancel aborts the request; non-2xx ⇒ status-only error.
- **Notes:** stdlib HTTP, no Vault SDK (§8). The token resolver is `func() string` so the broker-auth credential itself flows through the **same** SecretStore boundary (it is `secrets.Get("NILCORE_BROKER_TOKEN")`), never a literal in config (I3).

#### EXT-06-T04 — `KMSClient` (stdlib `net/http` cloud secret-manager transport)
- **Goal:** a second `broker.Client` over a cloud secret manager's REST API (AWS Secrets Manager / GCP Secret Manager / Azure Key Vault) using **only** `net/http` + `encoding/json` — same posture as T03, different endpoint shape + signing.
- **Depends on:** EXT-06-T01. **Owns:** `internal/secrets/broker/kms.go`, `kms_test.go` (per-file Owns; must not edit `client.go`/`lease.go`; ∥ T03).
- **Acceptance:** `KMSClient{Endpoint string; sign func(*http.Request) error; HTTP *http.Client}` — the request **signer** is injected (`func(*http.Request) error`) so the AWS SigV4 / GCP-OAuth / Azure-AAD signing is a host-side closure fed by the SecretStore, not a vendor SDK and not a key in config; `Lease`/`Renew`/`Revoke` map to the manager's get-secret-value / rotate / (no-op or delete-version) calls; value into the unexported `Lease.value`; response body never returned; status-only errors; ctx-honoring; sane timeout.
- **Verify:** `make verify`; `go test -race ./internal/secrets/broker/`: `httptest.Server` scripting the manager's JSON → value lands in `Lease`, never the error; the signer is invoked per request (asserted); ctx-cancel aborts; non-2xx ⇒ status-only.
- **Notes:** the signer-as-closure keeps `I6` (no AWS/GCP/Azure SDK module) and `I3` (the signing key flows through SecretStore). If full SigV4 in stdlib is judged too large, the documented fallback is the **shell `ExternalStore`** wrapping the vendor CLI (`external.go`, zero new code) — recorded in the §8 module-justification table, not silently pulling an SDK.

#### EXT-06-T05 — Lease rotation/renewal/revocation lifecycle + background renewer
- **Goal:** the lease lifecycle on top of `BrokerStore`: a single ctx-bound background renewer that renews before expiry up to `MaxTTL`, transparent rotation, and `Revoke`-on-end with cache-zero.
- **Depends on:** EXT-06-T02. **Owns:** `internal/secrets/broker/lifecycle.go`, `lifecycle_test.go` (serial after T02 — same package dir).
- **Acceptance:** `LeaseManager{store *BrokerStore; TTL, MaxTTL time.Duration}` with `Start(ctx) (stop func())`; the renewer renews each cached lease at `Expiry - renewSkew`, stops renewing past `MaxTTL` (the lease dies → next `Get` re-leases, forcing broker re-authorization); a broker-rotated underlying secret surfaces transparently on the next renew (callers always read current); `Revoke(ctx)` revokes all live leases best-effort (a failure logged **by lease-id only**) and zeroes the cache; ctx-cancel triggers `Revoke` + stop; no value ever written to disk or passed to a log payload.
- **Verify:** `make verify`; `go test -race ./internal/secrets/broker/`: fake `Client` with a clock → renew fires before expiry; past `MaxTTL` no further renew (re-lease observed); rotation (fake returns a new value on renew) surfaces to the next `Get`; `Revoke` calls the fake's revoke for every live lease and zeroes the cache (a value-shaped probe is gone after `Zero`); ctx-cancel path revokes; **-race** clean under a renewer + concurrent `Get`s.
- **Notes:** one goroutine, ctx-bound — the renewer is owned by the run, dies with it (mirrors `docs/SWARM.md` §13 "resume is local-restart only" posture: no standing daemon holding leases across runs).

#### EXT-06-T06 — `onboard.Config.Broker` field + Validate clause · contract (config schema)
- **Goal:** additively extend `onboard.Config` with one optional `Broker *BrokerConfig` (`json:"broker,omitempty"`) + a Validate clause, v1-compatible under `DisallowUnknownFields`.
- **Depends on:** EXT-06-T01 (for the `Scope`/kind types). **Owns:** `internal/onboard/onboard.go`, `onboard_test.go`.
- **Acceptance:** `BrokerConfig{Kind string (vault|kms|external); Addr string; AuthSecretName string; TTL, MaxTTL string; ReadOnly bool; Scope (RunID/Role/Tenant templated, not literal secrets)}`; default-zero so every existing config parses unchanged; `Validate()` gains a broker clause (kind ∈ {vault,kms,external}; `AuthSecretName` non-empty and is a **name**, never a value; TTL ≤ MaxTTL; loud error otherwise — the value-shaped-field check rejects a config that put a secret literal where a name belongs); old configs without `broker` parse; a config with `broker` round-trips parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip with `Broker` set; old config parses under strict decode; `Validate` rejects unknown kind / TTL>MaxTTL / a literal in `AuthSecretName`.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface), treated as a contract surface (same posture as `docs/SWARM.md` SW-T08). Config holds **references, not secrets** (`docs/SECRETS.md:70-74`) — `AuthSecretName` is a name resolved through SecretStore, never the broker token itself. `onboard → broker` is downward (no cycle).

#### EXT-06-T07 — redaction extension: broker lease values into the log/output scrubber set
- **Goal:** ensure a broker lease value, if it ever reaches a log line or tool/command output, is scrubbed before write/before-context — the same defense-in-depth as `docs/SECRETS.md:64-68` (output redaction + log redaction P2-T06), now covering broker-sourced values.
- **Depends on:** EXT-06-T02. **Owns:** the existing redaction seam (`internal/redact/` or wherever P2-T06's log redaction + the output scrubber live — **sole owner for its duration**), `*_test.go`.
- **Acceptance:** the scrubber's "known stored values" set is fed broker lease values **transiently** (held only as long as the lease is live; dropped on revoke) so an accidental `env` dump or a broker error body cannot leak a leased value to the model context or the append-only log (I3, I5); the feed is push-based from the `LeaseManager` (no new import cycle — the scrubber exposes a `Register(value)`/`Forget(value)` seam, the broker calls it); a test proves a log/output containing a leased value is redacted, and a revoked lease's value is `Forget`-ten.
- **Verify:** `make verify`; `go test ./...` on the redaction package: a leased-value-bearing output → redacted; after `Forget`, the value is no longer in the active set (no unbounded growth); existing redaction tests stay green.
- **Notes:** additive; closes the "central distribution multiplies blast radius" path (`docs/ROADMAP-EXTERNAL-INFRA.md:152`) at the *log/context* boundary specifically. The scrubber never persists the value — it holds it transiently exactly as the SecretStore does (`secrets.go:3-6`).

#### EXT-06-T08 — wiring: prepend `BrokerStore` onto `chainStore`; per-run lease lifecycle · serialized cmd-wiring
- **Goal:** the operator front door — construct `BrokerStore` from `onboard.Config.Broker`, **prepend** it onto the existing `chainStore` (`main.go:1599-1660`) **only when configured**, bind the `LeaseManager` lifecycle to the run ctx, and resolve the broker-auth token through the **rest** of the chain (keychain/file/env) so the broker credential itself never bypasses the boundary.
- **Depends on:** EXT-06-T03, T04, T05, T06, T07. **Owns:** `cmd/nilcore/secrets_broker.go` (new — `buildBrokerStore`), `cmd/nilcore/main.go` (one additive hook inside `assembleStore`, `main.go:1599`).
- **Acceptance:** `buildBrokerStore(cfg.Broker, baseChain) (secrets.SecretStore, *broker.LeaseManager, error)` selects `VaultClient`/`KMSClient`/`ExternalStore` by `Kind`; the broker-auth token resolves via `baseChain.Get(cfg.Broker.AuthSecretName)` (the **per-host** chain — the broker credential is itself a per-host secret, never a literal); `assembleStore` **prepends** `BrokerStore` to `stores` **iff** `cfg.Broker != nil`, else the chain is **byte-identical to today** (§9); the `LeaseManager.Start(runCtx)` is wired so leases are revoked on run end/cancel; `provider.ResolveWith(spec, chain.Get)` is unchanged (the broker is invisible above `Get`); **no** other arm or shared helper edited.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, fake `broker.Client` + `httptest` if needed): `cfg.Broker == nil` ⇒ `assembleStore` returns exactly today's chain (assert store list identity / order unchanged); `cfg.Broker != nil` ⇒ `BrokerStore` is `stores[0]`, `Get` hits the broker first then falls through; the broker-auth token is fetched from the **base** chain (not from the broker — no chicken-and-egg); run-ctx cancel revokes leases; an import-graph test asserts no existing package imports `internal/secrets/broker` except the cmd wiring + `internal/onboard` (for the config type).
- **Notes:** **serialized cmd-wiring** — the only task editing `main.go`. The default binary is byte-identical when `broker` is unconfigured (one additive `if cfg.Broker != nil` branch; all logic in the new file) (§9).

#### EXT-06-T09 — EXT-01 credential-proxy interplay test + EXT-05 scope-key binding
- **Goal:** prove the composition (§3.4): a broker lease feeds the EXT-01 credential proxy (worker holds a scoped lease → proxy restricts push to the working branch → base land still gated), and the EXT-05 RBAC role/tenant maps to the lease `Scope`.
- **Depends on:** EXT-06-T08, plus the EXT-01 credential-proxy surface and the EXT-05 RBAC scope keys (both past gate). **Owns:** `internal/secrets/broker/interplay_test.go`, a wiring test in `cmd/nilcore`.
- **Acceptance:** a hermetic test where a fake fleet worker receives a `Scope{RunID,Role,Tenant}`-scoped lease, the (fake) EXT-01 proxy translates it and **refuses** a push to anything but the working branch (`docs/ROADMAP-EXTERNAL-INFRA.md:59`), and a base-branch land is refused absent a `policy.GateAction{PromoteToBase}` through a human approver (`gateaction.go:114-123`; nil approver default-denies); a test mapping an EXT-05 `(role,tenant)` to a `Scope` proves a role gets **only** its least-privilege secret set; a "captured lease" test proves an expired/revoked lease is unusable.
- **Verify:** `make verify`; `go test -race ./...` on the interplay surfaces: scope isolation (role A's lease never resolves role B's secret); proxy-restricts-push; gate-required-for-land; lease-expiry-makes-capture-useless.
- **Notes:** this is the **security-review-in-code** of §3.4. It is the proof that blast radius is bounded (§10).

#### EXT-06-T10 — Docs + CHANGELOG promotion · contract (docs), serialized last
- **Goal:** promote EXT-06 into the canonical docs and ledger.
- **Depends on:** EXT-06-T09. **Owns:** `docs/ARCHITECTURE.md`, `docs/SECRETS.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md`.
- **Acceptance:** `docs/SECRETS.md` a "Centralized broker (EXT-06)" section (the fourth backend; leases scoped+short-lived+least-privilege; rotation/revocation; the SecretStore interface unchanged; the model never sees a lease); `docs/ARCHITECTURE.md` a layer-map row for `internal/secrets/broker` with its import set (`net/http`/`encoding/json`/`time`/`sync` + `internal/secrets` — **never** the orchestrator) + the restated I3 boundary; `docs/ROADMAP-EXTERNAL-INFRA.md` mark EXT-06 shipped, keep the EXT-01/EXT-05 dependency line and the "do not build standalone" note; `CLAUDE.md` one repository-map line for the new pkg dir; `CHANGELOG.md` one `## [Unreleased]` line per merged EXT-06-T0x; `README.md` a default-off note + the honest caveats (broker is opt-in; per-host backends remain the default; the model never sees a lease).
- **Verify:** `make verify` (docs don't break the build); markdown-format pass; manual review that the layer-map import set matches `go list -deps` of the leaf.
- **Notes:** **serialized — contract files.** Lands last.

---

## §6 Parallel wave map & critical path

A fleet executes in ordered **waves**; every task in a wave has all deps merged to `main` and a pairwise-disjoint Owns set. `internal/secrets/broker` is largely **one package = one Owns unit**, so its files form a serialized sub-chain (the same intra-package serialization `docs/SWARM.md` §11 documents). T03/T04 are the one parallel pair *inside* the package, held by **per-file** Owns with a "don't touch the shared interface files" note (the conservative fold is shown too).

```
WAVE 0  (1 — THE GATE, serialized contract)
  └── EXT-06-T00  docs/* promotion + security-review record   (EXT-01, EXT-05 past gate)

WAVE 1  (1 — opens the leaf)
  └── EXT-06-T01  internal/secrets/broker/{client,lease,cache}   (T00)

WAVE 2  (2 concurrent — transports, per-file Owns over the T01 interface)
  ├── EXT-06-T03  internal/secrets/broker/vault.go   (T01)
  └── EXT-06-T04  internal/secrets/broker/kms.go     (T01)
        (conservative fold: one task EXT-06-T03 transports(vault+kms) → wave is size 1)

WAVE 3  (2 concurrent)
  ├── EXT-06-T02  internal/secrets/broker/store.go        (T01)   ← BrokerStore (SecretStore)
  └── EXT-06-T06  internal/onboard/onboard.go             (T01)   ← SERIAL pt: config-schema contract

WAVE 4  (2 concurrent)
  ├── EXT-06-T05  internal/secrets/broker/lifecycle.go    (T02)   ← serial after T02 (same pkg dir)
  └── EXT-06-T07  internal/redact/ (scrubber seam)        (T02)   ← sole redaction owner

WAVE 5  (1 — SERIAL pt: cmd-wiring)
  └── EXT-06-T08  cmd/nilcore/{secrets_broker.go,main.go}  (T03,T04,T05,T06,T07)   ← sole main.go editor

WAVE 6  (1 — composition proof)
  └── EXT-06-T09  interplay test (EXT-01 proxy + EXT-05 scope)   (T08, EXT-01, EXT-05)

WAVE 7  (1 — SERIAL pt: docs contract)
  └── EXT-06-T10  docs/* + CLAUDE.md + README.md + CHANGELOG.md   (T09)
```

**Peak concurrency = 2** (waves 2, 3, 4). **Critical path (longest dependency chain) — 8 sequential merges:**

```
EXT-06-T00 → T01 → T02 → T05 → T08 → T09 → T10        (6 hops on the lifecycle line)
EXT-06-T00 → T01 → T03/T04 → T08 → T09 → T10          (the transport line)
```
The binding chain is **T00 → T01 → T02 → T05 → T08 → T09 → T10 (7 merges)**; T03/T04 and T06/T07 fold into the same waves without lengthening it.

**Serialization points (parallelism intentionally throttled to one writer):**
1. **The §0 gate** — EXT-06-T00 only; nothing starts before it merges. *(This is the EXT-specific serialization on top of the usual ones.)*
2. `internal/secrets/broker` package dir — T01 opens; T02/T05 serialize as sibling files; T03/T04 are the one per-file parallel pair (or folded).
3. `internal/onboard/onboard.go` — T06 only (config schema).
4. The redaction scrubber seam — T07 only.
5. `cmd/nilcore/main.go` — T08 only.
6. `docs/*` / `docs/SECRETS.md` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — T10 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; the broker sub-chain is strictly increasing IDs; `onboard → broker` and `cmd → broker` are downward. **Foundation-before-orchestration holds:** the wiring (T08) cannot compile until `BrokerStore` (T02) + a transport (T03/T04) + the config field (T06) exist.

---

## §7 Per-invariant ledger

The seven invariants hold **by reuse**, not by new mechanism — the broker is a fourth backend behind an unchanged interface.

| Invariant | How EXT-06 preserves it |
|---|---|
| **I1** frozen contract | `backend.CodingBackend.Run(ctx, Task) (Result, error)` and `Task`/`Result` are **untouched** (`internal/backend/backend.go`). A lease is resolved *before* a backend runs, through the unchanged `provider.ResolveWith` `getenv` seam (`provider.go:22-26`); the broker is invisible above `SecretStore.Get`. |
| **I2** verifier sole authority | No broker credential lets a remote worker self-certify or auto-land. The local verifier still governs every shard; the Integrator never pushes to base (`integrate.go:12-17,91`); base land is exactly **one** gated `policy.GateAction{PromoteToBase}` through the human approver (`gateaction.go:25-30,86-94`; nil ⇒ deny). T09 proves it. |
| **I3** *(in full, below)* | See the dedicated subsection — the entire point of EXT-06. |
| **I4** sandboxed execution | Leases are injected per-run via the existing `box.ExecWithEnv` env-at-spawn path (`docs/SECRETS.md:54-61`); a model-emitted command still runs in the sandbox; the broker store runs **host-side** (like the other `SecretStore` backends), never inside the sandbox, and never executes a model-emitted program. |
| **I5** append-only audit | Every lease grant/renew/revoke is recorded as a **metadata-only** event (lease-id + scope + expiry, **never the value**); the log is append-only and **redaction runs before write** (`docs/SECRETS.md:64-68`; T07 feeds the broker values into the scrubber). A broken chain still HALTS as today. |
| **I6** zero-dep core | The broker client is **stdlib `net/http` + `encoding/json`** — **no Vault/cloud SDK module** (§8). `CGO_ENABLED=0` holds across the release matrix. `go.mod` is **unchanged** (the whole point of preferring stdlib HTTP — same discipline as the hand-rolled PBKDF2, `file.go:230-232`). |
| **I7** untrusted-as-data | A broker *response body* is untrusted data — it is parsed for the lease value into an **unexported** field and **never** returned to a caller or echoed into a prompt; a broker error never carries the response body into context; the scope template (role/tenant) is operator-config, never model-authored. |
| **Never-land** | EXT-06 constructs **no** new `GateActionType` and never auto-lands. A captured lease's only push path is through the EXT-01 proxy (working-branch-only) and base land still needs the human gate (T09). |

### I3 — in full (the cardinal rule, amplified by central distribution)

`docs/ROADMAP-EXTERNAL-INFRA.md:152` states it plainly: **"`I3` is the entire point"** for EXT-06, because "central distribution multiplies the blast radius of a leak." So EXT-06 does not merely *preserve* I3 — it must keep the cardinal rule *more* strictly, since one broker now backs many hosts. The five clauses of the rule (`docs/ARCHITECTURE.md:120-121`; `docs/SECRETS.md:3-7`), each made concrete:

1. **The model never sees a key — ever.** The model sits **outside** the secret boundary (`docs/SECRETS.md:36-52`). It emits text and tool calls; *NilCore* attaches the lease when it makes a provider call (auth header) or spawns a sandboxed process (`box.ExecWithEnv` env-at-spawn). The lease is resolved through `SecretStore.Get(name)` host-side, fed to `provider.ResolveWith`'s injected `getenv` (`provider.go:22-26`), and **never** placed in a prompt, a message, model context, or a tool argument. `ProjectTrusted`-style fences (the swarm precedent, `docs/SWARM.md` §9 I7) ensure only key-free fields ever reach the model. A lease the model never sees is a lease it cannot leak, be tricked into printing, or exfiltrate via injection — and because the lease is *scoped + short-lived*, even a hypothetical out-of-band capture is a narrow, expiring capability, not the fleet master.
2. **Never on disk in plaintext.** The lease value lives **only** in transient process memory: an **unexported** `Lease.value` (T01 — cannot be marshaled, cannot be struct-dumped) inside a `leaseCache` that `Zero()`s on revoke (T05). Nothing is written to the worktree, the image, the config, or any file. The config holds **references, not secrets** (`docs/SECRETS.md:70-74`): `AuthSecretName` is a *name* resolved through the per-host chain, never the broker token. This matches the existing discipline — `FileStore` keeps plaintext off disk via AES-256-GCM (`file.go:20-24`); the broker keeps it off disk by *never persisting it at all*.
3. **Never logged.** Every `SecretStore` error references the **name only** (`secrets.go:17-18`; the broker mirrors `external.go:27`); transport errors carry a **status code only**, never the broker response body (T03/T04); lease lifecycle events are **metadata-only** (id/scope/expiry, T05/I5); and the log/output scrubber's "known stored values" set is fed live broker lease values (T07) so even an accidental `env` dump or a leaked broker body is redacted before it reaches the append-only log or model context (`docs/SECRETS.md:64-68`). `Lease.MarshalJSON`/`String` emit no value and no `"value"` key (T01, compile-time-guarded).
4. **Never in a prompt.** Re-stated because it is the failure mode that matters most for a fleet: the lease is resolved *after* the model decides what to do, *by the host*, *at the call/spawn boundary* — the model never authored, saw, or transported it. Untrusted broker output (a response body) is `guard.Wrap`-fenced as data, never instructions (I7).
5. **Never given to the model.** The complete statement of (1): the broker-auth token (`NILCORE_BROKER_TOKEN`), the lease, and the EXT-01 proxy token all live host-side and are injected per-run; none is ever a model input. The broker-auth token itself flows through the **rest** of the per-host chain (keychain/file/env) so the broker credential is bootstrapped from a per-host secret, never a literal in config, and never bypasses the boundary (T08).

**Blast-radius containment (the I3-specific bar EXT-06 must clear):** scope (least-privilege per role/tenant/run), TTL (one-run-lifetime, renewed not held, dead past MaxTTL), revocation (on run end/cancel/compromise), and host-side-only handling. A single compromised host or injected worker yields, at worst, a narrow secret that is already expiring and whose only write path is the working-branch-restricted EXT-01 proxy — never the fleet master credential, never the base branch.

---

## §8 Module justification

`I6` (`docs/ROADMAP-EXTERNAL-INFRA.md:18`; CLAUDE.md §2.6) requires every new module be justified in **both** the PR and the CHANGELOG and keep `CGO_ENABLED=0`. EXT-06's deliberate answer: **add no module.** The broker client is hand-rolled over stdlib, exactly as the codebase hand-rolls PBKDF2 to stay stdlib (`file.go:230-232`) and speaks MCP JSON-RPC over the standard library rather than vendoring a client (CLAUDE.md §2.6).

| Candidate dependency | Decision | Rationale |
|---|---|---|
| HashiCorp Vault Go SDK (`github.com/hashicorp/vault/api`) | **Rejected** | Vault's API is plain HTTP+JSON (`POST /v1/...`). `net/http` + `encoding/json` covers lease/renew/revoke (T03). The SDK pulls a large transitive tree; stdlib keeps `go.mod` unchanged and `CGO_ENABLED=0` trivially. |
| AWS / GCP / Azure secret-manager SDKs | **Rejected (primary path)** | Their secret-get/rotate endpoints are REST+JSON; the only non-trivial bit is request signing (SigV4 / OAuth / AAD), injected as a host-side `func(*http.Request) error` closure (T04) fed by the SecretStore. Avoids a multi-MB cloud SDK and its CGO-adjacent transitive deps. |
| A request-signing micro-lib | **Rejected** | SigV4 is a documented HMAC-SHA256 construction; if needed it is hand-rolled in stdlib (the PBKDF2 precedent). |
| (fallback) the **shell `ExternalStore`** wrapping a vendor CLI | **Already shipped, zero new code** (`external.go`) | If full in-stdlib signing for a given cloud is judged too large, the operator points `ExternalStore.Command` at the vendor CLI (`vault`, `aws secretsmanager get-secret-value`, etc.) — value on stdout, never in argv (`external.go:14`). This is the documented escape hatch, **not** an SDK import. |

**Net `go.mod` change: none.** Prefer stdlib HTTP over a vendor SDK, CGO-free — the §0 dependency-budget criterion is met by *not spending the budget at all*.

---

## §9 Default-off byte-identical proof

The default `nilcore` binary is **byte-identical** with the broker absent — the per-host backends remain the default (`docs/ROADMAP-EXTERNAL-INFRA.md:19`). Proven, not asserted:

1. **`secrets.Detect()` is unchanged** (`secrets.go:30-35`) — it still picks keychain → env and **never** returns the broker. The broker, like `FileStore`/`ExternalStore`, is constructed only on explicit config (`secrets.go:27-29`).
2. **`assembleStore` prepends the broker only when `cfg.Broker != nil`** (T08). With `broker` unconfigured the `stores` list is **exactly today's** chain (`main.go:1599-1660`) — same backends, same order, asserted by an identity test (`cfg.Broker==nil` ⇒ store list unchanged).
3. **No existing package imports `internal/secrets/broker`** except the cmd wiring (T08) + `internal/onboard` (for the config type) — an import-graph test asserts it (the `docs/SWARM.md` §9 default-off precedent). Merely linking the leaf cannot change behavior.
4. **The leaf has no `init()` with global side effects** — asserted by a test, so linking it is inert.
5. **`onboard.Config.Broker` is `omitempty` + default-zero** (T06) — every existing `config.json` parses unchanged under `DisallowUnknownFields`; a config with no `broker` key drives the identical per-host path.
6. **`SecretStore` is untouched** — every caller (`provider.ResolveWith`'s `getenv`, the meter/provider stacks, `chainStore`) behaves identically; the broker is invisible above `Get(name)`.

The default identity — "secrets are **per-host**" (`docs/ROADMAP-EXTERNAL-INFRA.md:36`) — is preserved exactly; EXT-06 is opt-in and reversible-to-remove (delete the `broker` config block ⇒ back to the per-host chain).

---

## §10 Risks

- **Blast radius (the headline risk).** A central broker backing many hosts means one compromise can, in the worst case, touch every host's secrets — the exact concern `docs/ROADMAP-EXTERNAL-INFRA.md:152` names. **Mitigation:** least-privilege scope (a host/role/tenant gets only its secrets, broker-policy-enforced), short TTL (one-run-lifetime, renewed not held, dead past MaxTTL), revocation on end/cancel/compromise, and host-side-only handling. The broker-policy mapping `(role,tenant)→secrets` is the operator's responsibility and a **security-review §0 item** (T00); NilCore requests the narrowest scope, the broker enforces it. A captured lease is a narrow, expiring capability — never the fleet master.
- **Lease compromise.** An injected/prompt-poisoned worker that captures its own lease. **Mitigation:** the lease was never the master (scope); it self-expires fast (TTL); the model never saw it (I3 §7); its only push path is the EXT-01 working-branch-restricted proxy; base land still needs the human gate. T09 proves an expired/revoked lease is unusable and a role's lease never resolves another role's secret. The redaction fence (T07) keeps a leaked value out of logs/context.
- **Broker-auth bootstrap (chicken-and-egg).** The broker needs a credential to authenticate to it. **Mitigation:** the broker-auth token (`NILCORE_BROKER_TOKEN`) is itself a **per-host** secret resolved through the *rest* of the chain (keychain/file/env) — never a literal in config, never fetched from the broker it authenticates (T08). This keeps the per-host backends load-bearing even with the broker on.
- **Credential-proxy interplay (the EXT-01 seam).** Two new authority surfaces (broker lease + proxy token) that must compose without a gap. **Mitigation:** §3.4's clean split (broker = what/how-narrow/how-long; proxy = what-action/what-branch) + T09's interplay test + the §0 security review. The risk is a *gap between* the two surfaces (e.g. a lease scoped wider than the proxy restricts, or a proxy that trusts a lease it shouldn't) — T09 asserts scope isolation, working-branch-only push, and gate-required-for-land specifically to close it. Because both EXT-01 and EXT-06 are gated and both require the human thesis decision, neither ships ahead of the other's review.
- **Renewer/daemon drift toward standing authority.** A background renewer could become a long-lived daemon holding leases across runs — a quiet thesis violation. **Mitigation:** the renewer is **ctx-bound to the run**, dies with it, and revokes on exit (T05) — the same "no standing daemon" posture as `docs/SWARM.md` §13 "resume is local-restart only." A `deps_test` keeps the leaf free of any server/listener.
- **Transport / SDK creep.** Pressure to pull a Vault/cloud SDK for convenience, breaking `I6`/`CGO_ENABLED=0`. **Mitigation:** §8's stdlib-only mandate + the shipped `ExternalStore` shell escape hatch; a `go.mod`-unchanged check at every merge.
- **Drift past the EXT-01 line.** A "central broker" is one step from "remote control plane." **Mitigation:** EXT-06 is gated *and* bound to EXT-01/EXT-05 (no standalone build); the broker store is a `SecretStore` backend, not a fleet scheduler — a `deps_test` keeps `internal/secrets/broker` importing only stdlib + `internal/secrets`, never the orchestrator, never a remote-DB/RPC package (the `docs/SWARM.md` §9 guard, applied to secrets).

---

*EXT-06 is allowed only because the authority it adds is **scoped, gated, credential-stored, short-lived, and never handed to the model** — and only after the §0 gate clears with EXT-01/EXT-05. The `SecretStore` interface does not change; the per-host backends stay the default; the model never sees a key, ever. Build it the way the rest of the system earns trust: the SecretStore boundary, the human gate, a verifier that still has the only vote on "done" — now with a broker that shrinks every lease to the smallest, shortest-lived thing that can do the job. <3*
