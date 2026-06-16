# Roadmap — External-Infrastructure Upgrades (Gated)

**The upgrades that would make NilCore more capable but require standing external infrastructure, broad ambient authority, or a change to its "one static binary, runs anywhere" identity — quarantined here behind an explicit decision gate.**

This is the deliberate counterpart to `docs/UPGRADE-PATH.md`. Tiers 1–2 there are self-hostable, stdlib-first, nil/flag-gated, and reinforce NilCore's thesis. Everything in *this* file is different in kind: each item grants the process **standing authority** (a cloud control plane, a public hosting backend, a corporate identity provider, a remote credential store) that the design refuses *by default*. None of these should be built as a casual feature. Each must clear **the gate in §0 first** — a product-thesis decision, recorded, with a human owner.

> **Why a separate file at all.** `docs/PRINCIPLES.md:49-57` names the governing anti-principle: *"bolting on features that dilute the core."* And `docs/ARCHITECTURE.md:81` fixes the shipping identity: *"One Go binary, cross-compiled for `darwin` and `linux` … Distribution: a Homebrew tap (macOS) and a curl-pipe-sh installer + systemd unit (Linux VPS)."* Every item below contradicts some part of that on purpose. Keeping them here — visible, specified, but *gated* — is how NilCore stays honest: the gaps are acknowledged and a path exists, without the core silently drifting into a managed SaaS.

---

## 0. The gate — what must be true before ANY EXT task is written

An `EXT-NN` item is **not** an eligible task in the sense of `CLAUDE.md` §5 work-selection. Before a single line is written, **all** of the following must hold and be recorded (in the PR that promotes the item into `docs/TASKS.md`, itself a serialized contract change):

1. **A recorded thesis decision.** A human owner has explicitly decided that NilCore's identity may expand from "tiny self-hosted harness" toward this capability. This decision is the gate — it is *not* delegable to the agent (it is exactly the class of irreversible, outward-facing action the whole design reserves for a human).
2. **Invariants survive, not bypassed.** The item must show, concretely, that it **extends** rather than weakens `I1`–`I7` — especially `I3` (no ambient authority): any new standing credential/authority is scoped, gated, and held in `secrets.SecretStore`, never given to the model (`docs/ARCHITECTURE.md:120-121`).
3. **The verifier still governs (`I2`).** No external surface may ship work on a self-report. Whatever runs remotely, the project's own checks (`verify.Verifier.Check`) still decide "done," and the integrator's never-land guarantee (`internal/integrate/integrate.go:12-17`) is preserved — the only base-branch land remains a gated `policy.GateAction{PromoteToBase}` through the human approver (`internal/policy/gateaction.go:86-94`).
4. **Dependency budget justified (`I6`).** Every new module (cloud SDK, vector-DB client, SSO library) is justified in **both** the PR and the CHANGELOG, and must not break `CGO_ENABLED=0` (`.github/workflows/release.yml`). Default to stdlib + the existing seams (the codebase hand-rolls PBKDF2 to stay stdlib, `internal/secrets/file.go:230-232`).
5. **Default-off, opt-in, reversible to remove.** The default `nilcore` binary remains byte-identical with the feature absent; nothing here becomes a hard requirement.

If any of these cannot be met, the item stays on this roadmap, unbuilt.

---

## 1. The boundary — what "external infra" means here, sourced

NilCore today is single-host, single-process, self-hosted. The roadmap items each cross one of these documented lines:

