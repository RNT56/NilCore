# EXT-02 — Full-stack preview / hosting / one-click deploy (GATED · DOCS-ONLY blueprint)

> **STATUS: BLOCKED behind the §0 gate.** This is the *ready-when-the-gate-clears* execution plan
> for `docs/ROADMAP-EXTERNAL-INFRA.md` item **EXT-02**. It is **not** an eligible task under
> `CLAUDE.md` §5: no `EXT-02-T##` branch may be opened, and none of its task specs may be promoted
> into `docs/TASKS.md`, until a **human owner records the thesis decision** in §0 and the
> serialized contract PR that promotes the rows lands. Until then this file is a **boundary to
> defend**, exactly like the rest of `ROADMAP-EXTERNAL-INFRA.md`.

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/ROADMAP-EXTERNAL-INFRA.md §3` →
`docs/SWARM.md` (the depth template this plan matches) → this file.

---

## Table of contents

- [Summary](#summary)
- [§0 The gate — what must be true before any EXT-02 task is written](#0-the-gate)
- [§1 As-is: the seams this reuses (sourced)](#1-as-is-the-seams-this-reuses)
- [§2 The boundary — what is NET-NEW vs reuse](#2-the-boundary)
- [§3 Architecture (web surface + gated provisioning/deploy + preview-in-sandbox + secret confinement)](#3-architecture)
- [§4 The task DAG](#4-the-task-dag)
- [§5 Per-task specs (EXT-02-T01 … EXT-02-T14)](#5-per-task-specs)
- [§6 Parallel-execution wave map, critical path & serialization](#6-parallel-execution-wave-map)
- [§7 Per-invariant ledger (I1–I7 + never-land; deploy is irreversible → gated)](#7-per-invariant-ledger)
- [§8 Module justifications (CGO-free) — stdlib-only proof](#8-module-justifications)
- [§9 Default-off byte-identical proof](#9-default-off-byte-identical-proof)
- [§10 Risks](#10-risks)

---

## Summary

EXT-02 is the **"prompt → live, shareable URL"** loop — generate an app, render a **live preview**
as code is written, **provision** managed Postgres/auth/storage by prompt, and **one-click deploy**
to a hosted URL. It is the capability behind Replit, Lovable, Bolt, and v0. NilCore deliberately
does **not** have it: `internal/server` is a **chat-channel intake, not a web/HTTP host**
(`internal/server/server.go:1-14`), the only inbound `http.Server` is the egress proxy
(`internal/policy/egress_proxy.go:24`), and "deploy" exists **only** as an enumerated, gated,
irreversible action with no code path that performs it (`policy.Deploy`,
`internal/policy/gateaction.go:34`).

The plan builds the capability **without bending a single invariant**: the **preview** runs the
generated app inside the **existing sandbox** (`sandbox.Container` + the egress proxy) and is
fronted by a **net-new web-surface package** (separate from `internal/server`, which stays a chat
intake); **provisioning and deploy are harness-side calls behind gated `policy.GateAction`s**
(`Deploy` is already enumerated; nil approver default-denies, `internal/policy/gateaction.go:94-101`)
— **never a model-emitted command and never given to the model** (I3); **behavioral verification**
(`verify.BrowserVerifier`, `internal/verify/browser.go`) gates *"the deployed thing actually
works"* **before** the irreversible deploy lands; and the **integrator never lands to base** — the
only base land remains a gated `policy.GateAction{PromoteToBase}` (`internal/integrate/integrate.go:12-17`).
Provider credentials live **only** in `secrets.SecretStore`, injected **only** into a server-side
sandboxed runtime via `box.ExecWithEnv` **by name** (`internal/sandbox/sandbox.go:28-34`) — never
the frontend, never the model context.

Everything is a **new leaf package** or an **additive seam** over a single new `preview`/`deploy`
subcommand; the default `nilcore` binary stays **byte-identical** with the feature absent (§9).

---

## §0 The gate

An `EXT-02-T##` item is **not** an eligible task in the `CLAUDE.md` §5 sense. Before a single line
is written, **all** of the following must hold and be recorded in the serialized PR that promotes
the item into `docs/TASKS.md` (`docs/ROADMAP-EXTERNAL-INFRA.md §0`):

1. **A recorded thesis decision (the gate itself).** A human owner explicitly decides that
   NilCore's identity may expand from "tiny self-hosted coding *harness*" to "app-building
   *platform* with hosting obligations (uptime, SSL, tenancy)." This is the largest identity change
   on the roadmap (`docs/ROADMAP-EXTERNAL-INFRA.md:75`) and is **not delegable to the agent** — it
   is exactly the irreversible, outward-facing class the whole design reserves for a human. The
   decision **must name an owner for the hosting/provisioning backend's uptime and security**
   (`docs/ROADMAP-EXTERNAL-INFRA.md:77`).
2. **Invariants survive, not bypassed (§7).** Each EXT-02 task shows concretely that it **extends**
   I1–I7. Load-bearing here: **I3** — *"wire up Stripe/Supabase by prompt" is the direct inverse of
   "secrets are never given to the model"* (`docs/ROADMAP-EXTERNAL-INFRA.md:73`). Every provider
   credential is scoped, gated, held in `secrets.SecretStore`, injected only into a **server-side
   sandboxed runtime**, and **never** reaches the frontend or the model.
3. **The verifier still governs (I2).** No deployed app ships on a self-report. The project's own
   checks (`verify.Verifier.Check`) and a **behavioral** browser gate (`verify.BrowserVerifier`,
   `internal/verify/browser.go:33-44`) decide "done"; deploy is `policy.Classify`-irreversible
   (`internal/policy/policy.go`), routed through the **human gate**; the integrator's never-land
   guarantee (`internal/integrate/integrate.go:12-17`) is preserved — the only base land remains a
   gated `policy.GateAction{PromoteToBase}` through the human approver
   (`internal/policy/gateaction.go:94-101`).
4. **Dependency budget justified (I6, §8).** Every provisioning/deploy provider is spoken over
   **stdlib `net/http` + `encoding/json`** — exactly the pattern `internal/forge` already uses for
   the GitHub REST API (`internal/forge/forge.go:11-12` — *"Stdlib only … no client module"*). No
   cloud SDK module is added; `CGO_ENABLED=0` is preserved (`.github/workflows/release.yml`). Any
   exception requires PR + CHANGELOG justification.
5. **Default-off, opt-in, reversible to remove (§9).** The default `nilcore` binary is
   **byte-identical** with the feature absent; `preview`/`deploy` are new subcommands, all logic in
   new files, no existing package imports a new leaf, no new `init()` with global side effects.

If any of these cannot be met, EXT-02 stays on the roadmap, **unbuilt**.

