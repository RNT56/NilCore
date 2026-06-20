# EXT-05 — Enterprise control plane (SSO/SCIM/RBAC + dashboards + tenancy)

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/OPERATIONS.md` (§1 Authorized control) → `docs/ROADMAP-EXTERNAL-INFRA.md` (§0 gate, §6 EXT-05) → `docs/SWARM.md` (the depth template this plan mirrors) → this file.

> **STATUS: BLOCKED behind the §0 gate.** This is a *plan*, not an eligible task. No `EXT-05-T##` may be written, branched, or merged until the gate in `docs/ROADMAP-EXTERNAL-INFRA.md` §0 clears with a recorded human thesis decision. The plan is structured so that, **the moment the gate clears**, a fleet of agents can execute it under `CLAUDE.md` §5 with zero collision. Nothing here lands the integrator; nothing here ships on a self-report; nothing here hands an identity credential to the model.

---

## Table of contents

- [§0 The EXT-05 gate (what must be recorded before any EXT-05-T## is written)](#0-the-ext-05-gate)
- [§1 Summary](#1-summary)
- [§2 As-is: the seam EXT-05 federates above (sourced)](#2-as-is-the-seam-ext-05-federates-above)
- [§3 Architecture (federate the IdP ABOVE the Authorizer seam)](#3-architecture)
- [§4 The task DAG (EXT-05-T01 … EXT-05-T16) — disjoint Owns](#4-the-task-dag)
- [§5 Per-task specs](#5-per-task-specs)
- [§6 Parallel wave map & critical path](#6-parallel-wave-map--critical-path)
- [§7 Per-invariant ledger (I3 federated identity, I7 trust line, I5 audit)](#7-per-invariant-ledger)
- [§8 Module justifications (SAML / OIDC / SCIM — prefer stdlib, CGO-free)](#8-module-justifications)
- [§9 Default-off byte-identical proof (the flat allowlist stays the default)](#9-default-off-byte-identical-proof)
- [§10 Risks](#10-risks)

---

## §0 The EXT-05 gate

EXT-05 is **not** an eligible task in the `CLAUDE.md` §5 sense. Before a single line of any `EXT-05-T##` is written, **all** of the following must hold and be recorded **in the PR that promotes EXT-05 into `docs/TASKS.md`** (itself a serialized contract change, per the §5 contract-file list). This restates `docs/ROADMAP-EXTERNAL-INFRA.md` §0, specialized to the enterprise control plane.

1. **A recorded thesis decision (the gate proper, human-only).** A human owner has explicitly decided that NilCore's identity may expand from "tiny single-operator self-hosted harness" toward "a harness that *can* serve a team/enterprise under managed identity." This decision is irreversible and outward-facing — exactly the class the design reserves for a human (`docs/ARCHITECTURE.md` §autonomy; `policy.GateStructured` default-denies a nil approver, `internal/policy/gateaction.go:86-99`). **It is not delegable to the agent.** Recorded with a named owner + date.
2. **Invariants extended, not bypassed.** The PR must show concretely that EXT-05 **extends** `I1`–`I7`:
   - **`I3` (load-bearing for EXT-05).** Federating to an IdP introduces *standing identity*: an OIDC/SAML client credential, a SCIM bearer token, signing/verification keys. Each is scoped to authentication-only, held in `secrets.SecretStore` (`internal/secrets/secrets.go:19-24`), injected per-request server-side, **never written to disk in plaintext, never logged, never placed in a prompt, never given to the model.** The federation runs **above** the existing trust line — it only *decides who a principal is*; the deny-default `Permit` seam below it is unchanged.
   - **`I7` (the per-message trust line).** One principal per thread, deny-default, must survive. The IdP-derived principal is still a single opaque id that `Authorizer.Permit` checks **before** any message becomes a `Turn` (`internal/server/server.go:152-173`). Federation changes the *source* of the allow-set (a directory, not a static list), never the *position* of the check.
   - **`I5` (append-only audit).** `unauthorized_command` / `unauthorized_gate` events stay verbatim (`internal/channel/authorized.go:52-54,76-78`; `internal/server/server.go:158-160,169-171`). New events (SSO assertion verified, SCIM provision, RBAC denial, tenant scope check) are append-only, metadata-only, redacted.
   - **`I2`** still governs: no control-plane surface ships work on a self-report; the verifier's verdict and the integrator's never-land guarantee are untouched (EXT-05 touches *authorization*, not *the loop*).
3. **The verifier still governs (`I2`) and the integrator never lands.** EXT-05 adds no code path that promotes to base; `policy.GateAction{PromoteToBase}` through the human approver remains the only base-branch land (`internal/integrate/integrate.go` never-land; `internal/policy/gateaction.go:86-99`). RBAC may *gate* who is allowed to *request* a promotion, but the gate itself is unchanged and still default-denies.
4. **Dependency budget justified (`I6`).** Every new module (SAML XML-dsig, OIDC/JOSE, SCIM) is justified in **both** the PR and the CHANGELOG and must not break `CGO_ENABLED=0` (`.github/workflows/release.yml`). The default is **stdlib + a hand-rolled minimal verifier** (OIDC ID-token validation is `crypto/rsa` + `crypto/ecdsa` + `encoding/json`; SCIM is `net/http` + `encoding/json`). See §8 — only SAML may justify a single CGO-free module, and even SAML has a stdlib path.
5. **Default-off, opt-in, reversible to remove.** The default `nilcore` binary is **byte-identical** with the control plane absent. The flat allowlist (`channel.Authorized`, empty-by-default deny-all) **remains the default and only** authorizer when no IdP is configured. Removing the feature is deleting leaf packages + one config block. See §9 for the proof obligation.

If any of these cannot be met, EXT-05 stays on the roadmap, unbuilt.

---

## §1 Summary

EXT-05 makes NilCore *able* to serve teams and enterprises under managed identity — SAML/OIDC SSO, SCIM directory provisioning, RBAC roles, per-user/team usage-and-cost dashboards, an admin console, and multi-tenancy — **without** changing what makes NilCore trustworthy. The whole design is a single architectural move: **federate the identity provider ABOVE the existing `Authorizer.Permit` seam, never below it.** Today authorization is a *flat opaque-id allowlist* — `channel.Authorized` is a `map[string]struct{}`, empty-by-default deny-all, sourced from `NILCORE_ALLOWLIST` + `config.json` `channel.allow` (`internal/channel/authorized.go:15-19,35-38`; `cmd/nilcore/main.go:1512,1531`), and `serve` *fatals* on an empty allowlist (`cmd/nilcore/main.go:608-612,626`). EXT-05 keeps that seam exactly where it is — the per-message trust line stays the single load-bearing promotion to principal trust (`internal/server/server.go:16-24,152-173`) — and replaces only the **source** of the allow-set: instead of a static list, the allow-set is *derived from a directory* (IdP groups → RBAC roles → an opaque-id allow-set + scope), refreshed by SCIM, recomputed per message. Cost attribution extends the budget `Ledger`'s keying from per-task/global (`internal/budget/budget.go:33-39`) to per-user/per-tenant, fed by the already-existing `meter.Provider.OnUsage` telemetry hook (`internal/meter/meter.go:52,57-61`) into a dashboard that is a **read-only projection** — never a new authority. Tenancy is an isolation *prefix* over the conversation/budget/event-log keyspace (the same discipline `internal/swarm` uses for run-isolation), not a new database and not a multi-host control plane. Every piece is a **new leaf package or an additive seam**; the frozen contract (`I1`), `go.mod` (`I6`), and `internal/channel/channel.go` are untouched. **All opt-in; absent ⇒ the flat allowlist is the default and the binary is byte-identical.** EXT-05 does not add a way to ship work the verifier did not bless, nor a way to drive the agent without crossing the deny-default trust line — it adds *who* and *how-much-by-whom*, never *whether the verifier governs*.

---

## §2 As-is: the seam EXT-05 federates above

The single most important fact for this plan: **the authorization trust line already exists, is correct, and is the only promotion to principal trust.** EXT-05 federates *above* it and never rebuilds it.

### 2.1 The flat opaque-id allowlist (federate ABOVE this — do not replace)

| Seam | What it gives EXT-05 (sourced) |
|---|---|
| `channel.Authorized` | the flat allow-set: `allowed map[string]struct{}`, **empty-by-default deny-all** ("anyone who finds the bot drives it" hole closed), `Permit(principal) bool` is a single map lookup, `Receive` refuses + logs `unauthorized_command`, `GuardedApprove` refuses + logs `unauthorized_gate` (`internal/channel/authorized.go:15-19,35-38,43-56,74-81`). **EXT-05 keeps `Authorized` as the default; it adds a *second* `Authorizer` impl that derives the same opaque-id decision from a directory.** |
| `server.Authorizer` | the abstraction the server depends on: `interface { Permit(principal string) bool }` — `*channel.Authorized` satisfies it (`internal/server/server.go:60-63`). **This interface is the federation seam. A `directory.Authorizer` implements the same one method.** It is the **only** promotion to principal trust. |
| `server.intake` | the trust line in action: `if s.Auth == nil \|\| !s.Auth.Permit(req.Sender)` BEFORE anything (`internal/server/server.go:152-173`); a nil `Auth` is deny-all (no ambient authority); one thread is pinned to one principal from the first authorized message (`server.go:16-24,164-173,314-334`). |
| `cmd` wiring | `auth := channel.NewAuthorized(bot, allow, log)` where `allow` is `NILCORE_ALLOWLIST` + `config.json channel.allow` (`cmd/nilcore/main.go:1512,1518-1531`); `serve` fatals on an empty allowlist (`main.go:608-612,626`). **EXT-05 swaps which `Authorizer` is constructed here when (and only when) an IdP is configured; default stays `NewAuthorized`.** |
| `channel.NewApprover` | per-thread gate routing (`internal/channel/approver.go:20`); gate answers run through `GuardedApprove` so an unauthorized principal's approval is ignored. RBAC layers a role check *in front of* this, never replacing it. |

### 2.2 The cost/telemetry seams (extend the keying — do not rebuild)

| Seam | What EXT-05 reuses |
|---|---|
| `budget.Ledger` | per-scope metering + ceiling enforcement: `tasks map[string]*meter`, `ceilings map[string]float64`, `global meter`, `SetTaskCeiling`/`SetGlobalCeiling`/`Charge`/`Spent`/`Total` (`internal/budget/budget.go:33-39,52-128`). **The scope key is an arbitrary string** — `meter.Provider{Task:"…"}` already keys by `swarm/<runID>/<shardID>` in the swarm plan. EXT-05 keys by `tenant/<t>/user/<u>` so attribution is per-user/tenant *for free* — no Ledger change, just a richer key. |
| `meter.Provider.OnUsage` | the one observational hook that sees `resp.Usage` on **every** model call (Complete + Stream): `OnUsage func(modelID string, in, out int)`, nil-safe, never affects charging (`internal/meter/meter.go:39-61,84,143`). **This is the dashboard's data source** — fan it into a per-user/tenant aggregator exactly as the conversational front door fans it into the context gauge. |
| `secrets.SecretStore` | `Get/Set/Delete/Name` (`internal/secrets/secrets.go:19-24`); the IdP client secret, SCIM token, and signing keys live here by **name**, injected server-side, never to the model (`I3`). |
| `policy.GateStructured` | nil-approver default-deny for every irreversible action (`internal/policy/gateaction.go:86-99`). RBAC gates *who may request* a `PromoteToBase`; the gate itself is unchanged. |

**What is genuinely new** (the only code EXT-05 writes): a federation/identity leaf (OIDC + SAML verifiers, a `directory.Authorizer` over the seam), a SCIM provisioning leaf, an RBAC policy leaf, a tenancy keyspace leaf, a usage-aggregator leaf over `OnUsage` + `Ledger`, a read-only dashboard/admin projection (`//go:build tui` for the rich console, stdlib text/json otherwise), config-schema fields, and the cmd wiring that selects the federated authorizer **only when an IdP is configured**. Every one is a new leaf or an additive seam.

---

## §3 Architecture

The organizing principle: **identity is decided ABOVE the trust line; the trust line itself is unchanged.** SSO authenticates *who*, SCIM keeps *who-is-allowed* fresh, RBAC maps *who* → *what roles* → *an opaque-id allow-set + scope*, and that allow-set is consumed by the **same** `Authorizer.Permit` seam the flat allowlist uses. The dashboard and admin console are **read-only projections**; tenancy is a **keyspace prefix**. Nothing below the seam moves.

```
   Browser / Slack / Telegram principal
            │  (SSO login, server-side, harness-held client secret from SecretStore — never to model)
            ▼
   ┌───────────────────────────────────────────────────────────────────────────┐
   │  internal/identity   (EXT-05-T01..T03)   FEDERATION — runs ABOVE the seam   │
   │    OIDC verifier (stdlib JOSE: crypto/rsa+ecdsa, JWKS over net/http)        │
   │    SAML verifier  (assertion + XML-dsig; stdlib path, optional 1 CGO-free dep)│
   │    → Principal{ Subject(opaque), Tenant, Groups[] }  (a verified identity)  │
   └───────────────────────────────┬───────────────────────────────────────────┘
                                    ▼
   ┌───────────────────────────────────────────────────────────────────────────┐
   │  internal/scim       (EXT-05-T04)   PROVISIONING (keeps the directory fresh)│
   │    SCIM 2.0 Users/Groups intake (net/http + encoding/json), bearer from     │
   │    SecretStore; writes the directory store (who exists, group membership)   │
   └───────────────────────────────┬───────────────────────────────────────────┘
                                    ▼
   ┌───────────────────────────────────────────────────────────────────────────┐
   │  internal/rbac       (EXT-05-T05)   ROLE MAPPING (groups → roles → scope)   │
   │    Role{viewer|operator|approver|admin}; group→role binding; per-role caps; │
   │    Decide(Principal) → Grant{ Permit bool, Tenant, MayApprove, MayPromote } │
   └───────────────────────────────┬───────────────────────────────────────────┘
                                    ▼
   ┌───────────────────────────────────────────────────────────────────────────┐
   │  internal/directory  (EXT-05-T06)   THE FEDERATED Authorizer (the seam)     │
   │    type Authorizer struct { dir, rbac, log }                                │
   │    func (a *Authorizer) Permit(principal string) bool  ◄── SAME ONE METHOD  │
   │      = identity resolves → RBAC grants Permit → tenant in scope → true      │
   │    *server.Authorizer satisfied; deny-default preserved; logs reuse the     │
   │    unauthorized_command / unauthorized_gate Kinds (I5)                       │
   └───────────────────────────────┬───────────────────────────────────────────┘
                                    ▼
   ════════════════════ THE UNCHANGED TRUST LINE (I7) ═══════════════════════════
   server.intake:  if Auth==nil || !Auth.Permit(req.Sender) → refuse + log + tell
   one thread = one principal (pinned from first authorized message)
   ══════════════════════════════════════════════════════════════════════════════
                                    │  (only authorized principal text becomes a Turn)
                                    ▼
            the EXISTING loop  (session.Turn → router → drivers → verifier → gate)
                                    │
                       meter.Provider.OnUsage(modelID,in,out)   budget.Ledger.Charge(key=tenant/<t>/user/<u>, …)
                                    ▼
   ┌───────────────────────────────────────────────────────────────────────────┐
   │  internal/usage      (EXT-05-T07)   per-user/tenant AGGREGATION             │
   │    fold OnUsage + Ledger.Spent(key) into a concurrency-safe tally           │
   │  internal/console    (EXT-05-T08..T09)  READ-ONLY dashboard + admin view    │
   │    text/json (stdlib) + //go:build tui Charm console; NO new authority      │
   └───────────────────────────────────────────────────────────────────────────┘
```

### 3.1 SSO (OIDC + SAML) — federated ABOVE the seam (EXT-05-T01..T03)

`internal/identity` verifies an IdP assertion and yields a **verified `Principal{Subject, Tenant, Groups}`** — nothing more. The harness-held client secret / signing keys come from `SecretStore` by name, are used only server-side, and **never reach the model or the log** (`I3`). The verifier is the trust anchor: an OIDC ID token is validated (signature against the IdP JWKS, `iss`/`aud`/`exp`/`nonce`) with **stdlib crypto** (`crypto/rsa`, `crypto/ecdsa`, `encoding/json`, `net/http` for JWKS) — no JOSE module. SAML validates the assertion + XML-dsig (stdlib path; see §8). The output is an **opaque `Subject`** — the same shape `Permit` already consumes — so the seam below never learns it came from an IdP. **An unverifiable assertion yields no Principal ⇒ deny (fail-closed).**

### 3.2 SCIM provisioning — keeps the directory fresh (EXT-05-T04)

`internal/scim` is a SCIM 2.0 intake (`/Users`, `/Groups`) over `net/http` + `encoding/json`, authenticated by a bearer token from `SecretStore`. It writes the **directory store** (the set of provisioned subjects + their group membership). It grants no command authority itself — it only updates *who exists and what groups they are in*; `Permit` still gates every message. De-provisioning (SCIM delete/`active:false`) removes the subject from the allow-set **immediately** (the next `Permit` denies). Untrusted SCIM payload fields are data, never instructions (`I7`).

### 3.3 RBAC — groups → roles → scope (EXT-05-T05)

`internal/rbac` maps IdP groups to a small closed role set (`viewer`, `operator`, `approver`, `admin`) and emits a `Grant{Permit, Tenant, MayApprove, MayPromote, Caps}`. `Permit` is the command gate; `MayApprove` front-gates `channel.GuardedApprove`; `MayPromote` front-gates a `PromoteToBase` *request* (the policy gate still default-denies and is unchanged). `Caps` carries per-role/per-tenant budget ceilings that flow into `Ledger.SetTaskCeiling`. **Unknown group ⇒ no role ⇒ no Permit (fail-closed).** The role set is closed and small on purpose (no dynamic policy DSL — that would be a new authority surface).

### 3.4 The federated Authorizer — the seam (EXT-05-T06)

`internal/directory.Authorizer` implements `server.Authorizer` (`Permit(principal) bool`) by composing identity → rbac → tenant-scope. It is a **drop-in** for `*channel.Authorized` at exactly the `cmd/nilcore/main.go:1512` construction site. It **reuses** the `unauthorized_command`/`unauthorized_gate` event Kinds (`I5`). When no IdP is configured, it is never constructed — `NewAuthorized` is, and the binary is byte-identical (§9).

### 3.5 Per-user/tenant cost + the dashboard (EXT-05-T07..T09)

The budget key becomes `tenant/<t>/user/<u>` (the Ledger keys by arbitrary string today; **no Ledger change**). `meter.Provider.OnUsage` already fires on every model call — `internal/usage` folds it + `Ledger.Spent(key)` into a per-user/tenant tally. `internal/console` renders it: a **stdlib text/json** projection always, and a **`//go:build tui` Charm** admin console (zero Charm in the default binary, per the §2 invariant-6 exception discipline). **The dashboard is a read-only projection over the Ledger + OnUsage + the append-only log — it is never a new authority and never an HTTP-served endpoint in v1** (a served multi-tenant dashboard endpoint would cross further toward a hosted SaaS; pinned out of scope in §10).

### 3.6 Multi-tenancy — a keyspace prefix, not a new database (EXT-05-T10)

Tenancy is an **isolation prefix** over the existing keyspaces — conversation/thread ids, budget ledger keys, and event-log Detail are namespaced `tenant/<t>/…`, exactly as `internal/swarm` run-isolates by `swarm/<runID>/` ID prefix (filtered in Go, no store change). A thread pinned to principal `u` in tenant `t` can never be read or steered by a principal in tenant `t'` (the `Permit` + the one-thread-one-principal pin already enforce per-principal isolation; the tenant prefix extends it to groups of principals). **Single-host, single-process, one local SQLite store** — tenancy is a *logical* partition, **not** a multi-host control plane (that is `EXT-01`, out of scope; §10).

---

## §4 The task DAG

**Namespace `EXT-05-T01 … EXT-05-T16`.** One task = one branch (`task/EXT-05-T0x`) = one PR. Owns sets are **pairwise disjoint** (package dir = unit of ownership; each existing-file edit is held by exactly one task). The federation foundation (T01–T06) lands **before** the dashboard/tenancy/wiring tasks. **All gated behind §0 — no row is eligible until the gate clears.**

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-05-T01 | Identity core: `Principal` + verifier interface + stdlib JWKS client | — | `internal/identity/` (`identity.go`, `jwks.go`, `*_test.go`, `deps_test.go`) | new leaf; opens `internal/identity` |
| EXT-05-T02 | OIDC ID-token verifier (stdlib crypto) | EXT-05-T01 | `internal/identity/` (`oidc.go`, `oidc_test.go`) | serial after T01 (same pkg) |
| EXT-05-T03 | SAML assertion + XML-dsig verifier | EXT-05-T02 | `internal/identity/` (`saml.go`, `saml_test.go`) | serial after T02 (same pkg); §8 module decision lands here |
| EXT-05-T04 | SCIM 2.0 provisioning intake + directory store | EXT-05-T01 | `internal/scim/`, `internal/directory/store.go` | new leaf + the directory store file |
| EXT-05-T05 | RBAC role mapping + `Grant` | EXT-05-T01 | `internal/rbac/` | new leaf |
| EXT-05-T06 | Federated `directory.Authorizer` (the seam impl) | EXT-05-T03, EXT-05-T04, EXT-05-T05 | `internal/directory/authorizer.go` + `internal/directory/*_test.go` | implements `server.Authorizer`; reuses I5 Kinds |
| EXT-05-T07 | Per-user/tenant usage aggregator over `OnUsage` + `Ledger` | — | `internal/usage/` | new leaf; **no `budget`/`meter` edit** |
| EXT-05-T08 | Console projection (stdlib text/json) | EXT-05-T07 | `internal/console/` (`console.go`, `render.go`, `*_test.go`) | read-only projection |
| EXT-05-T09 | Admin console TUI (`//go:build tui`) | EXT-05-T08 | `internal/console/` (`console_tui.go`) | serial after T08 (same pkg); zero Charm in default |
| EXT-05-T10 | Tenancy keyspace (prefix helpers + isolation guards) | EXT-05-T05 | `internal/tenancy/` | new leaf; ID-prefix discipline (mirrors swarm) |
| EXT-05-T11 | RBAC front-gate for approve / promote (additive helper) | EXT-05-T05, EXT-05-T06 | `internal/directory/gate.go` + test | wraps `GuardedApprove`/`PromoteToBase` request; gate unchanged |
| EXT-05-T12 | Config schema: `Config.Enterprise *enterprise.Config` + Validate | EXT-05-T02, EXT-05-T05, EXT-05-T07 | `internal/onboard/onboard.go`, `onboard_test.go` | **contract (config schema)** — serialized |
| EXT-05-T13 | `enterprise` wiring leaf (compose identity+scim+rbac+directory+usage) | EXT-05-T06, EXT-05-T07, EXT-05-T10, EXT-05-T11 | `internal/enterprise/` | composition leaf, imported only by cmd |
| EXT-05-T14 | cmd: select federated authorizer when IdP configured + `nilcore admin` | EXT-05-T12, EXT-05-T13, EXT-05-T08 | `cmd/nilcore/enterprise.go` (new), `cmd/nilcore/main.go` (one `case "admin"` + authorizer-select) | **serialized cmd-wiring** (sole `main.go` editor) |
| EXT-05-T15 | `nilcore init` enterprise step (onboard wizard, opt-in) | EXT-05-T12 | `internal/onboard/wizard.go` | additive wizard step; default = skip |
| EXT-05-T16 | Docs + CHANGELOG promotion | EXT-05-T14, EXT-05-T15 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/OPERATIONS.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs)** — serialized last; records the §0 gate decision |

> **Owns-disjointness notes.** `internal/identity` is the **whole package** as the Owns unit for T01–T03 (so §5 work-selection forbids two open at once — they are a serial sub-chain). `internal/console` likewise serializes T08→T09. `internal/directory` is split so T04 owns only `store.go`, T06 owns `authorizer.go`, T11 owns `gate.go` — disjoint files, all in one dir, but each held by one task at a time (the dir opens at T04). `internal/onboard/onboard.go` (config schema) is a serialized contract surface held by T12 only; `cmd/nilcore/main.go` by T14 only.

---

## §5 Per-task specs

#### EXT-05-T01 — Identity core: `Principal` + verifier interface + stdlib JWKS client
- **Goal:** open `internal/identity` with the verified-identity type and the verifier abstraction the OIDC/SAML impls satisfy, plus a stdlib JWKS fetch/cache so an IdP's public keys are retrievable without a JOSE module.
- **Depends on:** — (reuses stdlib + `internal/secrets`, `internal/eventlog` via an injected sink).
- **Owns:** `internal/identity/` (`identity.go`, `jwks.go`, `*_test.go`, `deps_test.go`).
- **Acceptance:** `Principal{Subject string /*opaque*/, Tenant string, Groups []string}`; `Verifier interface { Verify(ctx, raw []byte) (Principal, error) }`; a verification failure returns `(Principal{}, err)` and an empty `Subject` is **never** treated as authorized (fail-closed — a downstream `Permit("")` must be false); `JWKS{FetchAndCache(ctx, url) (keySet, error)}` over `net/http` + `crypto/rsa`/`crypto/ecdsa` + `encoding/json`, with a bounded TTL cache and a hard response-size cap; the IdP HTTP client honors `ctx`, a timeout, and only `https` issuer URLs (reject `http`); **no secret is ever in an error string** (`I3`); a metadata-only `sso_verify`/`sso_verify_failed` event via an injected `EventSink` (the leaf never imports `eventlog` directly — mirrors `schema.SchemaVerifier`).
- **Verify:** `make verify`; `go test -race ./internal/identity/...`: JWKS round-trip against an `httptest` server; expired/oversized/`http`-scheme JWKS rejected; an empty-Subject Principal is rejected by a guard helper; assert no key bytes in any error/Output; `deps_test.go` (`go list -deps`) asserts no `agent`/`super`/`project`/`server` import and **no third-party module**.
- **Notes:** establishes `internal/identity` ownership; T02/T03 add sibling files in the same dir (serial). Stdlib-only (`I6`).

#### EXT-05-T02 — OIDC ID-token verifier (stdlib crypto)
- **Goal:** validate an OIDC ID token end-to-end with stdlib only — signature against the JWKS, `iss`/`aud`/`exp`/`iat`/`nonce` claims — yielding a `Principal` whose `Subject` is the opaque `sub`, `Tenant` the configured tenant claim, `Groups` the configured groups claim.
- **Depends on:** EXT-05-T01. **Owns:** `internal/identity/` (`oidc.go`, `oidc_test.go`).
- **Acceptance:** `OIDCVerifier{Issuer, Audience, TenantClaim, GroupsClaim, JWKS, Clock}` implements `identity.Verifier`; verifies RS256/ES256 via `crypto/rsa`/`crypto/ecdsa` (reject `alg:none` and symmetric `alg` hard); validates `iss==Issuer`, `aud` contains `Audience`, `exp`/`iat` within a small skew (`Clock` injectable for tests), `nonce` when present; maps `sub`→`Subject`, the configured claims→`Tenant`/`Groups`; a malformed/expired/wrong-aud/`alg:none` token ⇒ error (no Principal); the client secret (if a confidential client) resolves via `secrets.SecretStore` by **name**, used only in the token-exchange call, never logged.
- **Verify:** `make verify`; `go test -race ./internal/identity/...`: a signed test token (test keypair) verifies; tampered signature / `alg:none` / expired / wrong `aud` / wrong `iss` each rejected; tenant/groups claim extraction; **fuzz the JWT parse** for panics; assert no secret in errors.
- **Notes:** the cheapest, sharpest stdlib path (`I6`) — OIDC needs no JOSE module.

#### EXT-05-T03 — SAML assertion + XML-dsig verifier
- **Goal:** validate a SAML 2.0 assertion (signature, `NotBefore`/`NotOnOrAfter`, audience, issuer) → `Principal`. The one task where §8's module decision is made and recorded.
- **Depends on:** EXT-05-T02. **Owns:** `internal/identity/` (`saml.go`, `saml_test.go`).
- **Acceptance:** `SAMLVerifier{IDPMetadata/cert, SPEntityID, TenantAttr, GroupsAttr, Clock}` implements `identity.Verifier`; validates the enveloped XML-dsig against the IdP cert (the **stdlib path**: canonicalize + `crypto/rsa`/`crypto/x509` digest check over a constrained, namespace-explicit parse — **no** arbitrary XML transforms accepted; reject anything outside the allowlisted canonicalization); validates conditions/audience/issuer; maps `NameID`→`Subject`, attrs→`Tenant`/`Groups`; **XML attacks fail closed**: external-entity refused (no DTD), signature-wrapping refused (verify the signed element is the asserted one), unsigned/multiply-signed ⇒ error.
- **Verify:** `make verify`; `go test -race ./internal/identity/...`: a signed test assertion verifies; XML-signature-wrapping, XXE/DTD, expired conditions, wrong audience, unsigned each rejected (table of canonical SAML attack vectors); assert no cert/key bytes in errors.
- **Notes:** **§8 module decision lands here and is recorded in the PR + CHANGELOG.** If the stdlib canonicalization proves infeasible within review, exactly **one** CGO-free, vendored, audited XML-dsig dependency may be justified (`I6`) — never a broad SAML SDK. If no acceptable option clears review, **OIDC-only ships and SAML stays gated** (OIDC covers the majority of IdPs).

#### EXT-05-T04 — SCIM 2.0 provisioning intake + directory store
- **Goal:** a SCIM 2.0 endpoint that keeps the directory store fresh (who exists, group membership) under a `SecretStore` bearer token, granting **no command authority itself**.
- **Depends on:** EXT-05-T01. **Owns:** `internal/scim/`, `internal/directory/store.go` (opens `internal/directory`).
- **Acceptance:** `Store` over the local SQLite `internal/store` (a `scim-*` Status namespace, run/tenant-isolated by ID prefix in Go — no store change) holding `subject → {Tenant, Groups, Active}`; `Handler` (`net/http` + `encoding/json`) serving SCIM 2.0 `/Users` + `/Groups` (create/replace/patch/delete) authenticated by a **constant-time** bearer compare against a token from `secrets.SecretStore`; `active:false`/delete removes the subject from the allow-set so the **next `Permit` denies** (de-provision is immediate); request bodies are size-capped and parsed with `DisallowUnknownFields`; SCIM fields are **data, never instructions** (`I7`); metadata-only `scim_provision`/`scim_deprovision` events; the listener binds `localhost`/a configured private bind only (no `0.0.0.0` default) and is **off unless explicitly configured** (default-off).
- **Verify:** `make verify`; `go test -race ./internal/scim/... ./internal/directory/...`: provision→`Store` has the subject; deprovision→subject gone (`Permit` denies on the next check via a directory-Authorizer stub); wrong/empty bearer ⇒ 401 (constant-time path exercised); oversized body rejected; unknown field rejected; assert the token never appears in a log/response.
- **Notes:** SCIM updates *the directory*, not *the trust line*. `Permit` still gates every message. The local single-writer SQLite store is the serialization point (single host).

#### EXT-05-T05 — RBAC role mapping + `Grant`
- **Goal:** map IdP groups to a small closed role set and emit the authorization `Grant` the directory Authorizer consumes.
- **Depends on:** EXT-05-T01. **Owns:** `internal/rbac/`.
- **Acceptance:** closed `Role` enum (`viewer`/`operator`/`approver`/`admin`); `Bindings` (group → []Role, operator-configured); `Policy{Bindings, RoleCaps map[Role]Caps}`; `Decide(p identity.Principal) Grant` where `Grant{Permit bool, Tenant string, MayApprove bool, MayPromote bool, Caps Caps}`; the role→capability table is **fixed in code** (no dynamic DSL): `viewer`⇒no Permit (dashboard read only, surfaced via console not the command path), `operator`⇒Permit, `approver`⇒Permit+MayApprove, `admin`⇒Permit+MayApprove+MayPromote; **unknown group ⇒ no role ⇒ `Permit:false`** (fail-closed); `Caps{TenantCeiling, UserCeiling float64}` flows to `Ledger.SetTaskCeiling`.
- **Verify:** `make verify`; `go test -race ./internal/rbac/...`: each role's capability set; unknown group ⇒ deny; multi-group union takes the **most-privileged** role only if all groups resolve (a single unknown group does not silently widen); caps mapping; table-driven over the closed matrix.
- **Notes:** closed role set on purpose — a dynamic policy engine is a new authority surface and stays out of scope (`I3`).

#### EXT-05-T06 — Federated `directory.Authorizer` (the seam impl)
- **Goal:** the drop-in `server.Authorizer` that composes identity→rbac→tenant-scope into the single `Permit(principal) bool` decision, preserving deny-default and the `I5` audit Kinds.
- **Depends on:** EXT-05-T03, EXT-05-T04, EXT-05-T05. **Owns:** `internal/directory/authorizer.go` + `internal/directory/*_test.go`.
- **Acceptance:** `Authorizer{Store, RBAC, Log}` implements `server.Authorizer` (compile-time `var _ server.Authorizer` — **or**, to avoid importing `server`, a local `var _ interface{ Permit(string) bool }`); `Permit(principal)` = resolve the subject in `Store` (active?) → `RBAC.Decide` (`Permit`?) → tenant-in-scope → `true`; **any miss ⇒ false** (empty/unknown/inactive/unscoped subject denied); on a denial it appends the **existing** `unauthorized_command` Kind (no new denial Kind — `I5` continuity); the principal id passed in is the same opaque shape `channel.Authorized` uses (the IdP-derived `Subject`), so the seam below is unaware of the source; **zero network call in `Permit`** (the directory is already populated by SCIM/SSO out of band — `Permit` is a local lookup, keeping the hot path cheap and offline-safe).
- **Verify:** `make verify`; `go test -race ./internal/directory/...`: active+operator ⇒ Permit; inactive ⇒ deny; unknown subject ⇒ deny; wrong-tenant ⇒ deny; empty principal ⇒ deny; a denial appends `unauthorized_command`; `Permit` makes no HTTP call (a fake Store/RBAC with a network tripwire stays untouched); satisfies the `server.Authorizer` shape (interface-assignment test).
- **Notes:** this is the federation seam — a drop-in for `*channel.Authorized` at `cmd/nilcore/main.go:1512`. It **federates above** the trust line; it does not move it.

#### EXT-05-T07 — Per-user/tenant usage aggregator over `OnUsage` + `Ledger`
- **Goal:** turn the existing `meter.Provider.OnUsage` hook + `budget.Ledger` keying into a per-user/tenant tally, with **no edit to `budget` or `meter`**.
- **Depends on:** — (reuses `internal/budget`, `internal/meter` read-only).
- **Owns:** `internal/usage/`.
- **Acceptance:** `Aggregator{Ledger *budget.Ledger}` with `OnUsage(key, modelID string, in, out int)` (concurrency-safe, O(1) per update) accumulating per-key in/out tokens; `Snapshot(key) Usage{Tokens, Dollars}` reading `Ledger.Spent(key)` for the priced dollars and the local tally for the token split; the **scope key is `tenant/<t>/user/<u>`** (an arbitrary string — the Ledger already keys by string, so **no Ledger change**); the cmd wiring sets each metered provider's `Task` to this key and fans `meter.OnUsage` into `Aggregator.OnUsage`; immutable copy-out `Snapshot`; never affects charging (purely observational, like `OnUsage` itself).
- **Verify:** `make verify`; `go test -race ./internal/usage/...`: 300 goroutines `OnUsage` across many keys + one polling `Snapshot` (race-clean, correct per-key tally); dollars match `Ledger.Spent(key)`; an unknown key reports zero; Snapshot immutable.
- **Notes:** the dashboard's data source. The keyed Ledger is the entire mechanism — EXT-05 only enriches the key (the same move the swarm plan makes with `swarm/<runID>/<shardID>`).

#### EXT-05-T08 — Console projection (stdlib text/json)
- **Goal:** a read-only per-user/tenant dashboard projection (usage, cost, role, last-seen) as stdlib text/json — never a new authority, never a served endpoint in v1.
- **Depends on:** EXT-05-T07. **Owns:** `internal/console/` (`console.go`, `render.go`, `*_test.go`) — opens `internal/console`.
- **Acceptance:** `Model{Tenants []TenantRow, Users []UserRow}` projected from `usage.Aggregator` + `rbac.Policy` (role per user) + the directory store (active/last-seen) + the append-only log (counts); `RenderText(Model, termui.Style) string` (deterministic order, redacts any keyed URL `I3`, escapes untrusted display fields `I7`) and `RenderJSON(Model) []byte` (a **redacted projection**, never raw internal structs); **read-only**: the package exposes no mutator and imports nothing that can command the agent or change a grant; per-row figures are projections of trusted fields only (no model-authored `Value`).
- **Verify:** `make verify`; `go test -race ./internal/console/...`: a fixture aggregator+policy+store → expected text/json; deterministic columns; redaction of a `?api_key=` URL; escaping of a `<script>` display name; json carries no secret; an import-graph test asserts `console` imports no orchestrator/command path.
- **Notes:** the dashboard is a projection, full stop. A served HTTP dashboard endpoint is pinned out of scope (§10) so a follow-on PR cannot quietly add one.

#### EXT-05-T09 — Admin console TUI (`//go:build tui`)
- **Goal:** the rich admin console over the **same** `console.Model`/`Snapshot`, isolated behind `//go:build tui` so the default `nilcore` binary links **zero** Charm (the sanctioned `I6` exception discipline).
- **Depends on:** EXT-05-T08. **Owns:** `internal/console/` (`console_tui.go`).
- **Acceptance:** a `//go:build tui` Charm view rendering the **identical** `console.Model` the stdlib renderer uses (live == replay parity: the TUI shows what `RenderJSON` would emit); zero Charm symbol reachable in a default (`!tui`) build; read-only (it can scroll/filter, never mutate a grant or command a thread).
- **Verify:** `make verify` (default build links no Charm — assert via `go list -deps` on the default tag); `go test -tags tui ./internal/console/...`; a parity test that the TUI's model == the stdlib model for a fixture.
- **Notes:** mirrors `internal/swarm/board/board_tui.go`'s `//go:build tui` discipline exactly.

#### EXT-05-T10 — Tenancy keyspace (prefix helpers + isolation guards)
- **Goal:** the logical multi-tenant partition as an **ID-prefix discipline** over conversation/budget/event-log keyspaces — single-host, single-store, no new database.
- **Depends on:** EXT-05-T05. **Owns:** `internal/tenancy/`.
- **Acceptance:** `Key(tenant, kind, id) string = "tenant/"+tenant+"/"+kind+"/"+id` (the canonical prefix); `InTenant(key, tenant) bool` (Go-side filter, mirrors swarm's `ShardsByRun` prefix filter — **no store change**); `Scope(principal Principal) (tenant string, ok bool)`; a guard `SameTenant(a, b Key) bool` used by the directory Authorizer's tenant-in-scope check and by the console projection so a tenant's rows never bleed into another's; **a thread pinned to tenant `t` is unreadable/un-steerable from tenant `t'`** (proven by the `Permit` + one-thread-one-principal pin already in `server.intake`, extended by the tenant scope check).
- **Verify:** `make verify`; `go test -race ./internal/tenancy/...`: prefix round-trip; `InTenant` isolation (a `t'` key never matches a `t` filter); `SameTenant` cross-tenant denial; a property test that no two distinct tenants share a key namespace.
- **Notes:** tenancy is logical, not physical — **not** a multi-host control plane (that is `EXT-01`, §10). The ID-prefix discipline is exactly the swarm's run-isolation, reused.

#### EXT-05-T11 — RBAC front-gate for approve / promote (additive helper)
- **Goal:** front-gate gate *approval* and *promote-to-base requests* by RBAC role — **without** changing the policy gate itself (it still default-denies a nil approver).
- **Depends on:** EXT-05-T05, EXT-05-T06. **Owns:** `internal/directory/gate.go` + test.
- **Acceptance:** `GuardApprove(p Principal, answer bool) bool` = `rbac.Decide(p).MayApprove && channel.GuardedApprove(p.Subject, answer)` (RBAC *narrows*, never widens — an unauthorized or non-approver principal's approval is still ignored + logged via the existing `unauthorized_gate` Kind, `I5`); `MayRequestPromote(p Principal) bool` = `rbac.Decide(p).MayPromote` (gates who may **request** a `PromoteToBase`; `policy.GateStructured` still consults the human approver and **still default-denies** — `I2`/never-land unchanged); **no new `GateActionType`, no auto-land path.**
- **Verify:** `make verify`; `go test -race ./internal/directory/...`: a non-approver's `true` ⇒ ignored + `unauthorized_gate` logged; an approver's `true` ⇒ honored only if `GuardedApprove` also passes; a non-admin `MayRequestPromote` ⇒ false; assert no code path lands to base.
- **Notes:** RBAC is a *narrowing* front-gate; the irreversible-action gate is untouched and remains the human's.

#### EXT-05-T12 — Config schema: `Config.Enterprise *enterprise.Config` + Validate · contract (config schema)
- **Goal:** additively extend `onboard.Config` with **one** optional `Enterprise *enterprise.Config` (`json:"enterprise,omitempty"`) + a Validate clause, v1-compatible.
- **Depends on:** EXT-05-T02, EXT-05-T05, EXT-05-T07. **Owns:** `internal/onboard/onboard.go`, `onboard_test.go`.
- **Acceptance:** default-zero/nil so every existing config parses unchanged under `DisallowUnknownFields` (exactly the `Pool *pool.PoolConfig` precedent already in `onboard.go:66`); `enterprise.Config{OIDC *OIDCConfig, SAML *SAMLConfig, SCIM *SCIMConfig, RBAC RBACConfig, Tenants []string}` carries **only secret *refs* by name, never secret values** (`OIDC.ClientSecretRef`, `SCIM.TokenRef`, `SAML.IDPCertRef` — mirrors `ChannelConfig.TokenRefs`/`WebConfig.SearchKeyRef`); `Validate()` gains an enterprise clause (issuer is `https`, audience non-empty, RBAC bindings reference known roles, caps ≥ 0 — loud error otherwise); `nil Enterprise` ⇒ the flat allowlist path (default-off); a config with `enterprise` round-trips parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip with `Enterprise` set; old config (no `enterprise`) parses; `Validate` rejects `http` issuer, empty audience, unknown role, negative cap; **assert no secret value field exists on the config struct** (refs only).
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface), so it is a contract surface even though not on the frozen §5 list. `onboard → enterprise` is downward (no cycle).

#### EXT-05-T13 — `enterprise` wiring leaf
- **Goal:** compose identity + scim + rbac + directory + usage + tenancy into one constructible unit the cmd layer wires (the `buildStack` analogue for the control plane).
- **Depends on:** EXT-05-T06, EXT-05-T07, EXT-05-T10, EXT-05-T11. **Owns:** `internal/enterprise/`.
- **Acceptance:** `Build(cfg enterprise.Config, sec secrets.SecretStore, store *store.DB, ledger *budget.Ledger, log EventSink) (*Plane, error)` resolving every secret **by name** from `sec` (client secret, SCIM token, SAML cert — never a value in `cfg`), constructing the OIDC/SAML verifiers, the SCIM handler + directory store, the RBAC policy, the `directory.Authorizer`, and the `usage.Aggregator`; `Plane.Authorizer() server.Authorizer` (the federated seam impl); `Plane.SCIMHandler() http.Handler` (nil unless SCIM configured); `Plane.Usage() *usage.Aggregator`; `Plane.OnUsageHook() func(key, model string, in, out int)`; **a nil/zero `cfg` ⇒ `Build` returns `(nil, nil)`** so the cmd layer falls back to the flat allowlist (default-off); imported **only** by `cmd/nilcore`.
- **Verify:** `make verify`; `go test -race ./internal/enterprise/...`: `Build` resolves refs from a fake SecretStore (never reads a value from cfg — assert cfg holds no value field); zero cfg ⇒ `(nil,nil)`; `Authorizer()` satisfies `server.Authorizer`; a missing referenced secret ⇒ loud error (no silent fallback to deny-all-but-look-configured); a `deps_test` asserts no orchestrator import.
- **Notes:** the single composition point; keeps secret resolution server-side (`I3`). Leaf: imported only by cmd.

#### EXT-05-T14 — cmd: select federated authorizer when IdP configured + `nilcore admin` · serialized cmd-wiring
- **Goal:** the operator front door — at the **single** authorizer construction site (`cmd/nilcore/main.go:1512`) select `enterprise.Plane.Authorizer()` **iff** an enterprise config is present, else `channel.NewAuthorized` (byte-identical default); start the SCIM listener iff configured; fan `meter.OnUsage` into the usage aggregator; add **one** `case "admin"` dispatch for the console.
- **Depends on:** EXT-05-T12, EXT-05-T13, EXT-05-T08. **Owns:** `cmd/nilcore/enterprise.go` (new), `cmd/nilcore/main.go` (the authorizer-select branch + one `case "admin"` + usage lines — the **only** edit to an existing file).
- **Acceptance:** `buildEnterprise(cfg) (*enterprise.Plane, error)` called during serve boot; **if** `cfg.Enterprise == nil` ⇒ the existing `channel.NewAuthorized(bot, allow, log)` path is taken **unchanged** (no new code reached); **else** `auth = plane.Authorizer()`, the SCIM `http.Handler` is served on the configured private bind, and each metered provider's `Task` key + `meter.OnUsage` are wired through `plane.OnUsageHook()`/`usage.Aggregator`; the per-message trust line (`server.intake` Permit) is byte-identical — only *which* `Authorizer` it calls changes; `nilcore admin` renders `console.RenderText`/`RenderJSON` (read-only) and requires the local operator (no channel principal) exactly as `nilcore chat` does (`cmd/nilcore/chat.go:68`); serve still **fatals on an empty allow-set** whichever authorizer is active (deny-default preserved — an enterprise config that resolves to an empty allow-set is as fatal as an empty `NILCORE_ALLOWLIST`); **no** other dispatch arm or shared helper edited.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, fake SecretStore/IdP/store): `cfg.Enterprise==nil` ⇒ `channel.NewAuthorized` constructed, `enterprise.Build` **not** called (a tripwire); `cfg.Enterprise!=nil` ⇒ federated authorizer constructed; an enterprise config resolving to an empty allow-set ⇒ serve fatals (deny-default); `nilcore admin` prints a non-empty read-only report; an **import-graph test** asserting no existing package imports the new enterprise leaves; an **init()-free test** asserting the new leaves have no global-side-effect `init()`.
- **Notes:** **serialized cmd-wiring** — the only task editing `main.go`. Default binary byte-identical (one branch + one arm + usage lines; all logic in new files). §9 proof obligation lives here.

#### EXT-05-T15 — `nilcore init` enterprise step (onboard wizard, opt-in)
- **Goal:** an additive `nilcore init` step that captures enterprise config (IdP issuer/audience, SCIM token *ref*, RBAC bindings, tenants) and persists it; **default = skip** so existing onboarding is unchanged.
- **Depends on:** EXT-05-T12. **Owns:** `internal/onboard/wizard.go`.
- **Acceptance:** a new "Enterprise control plane (optional)" wizard step defaulting to **skip** (Enter ⇒ no `enterprise` block written — existing onboarding flow byte-identical); when opted in, it captures the IdP issuer (`https`-validated), audience, group→role bindings, tenant list, and **captures secrets into the SecretStore by name** (the client secret / SCIM token are `Set` into `secrets`, only the *ref* written to config — mirrors how the bot token / Brave key are captured); writes `Config.Enterprise`; `Validate` runs before Save.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: skip ⇒ no `enterprise` block (existing wizard golden unchanged); opt-in ⇒ secrets land in a fake SecretStore by name, only refs in config, `Validate` passes; an `http` issuer is rejected in-wizard.
- **Notes:** additive wizard step; the captured secrets follow the existing capture-by-name discipline (`I3`).

#### EXT-05-T16 — Docs + CHANGELOG promotion · contract (docs), serialized last
- **Goal:** promote this plan into the canonical docs + ledger, **and record the §0 gate decision** that authorized EXT-05.
- **Depends on:** EXT-05-T14, EXT-05-T15. **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/OPERATIONS.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md`.
- **Acceptance:** the §0 gate decision (human owner + date + thesis statement) is recorded in the promotion PR and referenced in `docs/ROADMAP-EXTERNAL-INFRA.md` (the EXT-05 item gains a "gate cleared on <date> by <owner>" line); `docs/TASKS.md` gains the EXT-05 DAG rows + specs; `docs/ARCHITECTURE.md` gains an "Enterprise control plane (EXT-05, opt-in, default-off)" subsection (federation-above-the-seam, the unchanged trust line `I7`, IdP creds scoped+stored `I3`, the `unauthorized_*` Kinds preserved `I5`, tenancy as a logical keyspace prefix / **not** a multi-host control plane, the dashboard as a read-only projection / **not** an HTTP endpoint, never-land preserved) + the new leaf rows in the layer-map with their import sets + the restated leaf rule; `docs/OPERATIONS.md` §1 (Authorized control) gains an "optional IdP federation above the allowlist" paragraph (the flat allowlist stays the default); `CLAUDE.md` repository-map lines for the new leaves (**no invariant text change** — the invariants are unchanged, which is the point); `CHANGELOG.md` one `## [Unreleased]` line per merged EXT-05-T0x; `README.md` an enterprise section (opt-in, default-off, the flat allowlist remains default).
- **Verify:** `make verify` (docs don't break the build); a markdown pass; manual review that the layer-map import sets match each new leaf's `go list -deps`.
- **Notes:** **serialized — contract files.** Lands last. Per-task CHANGELOG lines are appended at each task's own merge; T16 adds the prose + reconciles trivial append conflicts.

---

## §6 Parallel wave map & critical path

A fleet executes in ordered **waves**; every task in a wave has all deps merged to `main` and a pairwise-disjoint Owns set, so the wave runs concurrently with zero collision. `internal/identity` and `internal/console` are each **one package = one Owns unit**, so their files form serialized sub-chains (the only intra-wave serialization).

```
WAVE 1  (4 concurrent — no-dep new leaves, each independently `make verify`-green)
  ├── EXT-05-T01  internal/identity/        (opens identity; everything SSO imports it)
  ├── EXT-05-T05  internal/rbac/
  ├── EXT-05-T07  internal/usage/           (reuses budget+meter read-only)
  └── EXT-05-T10  internal/tenancy/         (depends on T05 → moves to WAVE 2 if T05 unmerged)

WAVE 2  (4 concurrent)
  ├── EXT-05-T02  internal/identity/oidc.go        (T01)  ← SERIAL pt: identity pkg
  ├── EXT-05-T04  internal/scim/ + directory/store.go  (T01)  ← opens internal/directory
  ├── EXT-05-T08  internal/console/console.go,render.go  (T07)  ← opens internal/console
  └── EXT-05-T10  internal/tenancy/                (T05)   [if not done in W1]

WAVE 3  (2 concurrent)
  ├── EXT-05-T03  internal/identity/saml.go        (T02)  ← SERIAL pt: identity pkg; §8 module decision
  └── EXT-05-T09  internal/console/console_tui.go  (T08)  ← SERIAL pt: console pkg; //go:build tui

WAVE 4  (1 — converges identity+scim+rbac)
  └── EXT-05-T06  internal/directory/authorizer.go (T03,T04,T05)  ← the federated seam impl

WAVE 5  (2 concurrent)
  ├── EXT-05-T11  internal/directory/gate.go       (T05,T06)
  └── EXT-05-T12  internal/onboard/onboard.go      (T02,T05,T07)  ← SERIAL pt: config-schema contract

WAVE 6  (2 concurrent)
  ├── EXT-05-T13  internal/enterprise/             (T06,T07,T10,T11)  ← composition leaf
  └── EXT-05-T15  internal/onboard/wizard.go       (T12)              [needs T12 merged]

WAVE 7  (1 — SERIAL pt: cmd-wiring)
  └── EXT-05-T14  cmd/nilcore/enterprise.go + main.go  (T12,T13,T08)  ← sole main.go editor

WAVE 8  (1 — SERIAL pt: docs contract)
  └── EXT-05-T16  docs/* + CLAUDE.md + README.md + CHANGELOG.md  (T14,T15)  ← sole docs editor; records the §0 gate
```

**Peak concurrency = 4 (waves 1–2).** Critical path (longest dependency chain) — **8 sequential merges:**

```
EXT-05-T01 → EXT-05-T02 → EXT-05-T03 → EXT-05-T06 → EXT-05-T13 → EXT-05-T14 → EXT-05-T16
                                              ↑
                          (T06 also waits on T04 + T05, which finish in parallel by WAVE 3)
```

(The chain is 7 nodes / **7 merges on the longest path**; T13→T14→T16 closes it. T15 joins at WAVE 6/8 off the T12 spur, not on the critical path.)

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/identity` package dir — T01 opens; T02/T03 serialize as sibling files (package = unit of ownership; the OIDC→SAML order is the natural cheapest-first stdlib path).
2. `internal/console` package dir — T08 opens; T09 (`//go:build tui`) serializes after it.
3. `internal/directory` package dir — T04 opens (`store.go`); T06 (`authorizer.go`) and T11 (`gate.go`) are disjoint files but the dir is held by one task at a time.
4. `internal/onboard/onboard.go` — T12 only (config schema, a stable interface).
5. `cmd/nilcore/main.go` — T14 only.
6. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — T16 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; no task depends on a later task; the `internal/identity` and `internal/console` sub-chains are strictly increasing IDs. **Federation-before-orchestration holds:** the cmd wiring (T14) cannot compile until the `enterprise` plane (T13) exists, which cannot compile until the federated `Authorizer` (T06) exists, which cannot compile until identity/scim/rbac (T03/T04/T05) exist.

---

## §7 Per-invariant ledger

The seven invariants hold **by federating above the seam**, not by new mechanism below it. (Verified against the real code: the trust line is a single pre-Turn `Permit` check; the Ledger keys by arbitrary string; `OnUsage` is purely observational; the policy gate default-denies a nil approver; the integrator never lands.)

| Invariant | How EXT-05 preserves it |
|---|---|
| **I1** frozen contract | `backend.Task`/`Result`/`CodingBackend.Run` are **untouched** — EXT-05 touches *authorization* and *attribution*, never the loop. `Principal`/`Grant`/`Usage` are new leaf types; the federated authorizer satisfies the **existing** `server.Authorizer` (`Permit(string) bool`, `internal/server/server.go:60-63`). `internal/channel/channel.go` is untouched. |
| **I2** verifier sole authority | EXT-05 adds **no** code path that ships work on a self-report and **no** auto-land. RBAC may gate *who may request* a `PromoteToBase`, but `policy.GateStructured` still consults the human approver and **still default-denies a nil approver** (`internal/policy/gateaction.go:86-99`); the verifier's verdict and the integrator's never-land are unchanged. |
| **I3** no ambient authority — **federated identity scoped & stored** | Every standing IdP credential (OIDC client secret, SCIM bearer, SAML signing cert) lives in `secrets.SecretStore` **by name** (`internal/secrets/secrets.go:19-24`), resolved server-side in `enterprise.Build` (T13), used only in the verification/exchange call, **never written to disk in plaintext, never logged, never in a prompt, never given to the model.** Config carries **refs only** (T12), never values. Error strings reference names only. The federation *grants no broad authority* — it only authenticates *who* a principal is; the deny-default `Permit` seam below is unchanged. |
| **I4** sandboxed execution | EXT-05 adds **no** new model-emitted execution path. The SSO/SCIM HTTP surfaces are harness-side control-plane I/O (auth verification, directory sync), not model execution; they bind a private interface (T04, T14), are off-by-default, and never run model-emitted commands. The sandbox is untouched. |
| **I5** append-only audit — **`unauthorized_*` stays** | The **existing** `unauthorized_command` / `unauthorized_gate` Kinds are reused verbatim for every denial (T06, T11) — no new denial Kind, so the audit history is continuous (`internal/channel/authorized.go:52-54,76-78`; `internal/server/server.go:158-160`). New Kinds (`sso_verify`/`scim_provision`/`scim_deprovision`/`rbac_deny`) are append-only, **metadata-only, redacted** — no token, no claim value, no PII beyond the opaque subject. The log stays replayable; a broken chain still HALTS. |
| **I6** zero-dep core | All new leaves are stdlib-only (`CGO_ENABLED=0`): OIDC is `crypto/rsa`+`crypto/ecdsa`+`encoding/json`+`net/http` (no JOSE module); SCIM/JWKS/console are `net/http`+`encoding/json`; the TUI is `//go:build tui`-isolated (zero Charm in default). **The single possible module is SAML XML-dsig** — and even that has a stdlib path; any dep is one CGO-free, vendored, audited XML-dsig lib justified in PR+CHANGELOG, or SAML stays gated and OIDC-only ships (§8). |
| **I7** untrusted-as-data — **the per-message trust line** | The single load-bearing promotion to principal trust is **unmoved**: `server.intake` calls `Auth.Permit(req.Sender)` BEFORE any message becomes a `Turn`, one thread is pinned to one principal, a nil Auth is deny-all (`internal/server/server.go:16-24,152-173`). EXT-05 changes only the **source** of the allow-set (a directory, not a static list), never the *position* of the check. SSO assertions, SCIM payloads, and IdP claims are **untrusted data** verified before use; an unverifiable assertion / unknown subject / inactive user / out-of-scope tenant ⇒ **deny (fail-closed)**. The console/dashboard projects **only trusted fields** (redacted, escaped) — never a model-authored value. |
| **Never-land** | EXT-05 constructs **no** new `GateActionType` and **no** auto-land. The only base-branch land remains the one gated `policy.GateAction{PromoteToBase}` through the human approver (nil ⇒ deny). RBAC `MayPromote` *narrows* who may **request** it; it never widens or bypasses the gate. |

**EXT boundary (thesis).** EXT-05's control plane is **single-host, single-process, one local SQLite store.** Tenancy is a *logical* keyspace prefix (the swarm's run-isolation discipline), **not** a multi-host control plane and **not** a shared/networked directory database. The line it does **not** cross — multi-host tenant state, a remote control plane leasing identity across hosts, a hosted multi-tenant SaaS dashboard endpoint — is `EXT-01`/`EXT-02` and is explicitly out of scope (§10). A `deps_test`-style guard asserts the new leaves import no orchestrator (`agent`/`super`/`project`) and the SCIM/JWKS HTTP is a bounded client/private-bind listener, never a public multi-host RPC plane.

---

## §8 Module justifications (SAML / OIDC / SCIM — prefer stdlib, CGO-free)

The §0 gate criterion 4 and `I6` demand: default to stdlib; any module justified in PR **and** CHANGELOG; never break `CGO_ENABLED=0`. The codebase precedent is hand-rolling crypto to stay stdlib (PBKDF2 in `internal/secrets/file.go`).

| Capability | Decision | Justification |
|---|---|---|
| **OIDC SSO** (ID-token validation) | **Stdlib only — no module.** | An OIDC ID token is a JWT; validation is signature verification (`crypto/rsa`/`crypto/ecdsa`), claim checks (`encoding/json`), and JWKS fetch (`net/http`). This is the same "hand-roll to stay stdlib" discipline the secrets vault uses. `alg:none` and symmetric algs are rejected hard. No JOSE module. (`I6` clean.) |
| **SCIM provisioning** | **Stdlib only — no module.** | SCIM 2.0 is JSON over HTTP. `net/http` + `encoding/json` (+ `DisallowUnknownFields`, size caps, constant-time bearer compare) cover the `/Users` + `/Groups` intake. No SCIM SDK. (`I6` clean.) |
| **RBAC / tenancy / usage / console** | **Stdlib only — no module.** | Pure Go: maps, a closed role enum, string-prefix keyspace, a concurrency-safe tally over the existing Ledger, stdlib text/json rendering. The rich admin console is Charm behind `//go:build tui` — the **already-sanctioned** `I6` exception (zero Charm in the default binary). |
| **SAML SSO** (assertion + XML-dsig) | **Stdlib path first; at most ONE CGO-free vendored XML-dsig dep, or SAML stays gated.** | XML-dsig is the only genuinely hard piece (canonicalization + enveloped-signature verification + signature-wrapping/XXE defense). The **default attempt is stdlib** (`encoding/xml` with DTD/entity expansion disabled, a constrained namespace-explicit parse, allowlisted canonicalization, `crypto/x509`+`crypto/rsa` digest check, signed-element binding). **If** that cannot clear security review within EXT-05-T03, exactly **one** CGO-free, vendored, audited XML-dsig library may be added — justified in the PR + CHANGELOG, pinned, `CGO_ENABLED=0`-verified across the release matrix — **never** a broad SAML SDK that pulls a dependency tree. **If no acceptable option clears review, SAML stays gated and OIDC-only ships** (OIDC covers the large majority of modern IdPs; SAML can follow as a separate gated increment). The module decision is recorded in EXT-05-T03's PR and EXT-05-T16's CHANGELOG. |

**Net module budget:** **zero in the default path** (OIDC + SCIM + everything else is stdlib). At most **one** CGO-free XML-dsig dep, only for SAML, only if the stdlib path fails review, fully justified — and SAML is independently gateable so the default binary never depends on it.

---

## §9 Default-off byte-identical proof

The default `nilcore` binary must be **byte-identical** with the control plane absent, and the flat allowlist (`channel.Authorized`, empty-by-default deny-all) must remain the **default and only** authorizer when no IdP is configured. This is a proof obligation, not an assertion — established exactly as the swarm plan establishes its default-off proof.

1. **One config gate, nil by default.** `Config.Enterprise *enterprise.Config` is `omitempty` and **nil by default** (T12), the same shape as the already-shipped `Config.Pool` (`internal/onboard/onboard.go:66`). Every existing `config.json` parses unchanged under `DisallowUnknownFields`. **Test:** an existing config (no `enterprise` block) round-trips parse/Save/Load.
2. **One branch at the single construction site.** At `cmd/nilcore/main.go:1512` the authorizer is selected: `cfg.Enterprise == nil` ⇒ the **existing** `channel.NewAuthorized(bot, allow, log)` path runs **unchanged**, and `enterprise.Build` is **never called** (T14). **Test:** a tripwire fake on `enterprise.Build` stays untouched when `cfg.Enterprise == nil`; the constructed authorizer is `*channel.Authorized`.
3. **No existing package imports the new leaves.** `internal/identity`/`scim`/`rbac`/`directory`/`tenancy`/`usage`/`console`/`enterprise` are imported **only** by `cmd/nilcore` (and the config type by `internal/onboard`). **Test:** an import-graph test (`go list -deps`) asserts no existing package imports a control-plane leaf.
4. **No global-side-effect `init()`.** The new leaves contain **no `init()` with global side effects**, so merely linking them cannot change behavior. **Test:** an `init()`-free assertion over the new leaves.
5. **The default dispatch path reaches no new arm.** `nilcore` / `nilcore run` / `nilcore chat` / `nilcore serve` (without an enterprise config) reach neither the federated authorizer, the SCIM listener, nor the `case "admin"` arm. The only added `main.go` surface is one branch + one dispatch case + usage lines — exactly the byte-identity posture the `swarm`/`report`/`schedule`/`watch` cases already establish (`docs/SWARM.md` §9). **Test:** the default dispatch path is exercised and reaches none of the new code; `nilcore serve` still fatals on an empty allow-set (deny-default) whichever authorizer is active.
6. **The TUI is `//go:build tui`-isolated.** The default (`!tui`) build links **zero** Charm (T09). **Test:** `go list -deps` on the default tag shows no Charm package.

**Together:** with `Config.Enterprise == nil` (the default), the binary takes the identical code path it takes today — flat allowlist, deny-default, no IdP, no SCIM, no dashboard — and is byte-identical. The control plane is reachable **only** when an operator explicitly configures it, and removing it is deleting the leaves + the one config block.

---

## §10 Risks

- **IdP federation is the highest-value attack surface in the whole project.** A flaw in OIDC/SAML verification is a full authentication bypass — it would let an attacker mint a principal that `Permit` accepts. **Mitigation:** verifiers fail closed on every malformed/expired/wrong-aud/`alg:none`/unsigned input; a dedicated attack-vector test table (signature-wrapping, XXE, `alg` confusion, audience/issuer mismatch, clock skew); the §0 gate criterion 1's security review is mandatory before merge; SAML's XML-dsig is the riskiest piece and may stay gated (OIDC-only) if it cannot clear review (§8).
- **Standing identity credentials multiply blast radius (`I3`).** An OIDC client secret / SCIM bearer leak grants an attacker the directory. **Mitigation:** secrets stay in `SecretStore` by name, server-side only, never to the model/log/prompt; refs-only config; constant-time bearer compare; the SCIM listener is off-by-default and binds a private interface, never `0.0.0.0`.
- **Federation could silently move the trust line (`I7`).** The whole design is "federate above the seam"; a careless impl could let an IdP-derived flag bypass `Permit`. **Mitigation:** the federated authorizer satisfies the **same** `Permit(string) bool` and is the **only** authorizer the server ever calls; deny-default and one-thread-one-principal are unchanged and tested; serve still fatals on an empty allow-set.
- **A served/multi-tenant dashboard endpoint is a slippery slope toward a hosted SaaS (`EXT-02`).** v1's dashboard is a **read-only local projection** (text/json/TUI), explicitly **not** an HTTP-served endpoint. **Mitigation:** the `docs/ARCHITECTURE.md` note (T16) pins this so a follow-on PR cannot quietly add an HTTP scoreboard/dashboard; `console` imports nothing that can serve or command.
- **Tenancy as a logical prefix is weaker than physical isolation.** A bug in the prefix/scope check could leak one tenant's threads/usage to another. **Mitigation:** the `tenancy` leaf's isolation guards are property-tested (no two tenants share a key namespace); the cross-tenant `SameTenant` denial is exercised; tenancy rides the same ID-prefix discipline the swarm already proves. Physical (multi-store/multi-host) isolation is `EXT-01`, out of scope.
- **RBAC scope creep.** A dynamic policy DSL would itself be a new authority surface. **Mitigation:** the role set is **closed and small** (`viewer`/`operator`/`approver`/`admin`), fixed in code; unknown group ⇒ no role ⇒ deny; no runtime-editable policy in v1.
- **Module risk on SAML (`I6`).** An XML-dsig dependency could pull a tree or break `CGO_ENABLED=0`. **Mitigation:** stdlib-first; at most one CGO-free vendored audited dep, justified in PR+CHANGELOG, release-matrix-verified — or SAML stays gated and OIDC-only ships.
- **Gate-decision risk (the thesis itself).** EXT-05 turns a single-operator binary into one that *can* serve a multi-tenant org — pure surface growth orthogonal to coding quality (`docs/ROADMAP-EXTERNAL-INFRA.md` §6). **Mitigation:** this is precisely what the §0 gate exists to decide; the plan stays unbuilt until a human owner records the thesis decision. Default-off byte-identity (§9) guarantees that even after EXT-05 ships, NilCore-the-tiny-harness is unchanged for everyone who never opts in.

---

*EXT-05 is allowed to exist only because it adds *who* and *how-much-by-whom* without touching *whether the verifier governs*. The seven invariants hold by federating the IdP above a trust line that does not move — the flat, deny-default allowlist stays the default, the verifier keeps the only vote on "done," the integrator never lands, and the model never sees an identity credential. BLOCKED behind the §0 gate until a human records the thesis decision.*