| Today (sourced) | The line an EXT item crosses |
|---|---|
| Parallelism is **in-process on one host**: `scheduler` worker pool (`internal/scheduler/scheduler.go:71-84`), supervisor fan-out is **synchronous serial** dispatch (`internal/super/dispatch.go:121-130`), the bus is in-process Go channels (`internal/agent/bus/bus.go:36-38`), serve drives are goroutines in one process (`internal/server/server.go:89-99`) | Multi-host / managed dispatch (`EXT-01`) |
| `internal/server` is a **chat-channel/bot intake** (Telegram/Slack), not an HTTP/web/preview host (`internal/server/server.go:1-14`) | A web/HTTP app + preview + hosting backend (`EXT-02`) |
| **No editor surface** by design; the LSP is consumed *inbound* as a client (`internal/codeintel/lsp/lsp.go`), never exposed to an editor | An editor extension / LSP-server surface + a custom model (`EXT-03`) |
| Semantic index is **local, pure-Go, single-worktree** (`internal/codeintel/semantic/semantic.go`) | A remote/managed vector index at org scale (`EXT-04`) |
| Auth is a **flat opaque-ID allowlist** (`internal/channel/authorized.go:15-19`); cost is **per-task + one global wall** (`internal/budget/budget.go:33-39`) | SSO/SCIM/RBAC + per-user/tenant dashboards (`EXT-05`) |
| Secrets are **per-host** (env / keychain / AES-256-GCM file vault / external hook) (`internal/secrets/secrets.go:19-35`) | Centralized cross-fleet secret distribution (`EXT-06`) |
| Skills/MCP load from **local dirs / local subprocesses**; **no** remote fetch exists (`internal/skills/skills.go`, `internal/mcp/config.go:55-79`) | A remote registry / marketplace (`EXT-07`) |
| Sandbox is container **or** namespace+Landlock+seccomp; Firecracker microVM is named **"future"** (`docs/ARCHITECTURE.md:150`) | A microVM/KVM isolation tier (`EXT-08`) |

---

## 2. EXT-01 — Managed cloud agent fleet

**What it is.** A managed control plane that provisions a fresh isolated VM/microVM per task, clones the repo, runs the agent loop unattended (durable across a laptop closing), and returns a reviewable PR — at high concurrency, launchable/monitorable from web/mobile/Slack. The capability gap behind Claude Code on the web, Codex cloud, Devin, Jules, and Cursor cloud agents.

**Why it requires external infra.** Everything about NilCore's parallelism is in-process on one host (see §1). A fleet is a *categorical* jump: a scheduler/control plane that leases tasks to remote workers, provisions and reclaims VMs, persists task state across hosts, and exposes a dashboard. Today durable resume re-drives a **local** single task on restart under a deny-default approver (`cmd/nilcore/main.go:719-751`), and multi-agent supervise/project resume via `RunState` is explicitly an unwired "documented follow-on" (`cmd/nilcore/main.go:723-725`; `internal/agent/durability.go:148-306`).

**Invariants / thesis it stresses.**
- **`I3` (load-bearing):** a fleet grants the process standing authority to spawn machines, clone repos, and hold credentials across hosts — the antithesis of "no broad filesystem/network authority by default." Every fleet credential must live in `secrets.SecretStore` and never reach the model; a prompt-injected remote agent must not be able to exfiltrate the real token (the leaders solve this with a scoped credential proxy — NilCore would need the same).
- **Thesis:** directly contradicts "one self-hosted Go binary" (`docs/ARCHITECTURE.md:81`). This is the clearest identity change on the roadmap.
- **Must preserve `I2`:** each remote worker's output still re-runs the project verifier locally before anything lands; the integrator still never pushes to base (`internal/integrate/integrate.go:12-17`).

**The gate.** A thesis decision to offer (or self-host) a managed fleet, plus a security review of the credential-proxy design. High bar.

**How it would be built, honoring the gates.**
- Reuse the seams, don't rebuild the loop: the unit of remote work is still a `backend.CodingBackend.Run` (`I1`), wrapped — the role/worker abstraction is already "configuration over the one loop" (`internal/roster/roster.go:1-7`). `model.Resilient` already accepts an **ordered** `[]Provider` for failover but is passed a single element today (`cmd/nilcore/main.go:1019`) — a fleet would light up multi-provider routing here.
- Replace the in-process `scheduler` with a leasing control plane that submits the same `scheduler.Task{ID, Run}` unit (`internal/scheduler/scheduler.go:23-26`) to remote workers; preserve the error-isolation + ctx-cancellation contract.
- Wire the unbuilt multi-agent `RunState`/`ResumePlan` machinery (`internal/agent/durability.go:148-306`) for cross-host handoff/leasing, keeping `Checkpoint`'s store-backed model.
- A control-plane credential proxy translates a scoped in-sandbox token to the real one, restricting pushes to the working branch — so `I3` holds even with a remote, possibly-injected agent.
- Deploy/land stays a gated `policy.GateAction` through the human approver; nil approver default-denies (`internal/policy/gateaction.go:86-94`).