---

## §1 As-is: the seams this reuses

The most important fact: **none of the trust machinery is net-new.** EXT-02 *composes* shipped
seams; the only genuinely new code is a web surface, two harness-side provider clients (stdlib
HTTP), a preview runner over the existing sandbox, and the cmd wiring.

| Seam (sourced) | What EXT-02 reuses it for |
|---|---|
| `sandbox.Container` + `ExecWithEnv` (`internal/sandbox/sandbox.go:28-34, 159-176`) | **Run the preview** and all provisioning/deploy build steps **in-box**; inject provider creds **by name** for that single invocation, never logged (`sandbox.go:159-160`). |
| `sandbox.Container.AllowEgressVia` + `policy.EgressProxy` (`internal/sandbox/sandbox.go:91-99`, `internal/policy/egress_proxy.go:24,104-106,181`) | The preview's outbound network is the **allowlist proxy** with SSRF defense (`egress_proxy.go:33-43`) — the preview cannot reach `169.254.169.254`, localhost, or the internal net. `ProxyURL(addr)`. |
| `policy.GateAction{Deploy}` (`internal/policy/gateaction.go:34`) + `GateStructured` (`gateaction.go:94-101`) | **Provision** and **deploy** are gated structured actions; **nil approver default-denies** (`gateaction.go:98-100`). `Deploy` is **already enumerated** — EXT-02 constructs **no** new `GateActionType`. |
| `policy.Classify` flags `deploy` Irreversible (`internal/policy/policy.go`) | Deploy routes through the human gate (`docs/ROADMAP-EXTERNAL-INFRA.md:74`). |
| `verify.BrowserVerifier` (`internal/verify/browser.go:23-44`) | **Behavioral gate**: the preview/staged app is navigated headless in-box; **fail-closed** (no browser binary ⇒ red, `browser.go:20-22`) — gates "the deployed thing works" **before** the irreversible deploy. |
| `verify.Verifier` / `verify.Detect` (build/test ladder) | The project's own checks remain the authority on "done" before a deploy candidate is even staged (I2). |
| `integrate.Integrator` (`internal/integrate/integrate.go:12-17`) | Folds verifier-green generation branches; **NEVER pushes / lands to base** — only returns the green tree. The base land is a separate gated `PromoteToBase`. |
| `forge.GatedOpen` (`internal/forge/forge.go:60-71`) | The **template** for a harness-side, token-from-SecretStore, gated outward call: `GatedOpen(ctx, ask, …, prepare)` gates first, runs the side-effecting `prepare` **only on approval**. EXT-02's provision/deploy mirror this exactly. |
| `secrets.SecretStore` (`internal/secrets/secrets.go:18-24`) | Holds every provider token; *"error messages reference the secret name only"* (`secrets.go:17`); never logged/prompted/to the model (I3). |
| `worktree` + `worktreefs` (`O_NOFOLLOW`, atomic temp+rename) | Per-session disposable worktree; host-side artifact I/O for the generated app. |
| `eventlog.Log` (append-only, hash-chained, redacted) | Every generation step, verify, provision-gate, deploy-gate, and deploy is recorded and replayable (I5). |
| `roster` role workers ("configuration over the one loop") | The app-generator is a role/profile over the single `backend.CodingBackend.Run` loop (I1) — no contract change. |
| `server.Authorizer` / `channel.Authorized.Permit` (`internal/server/server.go:16-24,60-63`) | The web surface reuses the **same** deny-default per-principal trust line for who may drive a session — sourced static today; the directory/SSO source is **EXT-05**, out of scope. |

**Crucially:** `internal/server` is a chat-channel intake (`server.go:1-14`), **not** a web host.
The preview/deploy web surface is **NET-NEW and separate** — it never widens `internal/server`.

---

## §2 The boundary

| NET-NEW (the only code this plan writes) | REUSE (shipped, untouched) |
|---|---|
| `internal/webpreview/` — the HTTP web surface that **proxies** a sandboxed preview app to a browser, holds session/preview state, streams generation progress. **Separate from `internal/server`.** | `sandbox`, `policy.EgressProxy`, `worktree`, `eventlog`. |
| `internal/provision/` — harness-side, stdlib-HTTP, gated clients to provision managed DB/auth/storage (token from SecretStore, injected only into the in-box runtime). | `secrets.SecretStore`, `policy.GateStructured`, the `forge.GatedOpen` pattern. |
| `internal/deploy/` — harness-side, stdlib-HTTP, gated one-click deploy to a hosting backend; **behavioral verify before the irreversible deploy**. | `policy.GateAction{Deploy}`, `verify.BrowserVerifier`, `integrate.Integrator`, `policy.Classify`. |
| `internal/preview/` — runs the generated app **inside `sandbox.Container`** on an in-box port, health-checks it, exposes a read handle the web surface proxies. | `sandbox.Container.AllowEgressVia`, `verify.Detect`. |
| `internal/secretbind/` — the **one** confinement boundary: resolves a provider token by name from SecretStore and returns an `env map[string]string` consumed **only** by `box.ExecWithEnv`; a compile-time projection guarantees no token field is ever serialized to the web surface or a model prompt. | `secrets.SecretStore`, `sandbox.ExecWithEnv`. |
| `cmd/nilcore/preview.go`, `cmd/nilcore/deploy.go` + two new dispatch cases + usage lines in `main.go`. | the cmd dispatch pattern. |
| `internal/onboard/onboard.go` additive `Preview *PreviewConfig` (config schema, serialized). | `onboard.Config` strict-decode. |

**The line EXT-02 does NOT cross (stays future / other EXT items):** multi-tenant served
dashboards or per-tenant identity (= **EXT-05**); a managed remote fleet running these sessions
across hosts (= **EXT-01**); centralized cross-fleet secret distribution (= **EXT-06**). A
`deps_test` guard keeps every new leaf free of those couplings.

---

## §3 Architecture

