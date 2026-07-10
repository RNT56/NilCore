# External-infrastructure execution plans (GATED — ready when the §0 gate clears)

This file is the **execution-ready blueprint set** for the gated external-infrastructure tier
`EXT-01..08`. The *boundary and rationale* for each item — what it is, why it requires standing
external authority, and the **§0 thesis gate** it must clear — live in
[`docs/ROADMAP-EXTERNAL-INFRA.md`](ROADMAP-EXTERNAL-INFRA.md). This file holds the *how*: a full,
parallel-executable task DAG per item, mirroring the depth of [`docs/SWARM.md`](SWARM.md).

> **NONE of this is eligible work.** Every plan here is **BLOCKED behind the
> [`docs/ROADMAP-EXTERNAL-INFRA.md` §0 gate](ROADMAP-EXTERNAL-INFRA.md#0-the-gate--what-must-be-true-before-any-ext-task-is-written)** — a recorded human thesis decision that NilCore's identity may expand
> toward that capability, plus (per item) a security review. These docs exist so that *if and when*
> a human clears a gate, the work can be executed completely, robustly, and in parallel without
> re-deriving the design. Until then they are a **boundary to defend, not a backlog to burn down.**

Each plan was authored against the **real seams** (file:line-cited) and holds the same discipline as
every shipped phase: it **extends** invariants I1–I7, never bypasses them; the remote/new unit of
work stays `backend.Run` (I1) where applicable; the **verifier still governs** "done" (I2); the
integrator **never lands to base** (promotion is one gated `policy.GateAction{PromoteToBase}`, nil
approver default-denies); secrets stay in `secrets.SecretStore` and never reach the model (I3); and
the **default `nilcore` binary stays byte-identical** when the feature is absent.

---

## The eight plans

| Item | Title | Plan | Tasks | Critical path | Net-new packages (illustrative) |
|---|---|---|---|---|---|
| **EXT-01** | Managed cloud agent fleet | [ext-plans/EXT-01.md](ext-plans/EXT-01.md) | 14 | 8 merges | `internal/fleet{,/credproxy,/provisioner,/worker}` |
| **EXT-02** | Full-stack preview / hosting / deploy | [ext-plans/EXT-02.md](ext-plans/EXT-02.md) | 14 | 7 waves | `internal/{webpreview,preview,provision,deploy,secretbind}` |
| **EXT-03** | In-editor surface + custom model | [ext-plans/EXT-03.md](ext-plans/EXT-03.md) | 12 | 6 merges | `internal/{lsprpc,inlineedit}`, `cmd/nilcore-lsp`, `model.Completer` |
| **EXT-04** | Remote / managed vector index | [ext-plans/EXT-04.md](ext-plans/EXT-04.md) | 12 | — | `internal/codeintel/remoteindex` |
| **EXT-05** | Enterprise control plane (SSO/SCIM/RBAC) | [ext-plans/EXT-05.md](ext-plans/EXT-05.md) | 16 | 7 merges | `internal/{identity,scim,directory,rbac,tenancy,usage,console,enterprise}` |
| **EXT-06** | Centralized secret distribution | [ext-plans/EXT-06.md](ext-plans/EXT-06.md) | 11 | 7 merges | `internal/secrets/broker` |
| **EXT-07** | Remote skills/MCP registry & marketplace | [ext-plans/EXT-07.md](ext-plans/EXT-07.md) | 12 | — | `internal/regremote{,/trust,/verify,/bundle,/install}` |
| **EXT-08** | Firecracker microVM sandbox tier | [ext-plans/EXT-08.md](ext-plans/EXT-08.md) | 9 | 6 merges | `internal/sandbox` (3rd backend), `cmd/nilcore-guest` |

Each plan is a self-contained, disjoint-`Owns` DAG in its own `EXT-NN-T##` namespace — a fleet of
agents can execute one plan under the `CLAUDE.md` §5 work-selection rule with zero collision, and the
eight plans are **mutually independent at the file level** (they own different package trees), so even
*cross-item* parallelism is collision-free once a gate clears.

---

## Cross-cutting synthesis

The eight items are not eight islands. Several share substrate; building each shared piece **once**
avoids duplication and keeps the single load-bearing rule (I3) enforced in one place.

### Shared substrate (build once, reuse across items)

1. **The scoped-capability credential proxy (the I3 keystone).** `EXT-01`'s
   `internal/fleet/credproxy` mints a short-lived, HMAC-signed `ScopedToken{Repo, Branch, Exp, Sig}`
   and brokers token→real-credential **control-plane-side**, so a prompt-injected remote/multi-tenant
   worker can never exfiltrate the real credential, push outside its branch, or reach another tenant's
   worktree. **`EXT-06`** (central secret broker) and **`EXT-05`** (per-tenant credential scoping)
   must *compose with* this primitive, never re-mint their own — it is the single design that makes
   the whole tier safe under I3.
2. **The leasing control plane.** `EXT-01`'s `internal/fleet` (`ControlPlane`/`Lease`/`LeaseStore`,
   extending the (now single-host-wired) `agent.durability.RunState`/`ResumePlan` to cross-host handoff)
   underpins **`EXT-05`** multi-tenant scheduling. Build it in EXT-01; EXT-05 layers tenancy on top.