**What must not change.** `I1` (the remote worker is still a `Run(ctx,Task)`), `I2` (local verifier governs), `I3` (no credential to the model; gate on every irreversible land), `I5` (every remote step still appends to the audit log).

---

## 3. EXT-02 — Full-stack preview / hosting / one-click deploy

**What it is.** The "prompt → live, shareable URL" loop: generate an app, render a live preview as code is written, provision managed Postgres/auth/storage by prompt, and one-click deploy to a hosted URL. The capability behind Replit, Lovable, Bolt, v0.

**Why it requires external infra.** `internal/server` is a chat-channel intake, **not** a web/HTTP app or preview host (`internal/server/server.go:1-14`); the only inbound `http.Server` in the codebase is the egress proxy (`internal/policy/egress_proxy.go`). "Deploy" exists only as an enumerated, **gated**, irreversible action (`policy.GateAction{Deploy}`, `internal/policy/gateaction.go:27-35`) with no code path that performs it. Provisioning a managed DB and deploying to a hosted URL are standing external services with their own credentials and lifecycles.

**Invariants / thesis it stresses.**
- **`I3` (sharply):** "wire up Stripe/Supabase by prompt" is the *direct inverse* of "secrets are never given to the model." Any provisioning must keep provider credentials in `secrets.SecretStore`, inject them only into a sandboxed runtime (never the model context), and confine them to a server-side boundary (the leaders confine them to edge functions, never the frontend).
- **`I2`:** a deployed app must still be gated and verified — deploy is irreversible (`policy.Classify` flags `deploy`, `internal/policy/policy.go:25-44`) and routes through the human gate; pairs naturally with Tier-1 U1-T03 behavioral verification before any deploy.
- **Thesis:** turns NilCore from a coding *harness* into an app-building *platform* with hosting obligations (uptime, SSL, tenancy). Large surface, large diff — the anti-principle's prime example.

**The gate.** A decision to operate (or integrate) a hosting/provisioning backend, with an owner for its uptime/security. Very high bar; likely the lowest-priority EXT item for a tiny-harness product.

**How it would be built, honoring the gates.**
- Preview/run uses the existing sandbox (`sandbox.Container` with egress, `internal/sandbox/sandbox.go`) to run the app; a **new** web surface (separate from `internal/server`) would proxy the preview — net-new, opt-in.
- Provisioning + deploy are **gated `GateAction`s** (`Deploy` already enumerated); the actual API calls are harness-side, token-from-SecretStore, behind the human approver — never a model-emitted command.
- Behavioral verification (Tier-1 U1-T03) gates "the deployed thing actually works" before the irreversible deploy lands.

**What must not change.** `I3` (no provisioning secret ever reaches the model), `I2` (verify + gate before deploy), the integrator's never-land guarantee.

---

## 4. EXT-03 — In-editor surface + custom inline-edit / next-edit models

**What it is.** A full in-editor experience — Tab autocomplete, next-edit prediction, inline-diff apply — backed by purpose-trained low-latency models. The capability behind Cursor (Fusion), Copilot (Next Edit Suggestions), Windsurf.

**Why it's gated.** NilCore has **no editor surface** by design (CLI / headless / chat / phone), and its LSP is consumed *inbound* (a client spawning `gopls`, `internal/codeintel/lsp/lsp.go`), never exposed *to* an editor. A custom next-edit model is **bespoke intelligence** — against "the harness wins; borrow the intelligence" (`docs/PRINCIPLES.md`, principle 2 / `I1`,`I6`). Both are new surfaces requiring their own infrastructure (a hosted low-latency model; an editor extension distribution channel).

**Invariants / thesis it stresses.**
- **Thesis (principle 2):** "borrow the intelligence" — NilCore deliberately ships **no** custom model. A trained Fusion/NES-class model is the sharpest break from that bet. Training/hosting it is external infra.
- **`I1`:** an editor surface still drives the loop through `backend.CodingBackend.Run`; inline-completion is a *different* interaction shape that would need its own seam without widening the contract.