```
  nilcore preview --goal "a todo app with login" [--provision postgres,auth] [--deploy <target>]
        │
        ▼
  buildPreview (cmd/nilcore/preview.go)  ── composes the leaves; ONE eventlog, ONE SecretStore handle (never copied to the model)
        │
        ├─ worktree.New (disposable)  ──►  roster app-generator role  ──►  backend.CodingBackend.Run  (I1: unchanged contract)
        │        (generation streams progress events; each turn re-verified by verify.Verifier — I2)
        │
        ├─ internal/preview  ──►  sandbox.Container{Network: via EgressProxy}.ExecWithEnv("<dev-server>")  ── runs the app IN-BOX on an in-box port
        │        health-check loop → ready
        │                                            ▲
        ├─ internal/webpreview (NET-NEW http.Server) │  reverse-proxies the in-box preview to the operator's browser
        │        Authorizer (deny-default) ──────────┘  + streams generation progress (SSE/WS over stdlib)
        │        *** holds NO provider credential — the frontend never sees a token (I3) ***
        │
        ├─ internal/provision (gated)   ─ GateStructured(GateAction{Deploy, Detail:"provision postgres"}, ask) ─┐
        │        on APPROVAL only: stdlib-HTTP call, token from secretbind, result = a CONNECTION HANDLE        │ nil approver
        │        the conn string is injected into the in-box runtime by NAME (box.ExecWithEnv), never returned  │ ⇒ DENY
        │        to the web surface or the model                                                                │
        │                                                                                                       ▼
        └─ internal/deploy (gated, LAST)                                                              human approver (policy.Approver)
                 1. verify.Verifier.Check (project checks) ──── I2: must be green
                 2. verify.BrowserVerifier.Check (behavioral) ── "the deployed thing works" — fail-closed
                 3. integrate.Integrator (fold green branches) ── NEVER lands to base
                 4. GateStructured(GateAction{Deploy}, ask) ──── irreversible → human gate; nil ⇒ deny
                 5. on APPROVAL only: stdlib-HTTP deploy call (token from secretbind) → hosted URL
                 6. eventlog append (redacted) ──── I5
```

### 3.1 The new web-surface package & import sets

`internal/webpreview` is a **leaf** importing only: `sandbox` (read a preview handle), `policy`
(the `Authorizer` shape + egress), `eventlog`, `emit`, `guard` (wrap any inbound text as data, I7),
and stdlib `net/http`/`net/http/httputil` (the reverse proxy). It imports **no** orchestrator
(`agent`/`super`/`project`), **no** `provision`/`deploy`, and **never** `secrets` — so a token
cannot structurally reach it. A `deps_test.go` enforces this.

`internal/preview` imports `sandbox`, `policy` (egress), `verify` (Detect), stdlib. `internal/provision`
and `internal/deploy` import `policy`, `secretbind`, `verify`, `integrate`, `eventlog`, stdlib
`net/http`+`encoding/json` — **no SDK module** (I6, §8).

### 3.2 Provisioning & deploy as gated GateActions

`Deploy` is **already** an enumerated `GateActionType` (`internal/policy/gateaction.go:34`).
Provisioning **and** deploy both construct a `policy.GateAction{Type: Deploy, Detail: "..."}` and
call `policy.GateStructured(action, ask)`:

- `GateStructured` returns `false` when `ask == nil` (`gateaction.go:98-100`) — **default-deny, no
  ambient authority for an irreversible step.**
- The side-effecting API call runs **only after** the approver returns true — the exact
  `forge.GatedOpen(ctx, ask, …, prepare)` discipline (`forge.go:64-71`): gate first, mutate second.
- EXT-02 constructs **no new `GateActionType`** — the closed set in `gateaction.go:27-41` is honored
  (a new boundary action *"must be added deliberately rather than inferred from text"*,
  `gateaction.go:23-24`; EXT-02 needs none).

### 3.3 Preview-in-sandbox

The generated app runs inside `sandbox.Container` (`internal/sandbox/sandbox.go:40-64`), hardened
(`--cap-drop=ALL`, `--read-only`, no-new-privileges, `sandbox.go:107-126`), with the worktree the
single writable mount (I4). Network is **default-deny** (`Network:"none"`, `sandbox.go:78`) unless
the operator opts the preview into egress via `AllowEgressVia(policy.ProxyURL(addr))`
(`sandbox.go:91-99`) — and even then the SSRF-defended proxy (`egress_proxy.go:33-43`) blocks
localhost/metadata/private ranges. The web surface reaches the preview via a **read handle**, not by
granting the model a listening socket.

### 3.4 The secret-confinement boundary (`internal/secretbind`) — the I3 spine

This is the load-bearing mechanism. Provider creds **never** leave the server-side sandboxed
runtime:

- `secretbind.Bind(store secrets.SecretStore, names []string) (Env, error)` resolves each named
  token from the SecretStore and returns an opaque `Env` whose **only** consumer is
  `box.ExecWithEnv(ctx, cmd, env.forBox())` — a method that returns `map[string]string` and exists
  on **no** path reachable by `webpreview` or a model prompt.
- `Env` has **no exported field carrying a value** and **no `String()`/`MarshalJSON`** — a
  compile-time projection (`var _ json.Marshaler` is *deliberately absent*; a test asserts
  `json.Marshal(env)` errors or yields no value) so a token can never be serialized into the web
  response or an event Detail.
- Keyed provider calls inject by **name** exactly as the shipped finance pack does
  (`box.ExecWithEnv` with `$NILCORE_*_KEY`) — the model supplies no command and never sees the
  value. The deploy/provision HTTP calls (when made harness-side rather than in-box) read the token
  for a single per-request header and discard it, mirroring `forge.NewClient` (*"held only for
  per-request headers (I3)"*, `forge.go:48-49`).
- A provisioned connection string is treated as a **secret**: stored back into the SecretStore by
  name and injected into the in-box runtime by name — it is **never** returned to `webpreview` or
  the model.

---

## §4 The task DAG

**Namespace `EXT-02-T01 … EXT-02-T14`.** One task = one branch (`task/EXT-02-T0x`) = one PR. Owns
sets are **pairwise disjoint** (package dir = unit of ownership). The confinement + gating
foundation (T01–T05) lands **before** the orchestration tasks (T08–T13), so no surface can be wired
without its trust spine present.

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-02-T01 | Secret-confinement boundary leaf | — | `internal/secretbind/` | new leaf; I3 spine |
| EXT-02-T02 | Preview runner (app-in-sandbox) leaf | — | `internal/preview/` | new leaf |
| EXT-02-T03 | Provisioning client leaf (gated, stdlib-HTTP) | EXT-02-T01 | `internal/provision/` | new leaf |
| EXT-02-T04 | Deploy client leaf (gated, behavioral-verify-before-deploy) | EXT-02-T01 | `internal/deploy/` | new leaf |
| EXT-02-T05 | Behavioral deploy-gate verifier glue | — | `internal/deploy/verify/` | new sub-leaf; ∥ T04 prep |
| EXT-02-T06 | Web-surface reverse-proxy + progress stream leaf | EXT-02-T02 | `internal/webpreview/` | new leaf; NET-NEW web host |
| EXT-02-T07 | Web-surface auth + session map | EXT-02-T06 | `internal/webpreview/` (whole pkg) | serial after T06 |
| EXT-02-T08 | App-generator role + profile | — | `internal/roster/roster.go`, `internal/roster/worker.go` | roster is a leaf (not frozen) |
| EXT-02-T09 | `onboard.Config.Preview` field + Validate | EXT-02-T01, EXT-02-T03, EXT-02-T04 | `internal/onboard/onboard.go` | **contract (config schema)** — serialized |
| EXT-02-T10 | `nilcore preview` subcommand + buildPreview | EXT-02-T06, EXT-02-T07, EXT-02-T08, EXT-02-T02 | `cmd/nilcore/preview.go` | new file |
| EXT-02-T11 | `nilcore deploy` subcommand + buildDeploy | EXT-02-T03, EXT-02-T04, EXT-02-T05 | `cmd/nilcore/deploy.go` | new file |
| EXT-02-T12 | Dispatch wiring (`case "preview"` / `case "deploy"`) | EXT-02-T10, EXT-02-T11, EXT-02-T09 | `cmd/nilcore/main.go` | **serialized cmd-wiring** (only main.go editor) |
| EXT-02-T13 | Default-off byte-identity + import-graph guard tests | EXT-02-T12 | `cmd/nilcore/extoff_test.go` | solo; proves §9 |
| EXT-02-T14 | Docs + CHANGELOG promotion | EXT-02-T13 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs)** — serialized last |