3. **Remote-fetch + signature/provenance verification.** `EXT-04` (remote-index client) and
   `EXT-07` (registry fetch/publish) share an identical pattern: a bounded, host-pinned stdlib
   `net/http` client + a `crypto/ed25519` (or HMAC) verify step, *untrusted-until-verified*. Factor a
   shared `internal/remotefetch` verify primitive so both inherit one audited trust path.
4. **A net-new web/HTTP surface.** `internal/server` is a chat-channel intake, **not** a web host.
   `EXT-02` (live preview host), `EXT-05` (admin console), and an optional `EXT-01` fleet dashboard
   each need an HTTP surface. Build a minimal stdlib `net/http` scaffold once (the egress proxy at
   `internal/policy/egress_proxy.go` is the only inbound `http.Server` precedent) and have all three
   reuse it behind the federated Authorizer below.
5. **Federated identity / the `Authorizer` seam.** `EXT-05` produces `internal/identity` +
   `directory.Authorizer` (implementing the existing `server.Authorizer.Permit` seam). The EXT-01/02
   web surfaces consume it for principal auth, preserving the per-message deny-default trust line —
   the static `channel.Authorized` allowlist stays the default everywhere.

### Cross-EXT dependency & sequencing

```
EXT-08  (microVM)            ── independent · hardware gate only · strengthens I4 · cheapest, most-aligned
EXT-03-low  (self-hosted endpoint) ── independent · near-free (provider base-URL swap, zero contract change)
EXT-04  (remote index)       ── independent of the fleet (rides the Embedder/semantic seam)
EXT-07  (remote registry)    ── independent of the fleet (fronts the local registry via selfimprove.Flow)

EXT-01  (fleet)  ──────────► EXT-05  (tenancy needs the control plane + federated Authorizer)
       │                └──► EXT-06  (central secrets serve the fleet; EXT-06 depends on EXT-01/05)
       └──────────────────► EXT-02  (hosting reuses the web-surface + credential-proxy substrate)

EXT-03-high (editor + bespoke model) ── the sharpest thesis break ("borrow the intelligence") · last
```

- **Fully independent / parallelizable** once their own gate clears: EXT-08, EXT-03-low-bar, EXT-04,
  EXT-07. None depend on another EXT item.
- **EXT-01 is the keystone** of the fleet cluster: it builds the credential proxy + leasing control
  plane that EXT-05 and EXT-06 both require. EXT-06 explicitly records the EXT-01/05 dependency.
- **EXT-02** (hosting/deploy) is the largest surface and reuses the most substrate (web surface,
  credential confinement, behavioral verification before the gated `Deploy`) — lowest priority.

### The one rule under all of them — I3, no ambient authority

Every item adds *some* standing authority. Each is admissible **only** if that authority is **scoped,
gated, `SecretStore`-held, and never handed to the model** — enforced by the *one* credential-proxy
design (shared-substrate #1). The model never sees a credential; a remote or multi-tenant agent holds
only a short-lived capability token bound to its branch/scope; the real credential lives on the
control-plane host and is brokered, never distributed. The integrator's never-land guarantee (I2 +
the gated `PromoteToBase`) holds regardless of what runs remotely.

### Recommended build order (if/when a human clears multiple gates)

Most thesis-aligned + cheapest first → most thesis-stressing last:

**EXT-08** (microVM — strengthens I4, hardware gate) → **EXT-03-low-bar** (self-hosted endpoint —
near-free, zero contract change) → **EXT-04** (remote index) → **EXT-07** (remote registry) →
**EXT-01** (fleet — the keystone) → **EXT-06** (central secrets) → **EXT-05** (enterprise control
plane) → **EXT-02** (hosting/deploy) → **EXT-03-high-bar** (editor surface + bespoke model).

EXT-08 is the only item that *strengthens* an invariant rather than stressing the thesis, and it is
fully additive behind the existing `sandbox.New` auto-detect — it is the natural first move. EXT-02
and EXT-03-high-bar are the clearest identity changes and the last to consider.

---

*These plans are complete enough to execute and disciplined enough to refuse — which is the point.
The gaps NilCore is "behind" on are mostly deliberate; this file makes each crossable on purpose, the
way the rest of the system earns trust: scoped authority, the human gate, the SecretStore, and a
verifier that still has the only vote on "done." Build from [`docs/HORIZON.md`](HORIZON.md) and the
canonical roadmap first — reach for these only when the thesis itself is on the table. <3*