**The gate.** A decision to become an editor product and/or to train/host a model — the biggest identity change of all. Lowest priority for a tiny-harness thesis.

**How it would be built, honoring the gates (cheapest-first).**
- **Custom/self-hosted model without bespoke training:** the OpenAI-compatible base-URL swap already exists — `provider.NewOpenRouter`/`NewOpenAI` point at arbitrary `provider/model` endpoints (`internal/provider/openai.go:30-51`). A self-hosted OpenAI-compatible inference endpoint plugs into `model.Provider` (`internal/model/model.go:52-59`) with **zero** contract change — this is the cheap path to "a different model" before any bespoke training.
- **Multimodal already paved:** Tier-1 U1-T01's image blocks let an editor/IDE pass screenshots/diagrams through the same `model.Provider` seam.
- **Editor surface:** an LSP-server mode or extension is net-new and lives outside the core; it would call the loop, not embed in it (preserving the dependency direction, `docs/ARCHITECTURE.md:457-484`).

**What must not change.** `I1` (`Provider.Complete`/`CodingBackend` unchanged; new capabilities are optional interfaces à la `model.Streamer`, `internal/model/stream.go:20-64`), `I6` (a custom model is a config/endpoint, not a new module).

---

## 5. EXT-04 — Remote / managed vector index at org scale

**What it is.** A managed, remote embeddings index for cross-repo / org-wide retrieval (Cursor's Turbopuffer + Merkle sync; Sourcegraph's Zoekt+SCIP across 100k+ repos).

**Why it's gated.** NilCore's semantic index is **local, pure-Go, single-worktree, in-memory/SQLite** by design (`internal/codeintel/semantic/semantic.go`; `cmd/nilcore/live.go:13-40`). A remote vector DB is a standing external service with its own credentials, hosting, and a new module dependency — and "remote" is the line the local Tier-2 `U2-T04` (pure-Go ANN, *local*, in-scope) deliberately does not cross.

**Invariants / thesis it stresses.**
- **`I6`:** a managed vector-DB client is a new module needing PR+CHANGELOG justification and must keep `CGO_ENABLED=0`.
- **`I3`:** index credentials in `SecretStore`; never to the model. A privacy-preserving design (store only embeddings + obfuscated path/line, re-read code locally — the Cursor model) is the bar.
- **Thesis:** standing remote infra vs the local-first index.

**The gate.** A decision to depend on a remote index service (self-hosted or managed) for retrieval at scale.

**How it would be built.** It rides Tier-2 `U2-T03`'s `Embedder` seam (`internal/codeintel/semantic/semantic.go:32-34`) and slots into the fixed `Retrieve` fusion order behind the existing `semantic` provenance lens (`internal/codeintel/retrieve/retrieve.go:21-136`) — the local index degrades to it. The remote client is opt-in; absent ⇒ local pure-Go ANN (`U2-T04`) ⇒ lexical fallback.

**What must not change.** `I7` (retrieved context still enters the loop as data; the `codeintel` tool is read-only by construction, `internal/tools/codeintel.go:24-37`), `I6` (CGO-free, justified dep).

---

## 6. EXT-05 — Enterprise control plane (SSO/SCIM/RBAC + dashboards + tenancy)

**What it is.** SAML/OIDC SSO, SCIM provisioning, RBAC roles, per-user/team usage & cost dashboards, an admin console, multi-tenancy. The capability behind Copilot Enterprise, Sourcegraph, AWS Q/Kiro, Devin Enterprise.

**Why it's gated.** Authorization today is a **flat opaque-ID allowlist** — `channel.Authorized` is a `map[string]struct{}`, empty-by-default deny-all, sourced from `NILCORE_ALLOWLIST` + `config.json` `channel.allow`, with **no** roles/groups/scopes/IdP (`internal/channel/authorized.go:15-19,35-38`; `cmd/nilcore/main.go:1254-1270`; serve fatals on an empty allowlist, `main.go:608-612`). Cost is **per-task + one global dollar wall** with no per-user/tenant attribution or dashboard (`internal/budget/budget.go:33-39`). An identity-provider integration + tenancy model + dashboards are a standing external control plane.