> `internal/webpreview` is the **whole package** the Owns unit for T06/T07 (not per-file), so the
> work-selection rule correctly forbids both open at once. T03/T04 depend on **T01** (they import
> `secretbind`) so each is independently `make verify`-green in isolation. `cmd/nilcore/main.go` is
> a serialized contract surface — only T12 edits it (one `case "preview"` + one `case "deploy"` +
> usage lines).

---

## §5 Per-task specs

#### EXT-02-T01 — Secret-confinement boundary leaf · I3 spine
- **Goal:** the one place provider creds are resolved and the one channel they may flow through — a
  resolver that returns an opaque `Env` consumable **only** by `box.ExecWithEnv` (or a single
  per-request HTTP header), structurally un-serializable to the web surface or a model prompt.
- **Depends on:** — (reuses `secrets`, `sandbox`).
- **Owns:** `internal/secretbind/` (`secretbind.go`, `secretbind_test.go`, `deps_test.go`).
- **Acceptance:** `Bind(store secrets.SecretStore, names []string) (Env, error)` resolves each name
  via `store.Get` (miss ⇒ error referencing the **name only**, mirroring `secrets.go:17`); `Env`
  has **no exported value field**, **no `String()`**, **no `MarshalJSON`** — `forBox() map[string]string`
  is the **only** accessor and is package-internal to box callers; `Header(name) (string, bool)` for
  the single-request harness-HTTP path returns a value used once and not retained; a compile-time
  `var _ fmt.Stringer = Env{}` is **deliberately absent** and a test asserts `fmt.Sprintf("%v", env)`
  and `%+v` contain **no** token bytes; `deps_test.go` asserts `secretbind` is imported by **only**
  `provision`/`deploy`/`cmd` (never `webpreview`).
- **Verify:** `make verify`; `go test -race ./internal/secretbind/...`: `Bind` over a fake store
  round-trips into `forBox()`; a missing name errors with the name but **not** any value; `%v`/`%+v`/
  `json.Marshal` over `Env` emit **no** token; `deps_test` enforces the importer allowlist.
- **Notes:** this is the EXT-02 I3 guarantee made executable. No new module (stdlib only).

#### EXT-02-T02 — Preview runner (app-in-sandbox) leaf
- **Goal:** run the generated app **inside `sandbox.Container`** on an in-box port, health-check to
  ready, expose a read handle the web surface proxies — no listening socket ever handed to the model.
- **Depends on:** — (reuses `sandbox`, `policy` egress, `verify.Detect`).
- **Owns:** `internal/preview/` (`preview.go`, `preview_test.go`, `deps_test.go`).
- **Acceptance:** `Run(ctx, box sandbox.Sandbox, spec RunSpec) (Handle, error)` where `RunSpec`
  names a **pack-allowlisted** dev-server command (or one derived from `verify.Detect(box.Workdir())`
  — the same conservative ladder), runs it via `box.Exec`/`ExecWithEnv` in-box; egress is
  default-deny unless `spec.Egress` opts in via `box.AllowEgressVia(policy.ProxyURL(addr))`
  (`sandbox.go:91-99`); a health-check loop (bounded, ctx-honoring) reports ready; `Handle{URL (in-box),
  Stop func()}`; a missing/failed dev server ⇒ error, never a false-ready; the model supplies **no**
  free command.
- **Verify:** `make verify`; `go test ./internal/preview/...` with a stub sandbox (canned `Result`):
  ready on healthy exit pattern, error on non-zero/timeout; egress off by default (no proxy wiring
  unless opted); `verify.Detect` reuse asserted; `deps_test` asserts no orchestrator/net-server import.
- **Notes:** mirrors the shipped browser/command verifier's fail-closed posture.

#### EXT-02-T03 — Provisioning client leaf (gated, stdlib-HTTP)
- **Goal:** provision a managed DB/auth/storage by prompt — **harness-side**, token from
  `secretbind`, **gated** behind the human approver, result a connection handle injected into the
  in-box runtime **by name** (never returned to the web surface or model).
- **Depends on:** EXT-02-T01. **Owns:** `internal/provision/` (`provision.go`, `provision_test.go`,
  `deps_test.go`).
- **Acceptance:** `GatedProvision(ctx, ask policy.Approver, req Request, bind secretbind.Env) (Handle, bool, error)`
  constructs `policy.GateAction{Type: policy.Deploy, Detail: "provision " + req.Kind}` and calls
  `policy.GateStructured(action, ask)` **first**; on `false` (incl. `ask==nil`, `gateaction.go:98-100`)
  returns `opened=false` **without** any API call; on `true` makes a single `net/http`+`encoding/json`
  request with the token as a **per-request header** (mirroring `forge.go:48-49`), discards the
  token; the returned **connection string is stored back into the SecretStore by name** (never in
  `Handle` as plaintext beyond a name reference); no SDK module; `BaseURL` overridable for tests
  (httptest) and self-hosted backends (mirroring `forge.Client.BaseURL`).
- **Verify:** `make verify`; `go test ./internal/provision/...` with `httptest` + fake approver +
  fake store: nil approver ⇒ no request, `opened=false`; deny ⇒ no request; approve ⇒ exactly one
  request with the header set; the token never appears in any error/log string; `deps_test` asserts
  no model/web-surface import and stdlib-HTTP only.
- **Notes:** the `forge.GatedOpen` discipline applied to provisioning. **Contract-clean:** no new
  `GateActionType`.

#### EXT-02-T04 — Deploy client leaf (gated, verify-before-deploy)
- **Goal:** one-click deploy to a hosted URL — **harness-side**, token from `secretbind`,
  **behavioral verify before the irreversible deploy**, gated behind the human approver, never
  model-emitted.
- **Depends on:** EXT-02-T01 (and reuses `verify`, `integrate`, `policy`). **Owns:** `internal/deploy/`
  (`deploy.go`, `deploy_test.go`, `deps_test.go`).
- **Acceptance:** `GatedDeploy(ctx, ask policy.Approver, plan Plan, bind secretbind.Env) (url string, deployed bool, err error)`
  runs in strict order — (1) `plan.Verifier.Check` (project checks, I2) must be green; (2)
  `plan.Browser.Check` (behavioral, `verify.BrowserVerifier`) must be green — a red at (1) or (2)
  returns `deployed=false` with **no** API call; (3) constructs `policy.GateAction{Type: policy.Deploy,
  Branch: plan.Branch, Detail: plan.Detail}` and calls `policy.GateStructured` (nil approver ⇒ deny,
  `gateaction.go:98-100`); (4) on approval only, a single stdlib-HTTP deploy call with the token as a
  per-request header; (5) appends a **redacted** `deploy` event (I5). The deploy step **never lands
  to a base branch** — it ships the verifier-green tree the Integrator produced; base promotion is a
  separate `PromoteToBase` (never auto-invoked here).
- **Verify:** `make verify`; `go test ./internal/deploy/...` with `httptest` + fake approver/verifier:
  a red project check ⇒ no deploy; a red browser check ⇒ no deploy (fail-closed); nil approver ⇒ no
  deploy; full-green + approve ⇒ exactly one deploy request; token absent from logs/errors; a test
  asserts the deploy event Detail carries **no** secret and **no** model `Value`.
- **Notes:** *deploy is irreversible* — the verify-then-gate ordering is the I2+gate guarantee made
  executable. No new `GateActionType`; no module.

#### EXT-02-T05 — Behavioral deploy-gate verifier glue
- **Goal:** assemble the `verify.BrowserVerifier` that drives the staged preview/deploy candidate
  headless in-box and reports pass/fail from exit code (the "the deployed thing works" gate).
- **Depends on:** — (reuses `verify.BrowserVerifier`, `sandbox`). **Owns:** `internal/deploy/verify/`
  (`gate.go`, `gate_test.go`).
- **Acceptance:** `BuildBrowserGate(box sandbox.Sandbox, target string) *verify.BrowserVerifier`
  wraps `verify.NewBrowser(box, cmd)` (`browser.go:29-31`) with the driver invocation against the
  staged URL; fail-closed when the browser binary is absent (`browser.go:20-22`) — red, never a
  false green; the verdict is **evidence the verifier consumes**, never a model self-report
  (`browser.go:11-15`).
- **Verify:** `make verify`; `go test ./internal/deploy/verify/...` with a stub box: exit 0 ⇒ Pass;
  exit non-zero ⇒ Fail; nil box / empty command ⇒ Fail (fail-closed, `browser.go:34-37`).
- **Notes:** thin glue — the behavioral check itself is shipped. CI-only live behavior; hermetic
  tests cover wiring.

#### EXT-02-T06 — Web-surface reverse-proxy + progress stream leaf · NET-NEW web host
- **Goal:** the net-new HTTP surface that reverse-proxies the in-box preview to the operator's
  browser and streams generation progress — **separate from `internal/server`** (which stays a chat
  intake) and **holding no provider credential** (I3).
- **Depends on:** EXT-02-T02. **Owns:** `internal/webpreview/` (`webpreview.go`, `proxy.go`,
  `stream.go`, `*_test.go`, `deps_test.go`).
- **Acceptance:** `New(handle preview.Handle, log *eventlog.Log) *Surface` over stdlib
  `net/http`+`net/http/httputil.ReverseProxy` to `handle.URL`; an SSE/WS progress stream
  (stdlib) for generation events; **binds loopback by default**; every inbound text is `guard.Wrap`'d
  as data before it could reach a session (I7); `deps_test.go` asserts `webpreview` imports **no**
  `secrets`/`secretbind`/`provision`/`deploy` (a token cannot structurally reach the frontend) and
  **no** orchestrator (`agent`/`super`/`project`).
- **Verify:** `make verify`; `go test ./internal/webpreview/...` with `httptest`: proxy forwards to a
  fake backend; the progress stream emits ordered events; an `?api_key=` in any proxied response is
  not amplified into a log; `deps_test` enforces the no-secret / no-orchestrator import allowlist.
- **Notes:** the reverse proxy uses stdlib only (I6). The default bind is loopback; any public bind
  is an explicit operator flag gated at T10.

#### EXT-02-T07 — Web-surface auth + session map
- **Goal:** the per-principal deny-default trust line for who may drive a preview session, reusing
  the shipped `Authorizer` shape, plus the per-session preview map.
- **Depends on:** EXT-02-T06. **Owns:** `internal/webpreview/` (whole pkg): `auth.go`, `session.go`,
  `*_test.go`.
- **Acceptance:** a `server.Authorizer`-shaped `Permit(principal) bool` gate (`server.go:60-63`),
  **nil ⇒ deny-all** (no ambient authority); one preview session per authorized principal; an
  unauthorized request is refused (logged, never promoted to a drive) — the *only* promotion to
  principal trust, mirroring `server.go:16-24`; SSO/SCIM/RBAC sourcing is **EXT-05** (out of scope,
  the static allowlist is the default).
- **Verify:** `make verify`; `go test ./internal/webpreview/...`: nil Auth ⇒ all requests denied;
  authorized principal drives; a second principal to the same session refused; the trust line is the
  only promotion path.
- **Notes:** same trust discipline as `internal/server`, on a different transport. Whole-package
  Owns ⇒ serial after T06.

#### EXT-02-T08 — App-generator role + profile
- **Goal:** the app-generation worker as "configuration over the one loop" — a roster role/profile
  over `backend.CodingBackend.Run` (I1), no contract change.
- **Depends on:** — **Owns:** `internal/roster/roster.go` (additive `RoleAppGen` const),
  `internal/roster/worker.go` (its Profile/System).
- **Acceptance:** `RoleAppGen` added to the open Role keyspace; write capability via
  `Profile.ReadOnly:false` (**not** the hardcoded `Role.ReadOnly()` helper — the documented gotcha:
  `Role.ReadOnly()` is `role != RoleImplementer`); System prompt is a full-stack-app generator;
  existing roster tests stay green (additive consts).