**Invariants / thesis it stresses.**
- **`I3`:** federating to an IdP introduces standing identity/authority — must be scoped and credential-stored, never to the model.
- **Thesis:** a single-operator binary becoming a multi-tenant SaaS. Pure surface growth.

**The gate.** A decision to serve teams/enterprises with managed identity. The capability is real but orthogonal to coding quality.

**How it would be built.** Federate SAML/OIDC + SCIM to the org IdP mapped to RBAC roles, layered **above** the existing `Authorizer` seam (`internal/server/server.go:60-62`, `Permit(principal) bool`) so the per-message trust line (`channel.Authorized.Permit`, the only promotion to principal trust, `internal/server/server.go:16-24`) is preserved, just sourced from a directory instead of a static list. Per-user/tenant cost extends `budget.Ledger`'s keying (`internal/budget/budget.go:33-39`) and the `meter.Provider.OnUsage` telemetry hook (`internal/meter/meter.go`) feeds a dashboard. All opt-in; the flat allowlist remains the default.

**What must not change.** `I7`/the trust line (one principal per thread, deny-default), `I3`, `I5` (`unauthorized_command`/`unauthorized_gate` audit events stay).

---

## 7. EXT-06 — Centralized secret distribution across a fleet

**What it is.** A central secret store/broker that distributes credentials to many hosts/workers (for `EXT-01`/`EXT-05`), beyond today's per-host backends.

**Why it's gated.** Secrets today are **per-host**: `EnvStore` (read-only, host-injected via systemd/shell, `internal/secrets/env.go:8-13`), `KeychainStore`, the AES-256-GCM `FileStore` for headless hosts (`internal/secrets/file.go`), and an `ExternalStore` hook for "corporate secret managers (Vault, cloud KMS wrappers)" (`internal/secrets/external.go:11-18`). There is no central broker. A fleet needs centralized, audited, least-privilege distribution.

**Invariants / thesis it stresses.** **`I3` is the entire point** — central distribution multiplies the blast radius of a leak. It must keep the cardinal rule: secrets injected per-run, never on disk in plaintext, never logged, never in a prompt, never given to the model (`docs/ARCHITECTURE.md:120-121`).

**The gate.** Tied to `EXT-01`/`EXT-05`; do not build standalone.

**How it would be built.** Extend the **already-existing** `ExternalStore` hook (`internal/secrets/external.go`) to front a Vault/cloud-KMS broker — the seam is designed for exactly this. Per-worker leases are short-lived and scoped; the SecretStore interface (`Get/Set/Delete/Name`, `internal/secrets/secrets.go:19-24`) is unchanged.

**What must not change.** `I3` in full; the model never sees a key, ever.

---

## 8. EXT-07 — Remote skills/MCP registry & marketplace

**What it is.** Remote fetch/publish/version of skills and MCP servers (Claude Code plugin marketplaces; Cline/Roo MCP marketplaces) — distribution at internet scale.

**Why it's gated.** Tier-2 `U2-T06` builds the *local* versioned registry; this item adds **remote fetch/publish**, which is the line `U2-T06` stops at. Nothing in `internal/skills`/`internal/mcp`/`internal/selfimprove` makes a network call today; MCP servers are local subprocesses from operator-configured local commands (`internal/mcp/config.go:55-79`), skills load from a local dir (`internal/skills/skills.go:75-100`).

**Invariants / thesis it stresses.**
- **`I7`:** a fetched skill/server is **untrusted** until operator-verified — it must not become a controlling instruction. Remote artifacts need signature/provenance verification before install.
- **`I3`/gate:** install routes through `selfimprove.Flow` (scope-check → verified task → human gate → merge, `internal/selfimprove/selfimprove.go:77-100`); "Merge is irreversible: always the human gate, no exceptions" (`selfimprove.go:93`).
- **Decided choice:** self-improvement stays "prompts/skills/tools only, never the core, gated" (`docs/ARCHITECTURE.md:24`); the `DefaultScope` allow/deny sets (`internal/selfimprove/selfimprove.go:33-42`) bound what may be written.

**The gate.** A decision to operate/trust a remote registry, with artifact-verification design reviewed.