- **Verify:** `make verify`; `go test ./internal/roster/...`: the role resolves; `Profile.ReadOnly==false`
  while `RoleAppGen.ReadOnly()==true` (exercises the gotcha); additive consts break no existing test.
- **Notes:** `roster.go`/`worker.go` are leaves (not frozen). The generator drives the **same** loop
  — `backend.Task`/`Result`/`CodingBackend.Run` untouched (I1).

#### EXT-02-T09 — `onboard.Config.Preview` field + Validate · contract (config schema)
- **Goal:** additively extend `onboard.Config` with one optional `Preview *PreviewConfig`
  (`json:"preview,omitempty"`) + a Validate clause, v1-compatible under `DisallowUnknownFields`.
- **Depends on:** EXT-02-T01, EXT-02-T03, EXT-02-T04. **Owns:** `internal/onboard/onboard.go`,
  `onboard_test.go`.
- **Acceptance:** default-zero so every existing config parses unchanged; `PreviewConfig` names the
  provisioning/deploy provider **endpoints** and the **secret names** (never values) to bind;
  `Validate()` gains a clause (provider ∈ known, secret names non-empty when a provider is set, loud
  error otherwise); old configs without `preview` parse; a config with `preview` round-trips
  parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip with `Preview` set; old
  config parses; `Validate` rejects an unknown provider and an empty secret-name set.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface),
  a contract surface. `onboard → secretbind/provision/deploy` is downward (no cycle). The config
  carries **secret names, never secret values** (I3).

#### EXT-02-T10 — `nilcore preview` subcommand + buildPreview
- **Goal:** the operator front door for the prompt→live-preview loop — compose worktree, the
  app-generator role, the preview runner, and the web surface behind one subcommand.
- **Depends on:** EXT-02-T06, EXT-02-T07, EXT-02-T08, EXT-02-T02. **Owns:** `cmd/nilcore/preview.go`
  (new file).
- **Acceptance:** `registerPreviewFlags` parses `--goal`, `--bind` (default loopback), `--egress-allow`,
  `--provision` (names only), `--sandbox/--runtime/--image/--log/--config/--deadline`;
  `buildPreview(deps) (previewAssembly, error)` resolves: `loadBoot` → `openLog` → worktree →
  `roster.NewWorker(RoleAppGen)` → drive `backend.CodingBackend.Run` (I1), re-verify each turn
  (`verify.Verifier`, I2) → `preview.Run` in-box → `webpreview.New` + `Authorizer` (deny-default);
  a public `--bind` is allowed only with an explicit operator flag + a warning; one shared
  `eventlog`; the model never receives a token.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, scripted fake provider + fake
  sandbox): default-flag parse; default bind is loopback; the web surface holds no token; an
  unauthorized request is denied; generation events stream.
- **Notes:** new file; the dispatch arm is added by T12. Provisioning here is **gated** at request
  time (T03), never at startup.

#### EXT-02-T11 — `nilcore deploy` subcommand + buildDeploy
- **Goal:** the operator front door for one-click deploy — verify (project + behavioral) then gate
  then deploy, with the integrator folding green branches and never landing to base.
- **Depends on:** EXT-02-T03, EXT-02-T04, EXT-02-T05. **Owns:** `cmd/nilcore/deploy.go` (new file).
- **Acceptance:** `registerDeployFlags` parses `--target`, `--branch`, the secret names, the
  approver wiring; `buildDeploy(deps) (deployAssembly, error)` composes: `verify.Verifier` →
  `deploy/verify.BuildBrowserGate` → `integrate.Integrator` (fold green, **never land to base**) →
  `provision.GatedProvision` (if requested, gated) → `deploy.GatedDeploy` (verify-then-gate-then-ship);
  the human approver is wired (nil ⇒ deny); exit 0 iff the project checks **and** the behavioral
  check **and** the chain verify **and** the operator approves; the deploy candidate is the
  verifier-green Integrator tip — base promotion is a **separate** gated `PromoteToBase`, never
  auto-invoked here.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic): a red check ⇒ no deploy
  (exit 1); nil approver ⇒ no deploy; full-green + approve ⇒ one deploy call; the integrator never
  pushes to base in any path; the deploy event is redacted.
- **Notes:** new file; dispatch arm added by T12. The verify-then-gate ordering is non-negotiable
  (I2 + irreversible-action gate).

#### EXT-02-T12 — Dispatch wiring (`case "preview"` / `case "deploy"`) · serialized cmd-wiring
- **Goal:** the **only** edit to `main.go` — two new dispatch cases + two usage lines.
- **Depends on:** EXT-02-T10, EXT-02-T11, EXT-02-T09. **Owns:** `cmd/nilcore/main.go` (two cases +
  usage lines — the **only** edit to an existing cmd file).
- **Acceptance:** `case "preview": return previewMain(...)`; `case "deploy": return deployMain(...)`;
  two usage lines; **no** other arm or shared helper edited; the default dispatch path (`nilcore` /
  `nilcore run`) reaches neither new arm.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...`: the new arms dispatch; the default path is
  unchanged; usage lists the new subcommands.
- **Notes:** **serialized** — the only task editing `main.go` (the §5 contract-file discipline).

#### EXT-02-T13 — Default-off byte-identity + import-graph guard tests · solo
- **Goal:** prove §9 — the default binary is byte-identical with the feature absent.
- **Depends on:** EXT-02-T12. **Owns:** `cmd/nilcore/extoff_test.go`.
- **Acceptance:** an **import-graph test** asserts **no** existing package imports
  `internal/webpreview`/`internal/preview`/`internal/provision`/`internal/deploy`/`internal/secretbind`;
  an **init()-free test** asserts the new leaves have no global-side-effect `init()`; a dispatch test
  asserts the default path reaches neither new arm; a deps test re-asserts `secretbind` is never
  imported by `webpreview` (the I3 structural guarantee).
- **Verify:** `make verify`; `go test ./cmd/nilcore/...`: all guard tests green.
- **Notes:** solo — pure test additions, no behavior. Mirrors the SWARM.md default-off guard pattern.

#### EXT-02-T14 — Docs + CHANGELOG promotion · contract (docs), serialized last
- **Goal:** promote this plan into the canonical docs and ledger.
- **Depends on:** EXT-02-T13. **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`,
  `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md`.
- **Acceptance:** `docs/TASKS.md` an EXT-02 DAG + specs (marked gated, post-thesis-decision);
  `docs/ARCHITECTURE.md` a "Full-stack preview/deploy (EXT-02, gated)" subsection (web surface
  separate from `internal/server`; provision/deploy as gated `Deploy` GateActions; the
  `secretbind` confinement boundary; behavioral-verify-before-deploy; never-land preserved) + the
  new leaf rows in the layer-map with import sets; `docs/ROADMAP-EXTERNAL-INFRA.md §3` updated to
  reference the executed plan + the EXT-05 (multi-tenant dashboard) / EXT-01 (managed fleet) /
  EXT-06 (central secrets) lines this does **not** cross; `CLAUDE.md` repository-map lines (no
  invariant text change — the point is the invariants are unchanged); `CHANGELOG.md` one
  `## [Unreleased]` line per merged EXT-02-T0x; `README.md` the `nilcore preview`/`deploy` usage +
  the gated/default-off note + the honest caveats.
- **Verify:** `make verify` (docs don't break the build); a markdown-format pass; manual review that
  the layer-map import sets match `go list -deps` of each new leaf.
- **Notes:** **serialized — contract files.** Lands last. The §0 thesis-decision record is part of
  the same promotion PR (it is the gate, `docs/ROADMAP-EXTERNAL-INFRA.md:13-15`).

---

## §6 Parallel-execution wave map

A fleet executes in ordered waves; every task in a wave has all deps merged to `main` and a
pairwise-disjoint Owns set. `internal/webpreview` is one package = one Owns unit, so T06→T07 is a
serialized sub-chain.

```
WAVE 1  (5 concurrent — no-dep new leaves + role)
  ├── EXT-02-T01  internal/secretbind/   (the I3 spine; everything gated imports it)
  ├── EXT-02-T02  internal/preview/
  ├── EXT-02-T05  internal/deploy/verify/
  ├── EXT-02-T08  internal/roster (additive RoleAppGen)
  └── (T06 waits on T02)

WAVE 2  (3 concurrent — composers over wave-1 leaves)
  ├── EXT-02-T03  internal/provision/     (T01)
  ├── EXT-02-T04  internal/deploy/        (T01)
  └── EXT-02-T06  internal/webpreview/    (T02)   ← opens internal/webpreview

WAVE 3  (2 concurrent)
  ├── EXT-02-T07  internal/webpreview/ (auth+session)  (T06)   ← serial in-package after T06
  └── EXT-02-T09  internal/onboard/onboard.go          (T01,T03,T04)  ← SERIAL pt: config-schema contract

WAVE 4  (2 concurrent)
  ├── EXT-02-T10  cmd/nilcore/preview.go   (T06,T07,T08,T02)
  └── EXT-02-T11  cmd/nilcore/deploy.go    (T03,T04,T05)

WAVE 5  (1 — SERIAL pt: cmd-wiring)
        EXT-02-T12  cmd/nilcore/main.go    (T10,T11,T09)   ← sole main.go editor

WAVE 6  (1)
        EXT-02-T13  cmd/nilcore/extoff_test.go   (T12)     ← proves default-off byte-identity

WAVE 7  (1 — SERIAL pt: docs contract)
        EXT-02-T14  docs/* + CLAUDE.md + README.md + CHANGELOG.md  (T13)  ← sole docs editor
```

**Peak concurrency = 5 (wave 1).** **Critical path (longest dependency chain) — 7 sequential merges:**

```
EXT-02-T02 → EXT-02-T06 → EXT-02-T07 → EXT-02-T10 → EXT-02-T12 → EXT-02-T13 → EXT-02-T14
```

(The T01 → T03/T04 → T11 → T12 chain is shorter; the web-surface chain dominates.)

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/webpreview` package dir — T06 opens; T07 serializes as sibling files.
2. `internal/onboard/onboard.go` — T09 only (config schema, a stable interface).
3. `cmd/nilcore/main.go` — T12 only (one `case "preview"` + one `case "deploy"` + usage).
4. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — T14 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; no task depends on a later
task; `internal/webpreview`'s sub-chain is strictly increasing IDs. **Foundation-before-orchestration
holds:** `provision`/`deploy` literally cannot compile without `secretbind` (T01); the preview
front door cannot compile without the web surface (T06/T07).

---

## §7 Per-invariant ledger

The seven invariants — plus the never-land guarantee — hold **by reuse**, not by new mechanism.
**Deploy is irreversible → gated.**

| Invariant | How EXT-02 preserves it |
|---|---|
| **I1** frozen contract | The app-generator drives the **unchanged** `backend.CodingBackend.Run(ctx, Task) (Result, error)`. `Task`/`Result`/the interface are **untouched** (`internal/backend/backend.go`). The generator is a `roster` role ("configuration over the one loop"); preview/provision/deploy are *post-generation* steps over the produced worktree, never a contract field. |
| **I2** verifier sole authority | A deploy candidate must pass (a) the project's own `verify.Verifier.Check` **and** (b) the behavioral `verify.BrowserVerifier` (`browser.go:11-15` — *"evidence the verifier consumes, never a model self-report"*) **before** the irreversible deploy. No `Result.SelfClaimed` decides shipping. The Integrator re-verifies after every merge (`integrate.go:1-22`). Fail at any check ⇒ no deploy (fail-closed). |
| **I3** no ambient authority | **The load-bearing invariant.** Every provider token lives in `secrets.SecretStore`, is resolved **only** through `internal/secretbind` into an opaque `Env`, and is injected **only** into a server-side sandboxed runtime via `box.ExecWithEnv` **by name** (`sandbox.go:159-160`) or used once as a per-request header (`forge.go:48-49`). It is **never** in the frontend (`webpreview` cannot import `secrets`/`secretbind` — `deps_test`), **never** in a prompt, **never** given to the model, **never** logged (events redacted, I5). A provisioned connection string is itself a secret, stored back by name. |
| **I4** sandboxed execution | The preview app and all build/provision-prep steps run **inside `sandbox.Container`** (hardened: cap-drop, read-only rootfs, no-new-privileges, worktree the single writable mount, `sandbox.go:107-150`). Egress is default-deny (`Network:"none"`) unless opted into the SSRF-defended allowlist proxy (`egress_proxy.go:33-43`). A delegated coding backend, if used, runs in-box. No model-emitted command runs on the host. |
| **I5** append-only audit | Every generation turn, verify, provision-gate, deploy-gate, and deploy is an append-only, hash-chained, **redacted** event (`eventlog`). A broken chain shows RED; history is never mutated. Deploy event Detail carries no secret and no model `Value`. |
| **I6** zero-dep core | All new leaves are stdlib-only; provider clients speak `net/http`+`encoding/json` (the `internal/forge` precedent, `forge.go:11-12`); the web surface uses `net/http/httputil`. **No `go.mod` change; `CGO_ENABLED=0` preserved.** (§8) |
| **I7** untrusted-as-data | Generated code, fetched content, and any inbound web text are `guard.Wrap`'d before they could enter a prompt; a deploy candidate's behavior is judged by the verifier, never by the model's claim; proxied responses are forwarded as data, never promoted to instructions. |
| **Never-land** | The Integrator returns a verifier-green throwaway tip and **NEVER pushes/lands to base** (`integrate.go:12-17`). **Deploy** ships that green tree to a hosting target — it does **not** land to a base branch. The only base land remains exactly **one** gated `policy.GateAction{PromoteToBase}` through the human approver (`gateaction.go:94-101`); **deploy is a separate, also-gated `policy.GateAction{Deploy}`** (already enumerated, `gateaction.go:34`). Every gate: **nil approver ⇒ deny** — no ambient authority for an irreversible step. EXT-02 constructs **no new `GateActionType`.** |

**The single line under all of it:** I3 — *no ambient authority.* EXT-02 adds the most authority of
any item (a hosting + provisioning backend), so it is allowed **only** because that authority is
**scoped (per-provider tokens), gated (every provision/deploy through the human approver, nil ⇒
deny), credential-stored (`SecretStore` only), confined (server-side sandbox only via `secretbind`),
and never handed to the model** — and **only after the §0 thesis gate clears.**

---

## §8 Module justifications (CGO-free)

**No new module.** Every external surface is spoken over the **standard library**, exactly as the
shipped `internal/forge` speaks the GitHub REST API (`forge.go:11-12`: *"Stdlib only (invariant I6):
the GitHub REST API is spoken over net/http with encoding/json, no client module"*):

| Capability | Implementation | Module? |
|---|---|---|
| Provisioning provider API (DB/auth/storage) | `net/http` + `encoding/json`, token as a per-request header | **none** (forge precedent) |
| One-click deploy API | `net/http` + `encoding/json` | **none** |
| Web surface (preview proxy + progress stream) | `net/http` + `net/http/httputil.ReverseProxy` + SSE/WS over stdlib | **none** |
| Preview run + provisioning build steps | the existing `sandbox.Container` (already in tree) | **none** |
| Secret confinement | `internal/secrets` (shipped) + a new stdlib `secretbind` leaf | **none** |

`CGO_ENABLED=0` is preserved across the release matrix (`.github/workflows/release.yml`); the only
sanctioned deps (pure-Go SQLite, `golang.org/x/sys`, build-tagged Charm) are untouched and unused by
EXT-02. **If** a future provider truly required a non-stdlib client (e.g. a gRPC-only control plane),
that is a **separate, justified `go.mod` contract task** with its own PR + CHANGELOG rationale and a
proof it keeps `CGO_ENABLED=0` (I6) — it is **not** folded into any task above.

---

## §9 Default-off byte-identical proof

The default `nilcore` binary is **byte-identical** with EXT-02 absent — **proven, not asserted**
(EXT-02-T13):

1. **No existing package imports a new leaf.** An import-graph test asserts nothing in the current
   tree imports `internal/webpreview`/`internal/preview`/`internal/provision`/`internal/deploy`/
   `internal/secretbind`. They are reachable **only** through the two new cmd files.
2. **No global side effects.** The new leaves contain **no `init()` with global side effects**, so
   merely linking them cannot change behavior. (`roster`'s additive `RoleAppGen` const is an inert
   value; `onboard`'s `Preview` field is default-zero and parses every old config unchanged under
   `DisallowUnknownFields`.)
3. **The default dispatch path reaches neither new arm.** `nilcore` / `nilcore run` never enters
   `case "preview"` or `case "deploy"` — established exactly as the existing `schedule`/`watch`
   cases are.
4. **The I3 structural guarantee is itself a test.** `webpreview` cannot import `secrets`/`secretbind`
   — so even with the feature *present*, a token cannot structurally reach the frontend.

Removing EXT-02 is deleting the new leaves + the two cmd files + the two dispatch lines — fully
reversible, no residue (gate criterion §0.5).

---

## §10 Risks

1. **Identity drift (the thesis risk).** EXT-02 turns a coding *harness* into an app-building
   *platform* with **standing hosting obligations** (uptime, SSL, tenancy) — the anti-principle's
   prime example (`docs/ROADMAP-EXTERNAL-INFRA.md:75`) and *"likely the lowest-priority EXT item for
   a tiny-harness product"* (`:77`). **Mitigation:** the §0 gate with a **named owner for uptime and
   security**; everything default-off and reversible; the web surface kept strictly separate from
   `internal/server`.
2. **I3 leak surface (the credential risk).** "Wire up Stripe/Supabase by prompt" is the direct
   inverse of "secrets never reach the model" (`:73`). A careless path could leak a provider token to
   the frontend, a prompt, or a log. **Mitigation:** the single `secretbind` confinement boundary
   with a structural (compile-time + deps-test) guarantee that no token field is serializable and
   `webpreview` cannot import it; per-request-header-then-discard for harness HTTP; redacted events.
3. **SSRF / preview escape.** A generated app fetching `169.254.169.254` or localhost from inside the
   preview. **Mitigation:** egress is default-deny; when opted in, the SSRF-defended allowlist proxy
   blocks loopback/metadata/private ranges and pins to a validated IP (`egress_proxy.go:33-43`).
4. **Irreversible deploy of a broken app.** Shipping a deploy that does not actually work.
   **Mitigation:** verify-then-gate ordering — project checks **and** behavioral `BrowserVerifier`
   (fail-closed) **before** the gate; nil approver denies; the deploy never lands to base.
5. **Provisioning cost / orphaned resources.** Gated provisioning could leave standing paid
   infrastructure. **Mitigation:** every provision is gated (human approval, nil ⇒ deny); the owner
   named in §0 owns lifecycle/teardown; v1 may scope to ephemeral/teardown-on-session-end resources.
6. **Scope creep into EXT-05/EXT-01/EXT-06.** A served multi-tenant dashboard (EXT-05), a managed
   cross-host fleet running these sessions (EXT-01), or central secret distribution (EXT-06) are
   **separate gated items**. **Mitigation:** `deps_test` guards keep `webpreview`/`provision`/`deploy`
   free of orchestrator, remote-DB, and multi-tenant couplings; the SW-style ARCHITECTURE note pins
   the boundary so a follow-on PR cannot quietly cross it.

---

*EXT-02 is the gap NilCore is most deliberately behind on. The reason it is behind is, mostly, on
purpose: it is the clearest case where the thesis itself is on the table. This plan exists so that
**if** a human owner decides the thesis may expand, the capability can be built the way the rest of
the system earns trust — scoped authority, the human gate, the `SecretStore` confined to a
server-side sandbox, a verifier (and a browser) that still have the only vote on "done," and an
integrator that never lands to base. Until that §0 decision is recorded, this file is a boundary to
defend, not a backlog to burn down. <3*