**How it would be built.** A fetch step in front of `U2-T06`'s local registry: download → verify signature/provenance → present to `selfimprove.Flow` for verified, human-gated install into the existing discovery dirs. The per-tool `mcp.Gate` (`internal/mcp/client.go:26-28`) and untrusted-output fence stay. A registry client is a new dependency needing `I6` justification (prefer stdlib HTTP).

**What must not change.** `I7` (remote artifact = untrusted until verified), the verify+human-gate install path, the no-core-edit scope.

---

## 9. EXT-08 — Firecracker microVM sandbox tier

**What it is.** A microVM (Firecracker/KVM) isolation tier — the strongest end of the isolation spectrum NilCore already documents as **"future"**: *"Firecracker microVM (strongest; Linux/KVM; future) → container → namespace + Landlock (lightest, most portable)"* (`docs/ARCHITECTURE.md:150`).

**Why it's gated (lightly).** Of all EXT items this is the most *philosophy-aligned* — it strengthens `I4`, not the thesis-breaking ones. But it requires **KVM-capable infrastructure** (bare metal or nested-virt cloud), which a typical self-hosted VPS may lack — so it is external-infra-adjacent (a hardware/host capability gate), not a casual default.

**Invariants / thesis it stresses.** **`I4` (strengthens it).** It must satisfy the **unchanged** `sandbox.Sandbox` interface (`Exec`/`ExecWithEnv`/`Workdir`, `internal/sandbox/sandbox.go:22-35`) and be auto-detected/selected exactly as the namespace vs container backends are today (`sandbox.New`, `-sandbox`/`NILCORE_SANDBOX`), failing closed to container/namespace when KVM is absent.

**The gate.** Availability of KVM-capable hosts; a decision that the added isolation is worth the host requirement. Lowest-risk EXT item.

**How it would be built.** A third `sandbox.Sandbox` implementation behind the existing factory + auto-detect (mirroring how P7-T01 added the namespace backend additively behind `sandbox.New` without touching the interface). Egress still routes through `policy.EgressProxy`; per-run secret injection via `ExecWithEnv` (`I3`) is unchanged.

**What must not change.** The `sandbox.Sandbox` interface, `I3` (no host-env leak — the namespace backend's `do-not-seed-from-os.Environ()` discipline, `internal/sandbox/namespace_linux.go:79-94`, is the template), `I4`.

---

## 10. Cross-cutting invariant ledger

Every EXT item, against the invariants it most stresses and the one rule it can never break:

| Item | Most-stressed | Hard rule it must never break |
|---|---|---|
| EXT-01 Cloud fleet | `I3`, thesis | Verifier governs (`I2`); no credential to the model; gated land |
| EXT-02 Hosting/deploy | `I3`, thesis | No provisioning secret to the model; verify+gate before deploy |
| EXT-03 Editor + custom model | thesis (principle 2), `I1` | `Provider.Complete`/`CodingBackend` unchanged; model is config not a module |
| EXT-04 Remote vector index | `I6`, `I3` | CGO-free; index creds in SecretStore; retrieval is read-only data (`I7`) |
| EXT-05 Enterprise control plane | `I3`, thesis | Per-message `Permit` trust line preserved; deny-default |
| EXT-06 Central secrets | `I3` | Never on disk plaintext / logged / prompted / to the model |
| EXT-07 Remote registry | `I7`, `I3` | Verify + human-gate install; never edit the core; untrusted-until-verified |
| EXT-08 microVM | `I4` (strengthens) | `sandbox.Sandbox` interface unchanged; fail-closed; no host-env leak |

**The single line under all of them:** `I3` — *no ambient authority.* "The process holds no broad filesystem or network authority by default" (`docs/ARCHITECTURE.md:120-121`). Every item here adds authority; each is allowed only if that authority is **scoped, gated, credential-stored, and never handed to the model** — and only after the §0 gate clears.

---

*These are the gaps NilCore is genuinely behind on — and the reason it's behind is, mostly, on purpose. This file is not a backlog to burn down; it's a boundary to defend. Build from `docs/UPGRADE-PATH.md` first — it makes NilCore better at being NilCore. Reach for this file only when the thesis itself is on the table, and when it is, build it the way the rest of the system earns trust: scoped authority, the human gate, the SecretStore, and a verifier that still has the only vote on "done." <3*
