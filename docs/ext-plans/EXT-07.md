# EXT-07 — Remote skills/MCP registry & marketplace (GATED execution plan)

**Status: BLOCKED behind the §0 gate of `docs/ROADMAP-EXTERNAL-INFRA.md`.** This is a
*ready-when-the-gate-clears* plan. Not a single `EXT-07-T##` task may be promoted into
`docs/TASKS.md` (itself a serialized contract change) until a human owner records the thesis
decision in §0.1 and the artifact-verification design review in §0.2 has signed off. Until then
this document is a boundary to defend, not a backlog to burn down.

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/ROADMAP-EXTERNAL-INFRA.md` (§0 gate,
§8 EXT-07) → `docs/SWARM.md` (the depth template) → this file.

---

## Table of contents

- [§Summary](#summary)
- [§0 The gate — what must be true before any EXT-07 task is written](#0-the-gate)
- [§1 The line EXT-07 crosses (sourced)](#1-the-line-ext-07-crosses)
- [§2 As-is: what already ships (reuse, do not rebuild)](#2-as-is)
- [§3 Architecture (fetch+verify client in front of the local registry)](#3-architecture)
- [§4 The task DAG (EXT-07-T01 … EXT-07-T12)](#4-the-task-dag)
- [§5 Per-task specs](#5-per-task-specs)
- [§6 Parallel wave map & critical path](#6-parallel-wave-map--critical-path)
- [§7 Per-invariant ledger](#7-per-invariant-ledger)
- [§8 Module justifications (registry / signature client)](#8-module-justifications)
- [§9 Default-off, byte-identical proof](#9-default-off-byte-identical-proof)
- [§10 Risks (supply-chain, signature spoofing, malicious skill)](#10-risks)

---

## §Summary

EXT-07 adds **remote fetch / publish / version** of skills and MCP-server specs at internet scale —
the capability behind Claude Code plugin marketplaces and the Cline/Roo MCP marketplaces. Today
**nothing** in `internal/skills`, `internal/mcp`, `internal/registry`, or `internal/selfimprove`
makes a network call: MCP servers are local subprocesses launched from operator-configured commands
(`internal/mcp/config.go:55-79`), skills load from a local directory (`internal/skills/skills.go:80-104`),
and `internal/registry`'s `Entry.Source` is a **LOCAL path** with remote fetch explicitly deferred to
EXT-07 (`internal/registry/registry.go:9-11,43`). The local versioned registry is therefore *already
built* — EXT-07 only fronts it with a download-and-verify step.

The whole design is one sentence: **a fetched skill/MCP artifact is untrusted data until
operator-verified, and the only path from "downloaded" to "installed" runs through
`selfimprove.Flow` (scope-check → verified task → human gate → merge).** The remote client
*downloads bytes and verifies a detached signature + provenance against an operator-pinned trust
store*; it never installs, never executes, never lets a fetched byte become a controlling
instruction. The per-tool `mcp.Gate` (`internal/mcp/client.go:26-28,130-135`), the `guard.Wrap`
I7 fence (`internal/guard/guard.go:14-37`), and the `selfimprove.DefaultScope` no-core-edit
deny-list (`internal/selfimprove/selfimprove.go:33-42`) are all preserved unchanged. The registry
client is the **only** new authority (outbound HTTP GET + PUT to a pinned registry host); it is
built on **stdlib `net/http` + stdlib `crypto/ed25519`** — no new module, CGO-free.

The default `nilcore` binary is **byte-identical** with the feature absent: no network fetch occurs
when no `--registry` URL / `NILCORE_REGISTRY` is configured; `nilcore registry install <local.json>`
keeps its exact current behavior (local sources only). The remote path is a strictly additive
opt-in subcommand surface.

---

## §0 The gate

EXT-07 inherits the five universal gate criteria of `docs/ROADMAP-EXTERNAL-INFRA.md:13-21`. They are
**not delegable to the agent.** Each must be recorded in the PR that promotes EXT-07 into
`docs/TASKS.md` (a serialized contract change). The EXT-07-specific reading of each:

### §0.1 The thesis decision (the gate proper)
A named human owner records the decision that NilCore's identity may expand from "skills/MCP load
from local dirs / local subprocesses" toward "skills/MCP may be **fetched from a remote registry**."
This is the irreversible, outward-facing decision the whole design reserves for a human
(`ROADMAP-EXTERNAL-INFRA.md:15`). The decision must name **which registry/registries are trusted**
and **whose signing keys are pinned** — there is no default trust.

### §0.2 Artifact-verification design review (EXT-07-specific, mandatory)
Because the stressed invariant is **I7** (untrusted-until-verified), the gate additionally requires a
**recorded security review of the artifact trust model** (§3.4 below) before T01 is written:
- the signature scheme (Ed25519 detached signature over a canonical manifest digest),
- the provenance claim format and what it actually asserts (publisher identity + content digest +
  source URL, *not* "this code is safe"),
- the operator-pinned trust store (which public keys are trusted, how revocation works),
- the explicit statement that **a valid signature proves provenance, never safety** — a correctly
  signed malicious skill still must be stopped by the human gate + scope + sandbox (§10).

### §0.3 The other three universal criteria, for EXT-07
| Gate criterion (source) | EXT-07 satisfaction |
|---|---|
| **Invariants survive, not bypassed** (`:16`) | §7 ledger: I7 is *extended* by the verify-before-install gate; I3 by routing install through `selfimprove.Flow`'s human gate; no invariant weakened. |
| **Verifier still governs (I2)** (`:17`) | A self-skill/-MCP change still runs as a verified task inside `selfimprove.Flow.Run` (`selfimprove.go:84-91`); `make verify` re-runs; the install merges only on a green report. The fetch client never ships work on a self-report. |
| **Dependency budget justified (I6)** (`:18`) | §8: zero new modules. Registry client = stdlib `net/http`; signatures = stdlib `crypto/ed25519` + `crypto/sha256`; `CGO_ENABLED=0` preserved. |
| **Default-off, reversible** (`:19`) | §9 byte-identical proof: no registry URL ⇒ no network; the binary linked without the new leaves behaves identically. |

If any of §0.1–§0.3 cannot be met, EXT-07 stays on the roadmap, unbuilt.

---

## §1 The line EXT-07 crosses (sourced)

| Today (sourced) | The line EXT-07 crosses |
|---|---|
| `internal/registry` installs from **LOCAL paths only**; `Entry.Source` is "LOCAL path to the SKILL.md (no remote fetch — EXT-07)" (`internal/registry/registry.go:43`); the package doc itself names remote fetch as out of scope and gated (`registry.go:9-11`). | A network `GET` that downloads a remote artifact. |
| `nilcore registry install` works on a local `manifest.json`; remote fetch is explicitly EXT-07 (`cmd/nilcore/registry.go:13-14,71`). | A `registry fetch` / `registry publish` subcommand that talks to a remote host. |
| MCP servers are launched from **operator-configured commands**, "never model-emitted" (`internal/mcp/config.go:13-21,55-79`). | Installing an MCP `ServerSpec` whose command came from a remote registry. |
| Skills load from a local dir via `skills.LoadDir` (`internal/skills/skills.go:80-104`); a skill is a read-only `skill_<name>` tool returning instructions (`skills.go:65-77`). | A skill body authored by an unknown remote publisher entering the discovery dir. |
| `selfimprove.Flow` gates self-edits to prompts/skills/tools, never the core, always human-gated (`internal/selfimprove/selfimprove.go:32-42,77-100`). | A remotely-sourced capability change must still pass through *exactly this* gate. |
| Secrets are per-host via `SecretStore` (`internal/secrets/secrets.go:19-35`); the `ExternalStore` hook exists for corporate managers (`internal/secrets/external.go:10-18`). | A registry **publish** credential (signing key / API token) — a new standing credential, scoped & SecretStore-held. |

EXT-07 crosses the **remote-fetch/publish** line and nothing else: it does **not** add a hosted
backend NilCore operates (that registry host is somebody's infrastructure, pinned by the operator),
does **not** add multi-tenant identity (EXT-05), and does **not** add a remote control plane
(EXT-01).

---

## §2 As-is

The single most important fact: **the local versioned registry is already built and already names
EXT-07 as its remote front.** EXT-07 reuses and extends it; it never rebuilds it.

### 2.1 The shipped local spine (reuse, do not rebuild)
| Package / symbol | What it gives EXT-07 |
|---|---|
| `internal/registry` | `Manifest`/`Entry{Name,Kind,Version,Source}`, `LoadManifest` (missing ⇒ empty, opt-in, `registry.go:53-66`), `InstallSkill` (copies a local `SKILL.md` into the discovery dir, **verifies it loads, rolls back a bad install**, `registry.go:84-116`), `Installed` (`registry.go:120-128`). The remote client produces a *local* `Entry` whose `Source` points at a *verified, downloaded* file — so `InstallSkill` is reused **verbatim**. |
| `internal/skills` | `LoadDir` (`skills.go:80-104`), `parseSkill` frontmatter incl. `version` (`skills.go:106-138`), the read-only `skillTool` (`skills.go:65-77`). A fetched skill is still just a `skill_<name>` tool returning instructions — **no write surface** (`registry.go:12-14`). |
| `internal/mcp/config.go` | `ServerSpec{Name,Command,Version}` (`config.go:15-21`), `LoadConfig`/`Server` (`config.go:30-53`). Note: `Command` is "OPERATOR-configured, never model-emitted" (`config.go:13-14`) — EXT-07 must preserve this: a fetched MCP spec is **presented for operator approval**, it does not auto-arm. |
| `internal/mcp/client.go` | the per-tool `Gate` (`client.go:26-28`) checked on **every** `CallTool` (`client.go:130-135`); JSON-RPC over stdlib (`client.go:1-9`). Unchanged by EXT-07. |
| `internal/selfimprove` | `Flow{Scope,Run,Gate,Log}` and `Propose` = scope-check → verified task → human gate → merge (`selfimprove.go:67-100`); `DefaultScope` Allow `{internal/skills/, skills/, internal/tools/, docs/PERSONA.md}` / Deny `{core + contract files}` (`selfimprove.go:33-42`); "Merge is irreversible: always the human gate, no exceptions" (`selfimprove.go:93`). **This is the install gate, reused verbatim.** |
| `cmd/nilcore/registry.go` | the shipped `registry list|install` UX over the local `skillsDir()` (`registry.go:15-88`). EXT-07 **edits** this one file to add `fetch`/`publish` arms. |
| `cmd/nilcore/selfimprove.go` | the shipped `propose-edit` wiring: scope-check fail-fast, build orchestrator, `Flow` with `Run=orch.Execute`, `Gate=policy.NewConsoleApprover(...).Approve` (`selfimprove.go:43-70`). EXT-07's install reuses this exact `Flow` shape. |
| `internal/secrets` | `SecretStore` (`secrets.go:19-24`); `ExternalStore` for corporate KMS/Vault (`external.go:10-18`). Holds the **publish** signing key / token by name — never to the model, never on disk plaintext. |
| `internal/guard` | `Wrap` (I7 fence, `guard.go:14-37`) + `Suspicious` (`guard.go:41-`). A fetched manifest's human-readable fields are `guard.Wrap`'d before any model ever sees them. |
| `internal/policy` | `GateAction{PromoteToBase}` / `GateStructured` (`gateaction.go:25-94`); `NewConsoleApprover` (the nil-approver-default-denies gate). The install merge rides the existing human gate. |
| `internal/verify` | `Verifier`/`Report{Passed,Output}` (`verify.go:16-21`); `Composite` (`composite.go:31-49`). `selfimprove.Flow.Run` re-runs the project verifier on the candidate worktree. |

### 2.2 What is genuinely new (the only code EXT-07 writes)
A stdlib **fetch+verify client leaf** (`internal/regremote`), a **signature/provenance verify leaf**
(`internal/regremote/verify`), an **operator trust store** (pinned public keys), additive
**remote-source** support in `internal/registry` (a `RemoteSource` resolver that hands a *verified
local file* to the existing `InstallSkill`), additive **MCP-spec install** in `internal/registry`
(the deferred "follow-up" of `registry.go:16-17`, now in scope, still operator-approved), the
**publish** path (sign + PUT), and the `cmd/nilcore/registry.go` wiring (`fetch`/`publish` arms) +
one `docs` promotion. Every one is a new leaf or an additive seam over the shipped spine.

---

## §3 Architecture

The organizing principle: **the remote client is a pure download+verify front; the local registry +
`selfimprove.Flow` are the unchanged install spine.** Trust flows strictly one way — bytes are
untrusted until a signature+provenance check passes against the operator's pinned trust store, and
even a *verified* artifact is *trusted-as-provenance only* and must still clear the verifier + the
human gate before it can install. Nothing fetched ever becomes an instruction.

```
  nilcore registry fetch <name>@<ver> --registry https://reg.example   (opt-in; absent ⇒ no network)
            │
            ▼
  internal/regremote.Client.Fetch(ctx, ref)                          ← stdlib net/http GET, pinned host, size+timeout caps
            │   downloads: manifest.json  +  artifact bytes  +  artifact.sig (detached)  +  provenance.json
            ▼
  internal/regremote/verify.Verify(bundle, TrustStore)               ← UNTRUSTED → checks BEFORE anything touches disk-as-capability
            │   1. sha256(artifact) == manifest.digest                (integrity)
            │   2. ed25519.Verify(trustedPubKey, digest, sig)         (authenticity; key MUST be operator-pinned)
            │   3. provenance.publisher ∈ TrustStore, not revoked     (provenance)
            │   4. manifest.kind ∈ {skill, mcp}; name/version sane     (shape, fail-closed)
            │   ──► FAIL ⇒ artifact quarantined to a temp file, NEVER handed onward, audited (regremote_verify_failed)
            ▼  (on PASS only)
  write verified bytes to a temp file  ──►  registry.Entry{Kind, Source: <verified-temp-path>}
            │   (the remote client's ONLY output is a LOCAL Entry pointing at verified bytes — it never installs)
            ▼
  selfimprove.Flow.Propose(ctx, Proposal{Paths:[discovery-dir/<name>], Goal:"install fetched <kind> <name>@<ver>"})
            │   scope-check (skills/ or internal/tools/ in Allow; core/contracts DENIED)  ─ selfimprove.go:46-65
            │   Run as a VERIFIED task in a worktree (make verify re-runs)                ─ selfimprove.go:84-91
            │   HUMAN GATE before merge (policy.NewConsoleApprover; nil ⇒ deny)           ─ selfimprove.go:93-97
            ▼  (on merge only)
  registry.InstallSkill(entry, skillsDir)  /  registry.InstallMCP(spec, mcpJSONPath)     ← reused; loads+rolls-back
            │
            ▼
  loop discovers the new skill_<name> tool (read-only) / the new MCP ServerSpec (operator-approved, mcp.Gate intact)
```

### 3.1 The fetch+verify client in front of the local registry
`internal/regremote` (new leaf) owns the **only** network authority EXT-07 adds:
- `Client{BaseURL, HTTP *http.Client, MaxBytes int64, Timeout time.Duration}` over **stdlib
  `net/http`** — the codebase already hand-rolls HTTP boundaries stdlib-only (the egress proxy is a
  stdlib `http.Server`, `internal/policy/egress_proxy.go`); the registry client mirrors that.
- `Fetch(ctx, Ref) (Bundle, error)` does a bounded `GET` (host pinned to `BaseURL`, a hard
  `MaxBytes` cap, a `Timeout`, redirects to off-host hosts **refused**) and returns a `Bundle{Manifest,
  Artifact []byte, Sig []byte, Provenance}` **in memory** — it does **not** write to a capability
  directory and does **not** install. A non-2xx / oversize / off-host response is a clean error,
  never a partial install.
- `List(ctx, query)` / `Resolve(ctx, name) (versions, error)` for discovery — read-only, the
  results are `guard.Wrap`'d before display (I7).
- The client takes its egress through the existing sandbox egress allowlist when run in-sandbox;
  the registry host is an explicit allow entry (default-deny network, `CLAUDE.md` §7).

### 3.2 The verified-then-human-gated install via `selfimprove.Flow`
The remote client's **sole output** is a local `registry.Entry` (skill) or `mcp.ServerSpec` (MCP)
whose source is a **verified temp file**. The install is then *exactly* the shipped self-edit flow:
- The `Proposal.Paths` is the discovery-dir target (`internal/skills/...` or `skills/...`), which the
  `DefaultScope` Allow-list permits and the Deny-list (core/contracts) forbids
  (`selfimprove.go:33-42`) — a fetched artifact that tried to target `internal/agent/` or
  `backend.go` is **rejected at scope-check**, before any run.
- `Flow.Run` runs the install-and-load as a **verified task** (`make verify` green is non-negotiable,
  `selfimprove.go:88-91`); a skill that won't parse/load is already rolled back by
  `InstallSkill` (`registry.go:106-114`).
- `Flow.Gate` is the **human approver** (`policy.NewConsoleApprover`); merge is irreversible and never
  bypassed (`selfimprove.go:93`). A nil approver default-denies.

This is the load-bearing reuse: **EXT-07 does not invent a new install gate; it feeds the remote
artifact into the one that already exists.**

### 3.3 The artifact trust model — untrusted-until-verified (I7, the center)
Three trust tiers, strictly ordered, with downgrade-only transitions:
1. **Untrusted (downloaded bytes).** Everything `Fetch` returns. Held in memory / a quarantine temp
   file. May be hashed and signature-checked. **May never** be installed, executed, parsed-as-skill
   into the discovery dir, or shown to the model except `guard.Wrap`'d as DATA.
2. **Provenance-verified (signature + provenance pass).** The bytes' *origin* is established: this
   came from a publisher whose key the operator pinned, unmodified. **This proves provenance, never
   safety** — a verified artifact is still gated. It may now become a `registry.Entry` candidate.
3. **Operator-installed (passed scope + verifier + human gate).** Only now is it a live capability,
   and even then a skill is read-only (`skillTool`, `skills.go:65-77`) and an MCP call still hits
   `mcp.Gate` (`client.go:130-135`).

The fence that keeps a fetched artifact from becoming a controlling instruction:
- **Manifest/description text is `guard.Wrap`'d** before any model sees it (`guard.go:14-37`) and
  `guard.Suspicious` flags injection phrases for the audit trail.
- **A skill body is only ever surfaced via the read-only `skillTool`** — it returns instructions the
  *operator chose to install*, never instructions the *fetch* chose. The install decision is the
  human's, at the gate.
- **A fetched MCP `ServerSpec.Command` is presented for explicit operator approval** and never
  auto-executed — preserving "OPERATOR-configured, never model-emitted" (`mcp/config.go:13-14`).

### 3.4 Preserving `mcp.Gate`, the I7 fence, and the no-core-edit scope
| Preserved property | Mechanism (unchanged) |
|---|---|
| per-tool `mcp.Gate` | A fetched-then-installed MCP server is reached through the same `Client.CallTool` gate (`client.go:130-135`). EXT-07 adds no bypass. |
| the I7 fence | `guard.Wrap` on every fetched human-readable field; skill bodies stay behind the read-only `skillTool`; fetched bytes are DATA until the operator installs. |
| no-core-edit scope | `selfimprove.DefaultScope` Deny-list (`selfimprove.go:36-41`) rejects any fetched artifact targeting the core/contracts. EXT-07 does **not** widen the Allow-list beyond the existing skills/tools prefixes. |
| never-land | Install merge rides the existing human gate (`policy.GateStructured` / `NewConsoleApprover`); nil approver default-denies. |

### 3.5 Publish (sign + PUT)
`regremote.Client.Publish(ctx, bundle, signer)` signs the canonical manifest digest with an
**Ed25519 private key held in the `SecretStore` by name** (resolved like every other credential;
never to the model, never on disk plaintext, never logged — `secrets.go:17-24`) and `PUT`s the
bundle to the pinned registry. Publish is an **irreversible outward action** ⇒ it routes through the
human gate (a `policy.GateAction`). The publish credential is the one new standing credential and is
explicitly scoped (publish-only, one registry) per the I3 gate (§0.3).

---

## §4 The task DAG

**Namespace `EXT-07-T01 … EXT-07-T12`.** One task = one branch (`task/EXT-07-T0x`) = one PR. Owns
sets are pairwise disjoint (package dir / single file = unit of ownership). The
verify-before-anything foundation (T01–T04) lands **before** any install/publish/cmd wiring
(T07–T11), mirroring SWARM.md's foundation-before-orchestration ordering. **No task may be opened
until §0 clears.**

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-07-T01 | Operator trust store (pinned Ed25519 keys + revocation) | — | `internal/regremote/trust/` | new leaf; stdlib crypto |
| EXT-07-T02 | Signature/provenance verify leaf | EXT-07-T01 | `internal/regremote/verify/` | new leaf; **untrusted→verified** |
| EXT-07-T03 | Bundle/manifest/provenance schema leaf | — | `internal/regremote/bundle/` | new leaf; stdlib only |
| EXT-07-T04 | Fetch client (stdlib net/http, bounded, host-pinned) | EXT-07-T02, EXT-07-T03 | `internal/regremote/` (`client.go`,`fetch.go`,`*_test.go`,`deps_test.go`) | opens `internal/regremote` package |
| EXT-07-T05 | Publish client (sign + PUT, SecretStore key, gated) | EXT-07-T04 | `internal/regremote/` (`publish.go`,`publish_test.go`) | serial after T04 (same pkg) |
| EXT-07-T06 | Registry `RemoteSource` resolver (verified-file → Entry) | EXT-07-T04 | `internal/registry/remote.go`, `remote_test.go` | additive; sole new-file owner |
| EXT-07-T07 | Registry MCP-spec install (the deferred follow-up) | EXT-07-T03 | `internal/registry/mcp.go`, `mcp_test.go` | additive; sole new-file owner |
| EXT-07-T08 | Install adapter: fetched Entry → `selfimprove.Proposal` | EXT-07-T06, EXT-07-T07 | `internal/regremote/install/` | new leaf; the gate-feeding glue |
| EXT-07-T09 | `nilcore registry fetch` arm | EXT-07-T08 | `cmd/nilcore/registry.go` | **edit** the shipped file |
| EXT-07-T10 | `nilcore registry publish` arm | EXT-07-T05 | `cmd/nilcore/registry.go` | **edit** the shipped file — serial with T09 |
| EXT-07-T11 | Egress allow + config schema (`onboard.Config.Registry`) | EXT-07-T04 | `internal/onboard/onboard.go` | **contract (config schema)** — serialized |
| EXT-07-T12 | Docs + CHANGELOG promotion (Phase EXT-07) | EXT-07-T09, EXT-07-T10, EXT-07-T11 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs)** — serialized last |

> **Owns-disjointness note.** `cmd/nilcore/registry.go` is owned across two tasks (T09, T10); per
> `CLAUDE.md` §5 rule 3 they are therefore **serialized** (T10 after T09), not parallel — the file
> is the unit of ownership. `internal/regremote` package dir is opened by T04 and extended by T05,
> which serialize as sibling files (package = unit of ownership, the same trade SWARM.md SW-T09…T13
> makes). All other Owns sets are pairwise disjoint.

---

## §5 Per-task specs

#### EXT-07-T01 — Operator trust store (pinned Ed25519 keys + revocation)
- **Goal:** the operator-controlled root of trust — a set of **pinned** Ed25519 public keys
  (publisher identity → key), with a revocation list, loaded from an operator file. **There is no
  default-trusted key.** An empty/absent trust store ⇒ every verification fails closed.
- **Depends on:** — (stdlib `crypto/ed25519`, `encoding/json`, `os`).
- **Owns:** `internal/regremote/trust/` (`trust.go`, `trust_test.go`, `deps_test.go`).
- **Acceptance:** `TrustStore{Keys map[publisher]ed25519.PublicKey, Revoked map[string]bool}`;
  `Load(path) (TrustStore, error)` (missing ⇒ **empty store**, not an error, so it is opt-in but
  fail-closed); `Trusted(publisher) (ed25519.PublicKey, bool)` returns false for unknown **or**
  revoked; key material is parsed from operator-pinned PEM/base64, never fetched; the store is
  immutable after load. `deps_test.go` asserts no `agent`/`super`/`project` import and no
  network/RPC import (a trust store must never fetch its own keys).
- **Verify:** `make verify`; `go test -race ./internal/regremote/trust/...`: empty store ⇒
  `Trusted` false for all; pinned key resolves; revoked key ⇒ false; malformed key ⇒ load error.
- **Notes:** stdlib `crypto/ed25519` only (I6). Revocation is a local list the operator maintains —
  no remote CRL/OCSP (that would be a standing external dependency).

#### EXT-07-T02 — Signature/provenance verify leaf
- **Goal:** the **untrusted→provenance-verified** transition — given downloaded bytes + a detached
  signature + a provenance claim + the trust store, decide pass/fail **deterministically, with no
  network and no disk-as-capability write**. The single most security-critical leaf.
- **Depends on:** EXT-07-T01 (trust store), EXT-07-T03 (bundle types).
- **Owns:** `internal/regremote/verify/` (`verify.go`, `verify_test.go`, `deps_test.go`).
- **Acceptance:** `Verify(b bundle.Bundle, ts trust.TrustStore) (Result, error)` runs in fixed
  order, **fail-closed at the first failure**: (1) `sha256(b.Artifact) == b.Manifest.Digest`
  (integrity); (2) the publisher's key is `ts.Trusted(...)` — unknown/revoked ⇒ **fail, no further
  work**; (3) `ed25519.Verify(key, b.Manifest.Digest, b.Sig)` (authenticity); (4) provenance
  internal consistency (`provenance.digest == manifest.digest`, `kind ∈ {skill,mcp}`, name/version
  charset-sane); a `Result{OK bool, Publisher, Digest string}` with **no** model-authored field
  echoed in any error message (I7 — error strings are harness-authored, reference name/digest only).
  A nil/zero trust store ⇒ every artifact fails. `Verify` makes **zero** network calls and writes
  **zero** files.
- **Verify:** `make verify`; `go test -race ./internal/regremote/verify/...`: golden good bundle ⇒
  OK; tampered artifact byte ⇒ digest fail; wrong-key signature ⇒ authenticity fail; valid signature
  by a **non-pinned** key ⇒ fail (the spoofing case, §10); revoked publisher ⇒ fail; digest/manifest
  mismatch ⇒ fail; assert no `Value`/URL from the manifest appears in error output;
  `deps_test.go` asserts no network/RPC/orchestrator import.
- **Notes:** **this is the I7 enforcement point.** A passing `Verify` proves *provenance*, never
  *safety* — the package docstring states this explicitly so a follow-on cannot read it as "verified
  ⇒ safe to auto-install."

#### EXT-07-T03 — Bundle/manifest/provenance schema leaf
- **Goal:** the stdlib-typed shapes a remote bundle carries, with a strict decoder, so a malformed
  remote response fails fast before any crypto.
- **Depends on:** — (stdlib `encoding/json`).
- **Owns:** `internal/regremote/bundle/` (`bundle.go`, `bundle_test.go`).
- **Acceptance:** `Manifest{Name,Kind,Version,Digest,SourceURL}`, `Provenance{Publisher,Digest,
  SourceURL,BuiltAt}`, `Bundle{Manifest,Artifact []byte,Sig []byte,Provenance}`;
  `DecodeManifest`/`DecodeProvenance` use `json.Decoder` with `DisallowUnknownFields` (reject
  unexpected keys, fail-closed); a `Kind ∉ {skill,mcp}` ⇒ decode error; size-of-artifact is not
  decided here (the client caps it). No behavior, pure types + decode.
- **Verify:** `make verify`; `go test ./internal/regremote/bundle/...`: round-trip; unknown field ⇒
  error; bad kind ⇒ error; missing required field ⇒ error.
- **Notes:** **no JSON-schema module** (I6) — a Go struct + the stdlib strict decoder, exactly the
  pattern `onboard`/`mcp.Config` already use (`mcp/config.go:30-43`).

#### EXT-07-T04 — Fetch client (stdlib net/http, bounded, host-pinned)
- **Goal:** the bounded, host-pinned download path — the only new outbound authority.
- **Depends on:** EXT-07-T02, EXT-07-T03.
- **Owns:** `internal/regremote/` (`client.go`, `fetch.go`, `list.go`, `*_test.go`, `deps_test.go`)
  — opens the package.
- **Acceptance:** `Client{BaseURL *url.URL, HTTP *http.Client, MaxBytes int64, TrustStore
  trust.TrustStore}`; `New(baseURL, ts, opts) (*Client, error)` (rejects a non-https BaseURL unless
  an explicit `--insecure` test hook); `Fetch(ctx, Ref{Name,Version}) (bundle.Bundle, error)` does a
  bounded `GET` (an `io.LimitReader(MaxBytes)`, a ctx deadline, **off-host redirects refused** via a
  `CheckRedirect` that compares hosts), decodes via `bundle`, then calls `verify.Verify` — **returns
  an error (and the bytes are dropped) on any verify failure**; `Fetch` never writes to a capability
  directory; `WriteVerified(b, dir) (path, error)` writes a *verified* bundle's artifact to a temp
  file under `dir` with `O_NOFOLLOW`-equivalent care (mirror `worktreefs` discipline) and returns the
  local path; `List`/`Resolve` are read-only and `guard.Wrap` their displayed fields. A nil/zero
  `BaseURL` ⇒ `Client` construction is never attempted by the cmd (no URL ⇒ no network — §9).
- **Verify:** `make verify`; `go test -race ./internal/regremote/...` against an
  `httptest.Server`: happy path ⇒ verified bundle; oversize body ⇒ capped error (assert ≤ MaxBytes
  read); off-host redirect ⇒ refused; non-2xx ⇒ clean error; a 200 with a tampered artifact ⇒
  `Fetch` errors (verify wired in); `deps_test.go` asserts the package imports `net/http` but **no**
  `agent`/`super`/`project` and runs no server.
- **Notes:** stdlib `net/http` only (I6, §8). The egress proxy precedent (`policy/egress_proxy.go`)
  shows the codebase already does host-pinned stdlib HTTP. Default-deny network: the registry host is
  an explicit egress allow entry (T11).

#### EXT-07-T05 — Publish client (sign + PUT, SecretStore key, gated)
- **Goal:** sign a bundle's canonical digest with an Ed25519 key from the `SecretStore` and `PUT` it
  to the pinned registry, behind the human gate (publish is irreversible/outward).
- **Depends on:** EXT-07-T04 (same package).
- **Owns:** `internal/regremote/` (`publish.go`, `publish_test.go`).
- **Acceptance:** `Signer` interface `Sign(digest []byte) ([]byte, error)`; a
  `SecretStoreSigner{Store secrets.SecretStore, KeyName string}` resolves the **private key by name**
  (never to the model, never logged — `secrets.go:17-24`), signs `sha256(canonicalManifest)`;
  `Client.Publish(ctx, bundle, signer) error` builds the detached sig + provenance and `PUT`s the
  bundle; the cmd wraps the call in a `policy.GateAction` (the human approves the outward publish);
  a missing key ⇒ clean error; the private key bytes are zeroed after use where the stdlib allows.
- **Verify:** `make verify`; `go test -race ./internal/regremote/...`: sign+verify round-trip with a
  test keypair; `Publish` against `httptest.Server` asserts the PUT body carries a valid sig over the
  digest; a `SecretStore` miss ⇒ error; assert the key value never appears in any log/error string.
- **Notes:** the publish credential is the one new standing credential — scoped (publish-only, one
  registry), SecretStore-held, gated (§0.3 I3). Serial after T04 (same package dir).

#### EXT-07-T06 — Registry `RemoteSource` resolver (verified-file → Entry)
- **Goal:** let `internal/registry` accept a *verified, already-downloaded* file as a skill source —
  additively, without changing local-only behavior. The remote client hands the registry a local
  verified path; the registry's existing `InstallSkill` then runs **verbatim**.
- **Depends on:** EXT-07-T04. **Owns:** `internal/registry/remote.go` (new), `remote_test.go` (new).
- **Acceptance:** `EntryFromVerified(name, version, kind, verifiedLocalPath string) (Entry, error)`
  builds an `Entry` whose `Source` is the **verified local temp path** (so the package doc invariant
  "Sources are LOCAL paths" at `registry.go:8-9` literally still holds — the bytes are local and
  verified before this is called); a guard that the path is the one `regremote.WriteVerified`
  produced (not an arbitrary path); `InstallSkill` is unchanged and reused. No network code enters
  `internal/registry` — the fetch authority stays in `internal/regremote`.
- **Verify:** `make verify`; `go test ./internal/registry/...`: `EntryFromVerified` → `InstallSkill`
  installs + loads + rolls back a bad body (the existing rollback path, `registry.go:106-114`);
  existing `registry_test` stays green (additive); a `deps_test` asserts `internal/registry`
  still imports **no** `net/http`.
- **Notes:** the deliberate seam — keeps the network authority isolated in `regremote`; `registry`
  stays "install from a local verified path," its doc invariant intact.

#### EXT-07-T07 — Registry MCP-spec install (the deferred follow-up)
- **Goal:** install an `mcp.ServerSpec` into the operator's `mcp.json` — the follow-up
  `registry.go:16-17` deferred — **operator-approved**, never auto-armed.
- **Depends on:** EXT-07-T03. **Owns:** `internal/registry/mcp.go` (new), `mcp_test.go` (new).
- **Acceptance:** `InstallMCP(spec mcp.ServerSpec, mcpJSONPath string) error` appends/updates the
  spec in the operator's `mcp.json` via a read-modify-write (preserving existing servers,
  byte-compatible with the shipped `mcp.LoadConfig`, `config.go:30-43`); **the fetched `Command` is
  passed through unchanged but flagged** so the cmd presents it for explicit operator approval
  (preserving "OPERATOR-configured, never model-emitted", `config.go:13-14`); idempotent on
  re-install; a malformed spec ⇒ error, no write. **Installing an MCP spec does NOT spawn it** —
  spawning still happens at the operator-driven `mcp` command, behind `mcp.Gate`.
- **Verify:** `make verify`; `go test ./internal/registry/...`: install appends a server,
  `mcp.LoadConfig` reads it back; existing servers preserved; idempotent re-install; malformed ⇒ no
  write; assert install does not call `mcp.Spawn`.
- **Notes:** closes the "mcp-server install is a follow-up" gap (`registry.go:16-17`,
  `cmd/nilcore/registry.go:71`) — but **only the config write**, never auto-execution; the per-tool
  `mcp.Gate` and operator-launch model are untouched.

#### EXT-07-T08 — Install adapter: fetched Entry → `selfimprove.Proposal`
- **Goal:** the glue that turns a verified `Entry`/`ServerSpec` into a `selfimprove.Proposal` and
  runs it through the **existing** `Flow` (scope → verify → human gate). The single point where a
  fetched artifact meets the install gate.
- **Depends on:** EXT-07-T06, EXT-07-T07.
- **Owns:** `internal/regremote/install/` (`install.go`, `install_test.go`, `deps_test.go`).
- **Acceptance:** `Proposal(name, version, kind, discoveryPath string) selfimprove.Proposal` whose
  `Paths` is the discovery-dir target (skills/ or the mcp.json under an allowed prefix) and `Goal`
  is a harness-authored `"install fetched <kind> <name>@<version>"` (never a model/remote string —
  I7); `Run(ctx, flow *selfimprove.Flow, p Proposal) (bool, error)` is a thin call to
  `flow.Propose` — **all gating is `selfimprove`'s** (T08 adds no gate of its own); a proposal whose
  path is outside the Allow-list is rejected by `Scope.Check` *before* any install (assert it).
- **Verify:** `make verify`; `go test ./internal/regremote/install/...`: a skill proposal targeting
  `skills/x/SKILL.md` passes scope; a proposal targeting `internal/agent/...` is **rejected at
  scope-check** (the no-core-edit proof); the `Goal` string contains no fetched/model text; a fake
  `Flow` with `Gate` returning false ⇒ not merged (gate honored).
- **Notes:** **the load-bearing reuse** — EXT-07 invents no install gate; this feeds the artifact
  into `selfimprove.Flow` (`selfimprove.go:77-100`) verbatim.

#### EXT-07-T09 — `nilcore registry fetch` arm
- **Goal:** the operator front door for remote fetch+install — extend the shipped
  `cmd/nilcore/registry.go` with a `fetch` arm wiring `regremote.Client` → `install` → `Flow`.
- **Depends on:** EXT-07-T08. **Owns:** `cmd/nilcore/registry.go` (**edit** the shipped file).
- **Acceptance:** `registry fetch <name>[@<version>] --registry <url>` (and `NILCORE_REGISTRY`)
  constructs a `regremote.Client` with the operator trust store, `Fetch`es, `WriteVerified`s, builds
  the `install` proposal, and runs it through a `selfimprove.Flow{Gate: NewConsoleApprover(...).Approve}`
  exactly like `cmd/nilcore/selfimprove.go:59-70`; **no `--registry`/env ⇒ a clear error, never a
  silent default host**; a verify failure prints a harness-authored reason and exits non-zero
  (artifact discarded); existing `list`/`install` arms unchanged.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, `httptest.Server` + fake trust +
  fake approver): fetch+verify+gate-approve ⇒ installed; verify-fail ⇒ non-zero, not installed;
  gate-deny ⇒ not installed; **no `--registry` ⇒ error and no network call** (the §9 default-off
  proof at the cmd boundary).
- **Notes:** edits the file whose doc already says remote fetch is EXT-07 (`registry.go:13-14`); T09
  flips that line to "implemented, gated." Serial with T10 (same file).

#### EXT-07-T10 — `nilcore registry publish` arm
- **Goal:** the publish front door — sign + PUT behind the human gate.
- **Depends on:** EXT-07-T05. **Owns:** `cmd/nilcore/registry.go` (**edit** — serial after T09).
- **Acceptance:** `registry publish <path> --registry <url> --key-name <secret>` builds a bundle,
  resolves the signing key via the `SecretStore` (`SecretStoreSigner`), wraps the `PUT` in a
  `policy.GateAction` (human approves the outward publish; nil approver default-denies), and prints
  the published name@version; a missing key / missing `--registry` ⇒ clean error.
- **Verify:** `make verify`; `go test ./cmd/nilcore/...`: publish against `httptest.Server` with a
  fake SecretStore + approve ⇒ PUT carries a valid sig; gate-deny ⇒ no PUT; key miss ⇒ error; key
  value never logged.
- **Notes:** publish is the one outward-irreversible op EXT-07 adds — gated like every irreversible
  action (`CLAUDE.md` §7).

#### EXT-07-T11 — Egress allow + config schema (`onboard.Config.Registry`)
- **Goal:** additively extend the config schema with one optional `Registry *RegistryConfig`
  (URL + trust-store path + max-bytes) and add the registry host to the egress allowlist when in
  sandbox; v1-config-compatible.
- **Depends on:** EXT-07-T04. **Owns:** `internal/onboard/onboard.go`, `onboard_test.go`.
- **Acceptance:** default-zero `Registry *RegistryConfig json:"registry,omitempty"` so every existing
  config parses unchanged under `DisallowUnknownFields`; `Validate()` gains a registry clause (https
  URL, trust-store path exists, max-bytes > 0 when set; loud error otherwise); the registry host
  becomes an egress allow entry only when `Registry` is set (default-deny otherwise, `CLAUDE.md` §7);
  old configs without `registry` parse.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip with `Registry` set; old
  config parses; `Validate` rejects http (non-https), missing trust store, zero max-bytes.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface),
  treated as a contract surface (the same posture SWARM.md SW-T08 takes). `onboard → regremote` is
  downward (no cycle).

#### EXT-07-T12 — Docs + CHANGELOG promotion (Phase EXT-07)
- **Goal:** promote this plan into the canonical docs and ledger.
- **Depends on:** EXT-07-T09, EXT-07-T10, EXT-07-T11. **Owns:** `docs/TASKS.md`,
  `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md`.
- **Acceptance:** `docs/TASKS.md` EXT-07 DAG rows + specs (noting the local registry spine is
  **reused**, not rebuilt); `docs/ARCHITECTURE.md` a "Remote skills/MCP registry (EXT-07, gated)"
  subsection (the untrusted→verified→gated trust model, the `regremote` leaf isolating network
  authority, `mcp.Gate`/I7-fence/no-core-edit preserved, publish-credential scoping) + the new leaf
  rows in the layer-map with import sets; `docs/ROADMAP-EXTERNAL-INFRA.md` §8 marked "plan staged,
  gated — see EXT-07 plan"; `CLAUDE.md` one repository-map line (no invariant text change — the
  invariants are unchanged, which is the point); `CHANGELOG.md` one `## [Unreleased]` line per merged
  task; `README.md` the `registry fetch`/`publish` usage + the trust-store setup + the default-off
  note + the honest caveats (signature proves provenance not safety; install is human-gated).
- **Verify:** `make verify` (docs don't break the build); a markdown pass; manual review that the
  layer-map import sets match the actual `go list -deps` of each new leaf.
- **Notes:** **serialized — contract files. Lands last. Cannot start until §0 cleared and T09/T10/T11
  merged.**

---

## §6 Parallel wave map & critical path

A fleet executes in ordered **waves**; every task in a wave has all deps merged to `main` and a
pairwise-disjoint Owns set. `internal/regremote` is one package = one Owns unit (T04 opens it, T05
extends it as a sibling file), and `cmd/nilcore/registry.go` is one file owned by T09→T10, so those
two are the intra-plan serial sub-chains.

```
WAVE 1  (3 concurrent — no-dep new leaves, each independently make-verify-green)
  ├── EXT-07-T01  internal/regremote/trust/      (the root of trust; downstream verify imports it)
  └── EXT-07-T03  internal/regremote/bundle/      (the wire shapes)
       (T01 ∥ T03 — disjoint dirs, no deps)

WAVE 2  (1 — the security keystone)
  └── EXT-07-T02  internal/regremote/verify/      (T01, T03)   ← untrusted→verified gate

WAVE 3  (1 — opens the network leaf)
  └── EXT-07-T04  internal/regremote/{client,fetch,list}.go   (T02, T03)   ← the only new outbound authority

WAVE 4  (4 concurrent)
  ├── EXT-07-T05  internal/regremote/publish.go             (T04)  ← SERIAL pt: same pkg as T04, sibling file
  ├── EXT-07-T06  internal/registry/remote.go               (T04)  ← additive, sole new-file owner
  ├── EXT-07-T07  internal/registry/mcp.go                  (T03)  ← additive, sole new-file owner
  └── EXT-07-T11  internal/onboard/onboard.go               (T04)  ← SERIAL pt: config-schema contract
       (T05 serializes after T04 in the package dir; T06/T07/T11 are pairwise-disjoint and parallel)

WAVE 5  (1)
  └── EXT-07-T08  internal/regremote/install/     (T06, T07)   ← feeds the selfimprove gate

WAVE 6  (1 lane, serial inside cmd/nilcore/registry.go)
        EXT-07-T09 registry fetch (T08)  →  EXT-07-T10 registry publish (T05; serial after T09, same file)

WAVE 7  (1 — SERIAL pt: docs contract)
        EXT-07-T12  docs/* + CLAUDE.md + README.md + CHANGELOG.md   (T09, T10, T11)   ← sole docs editor
```

**Peak concurrency = 4 (wave 4).** Critical path (longest dependency chain) — **8 sequential
merges:**

```
EXT-07-T01 → EXT-07-T02 → EXT-07-T04 → EXT-07-T05 → EXT-07-T10 → EXT-07-T12
                                    └→ EXT-07-T08 → EXT-07-T09 → EXT-07-T12
```
The two tails converge at T12; the longer is `T01→T02→T04→T08→T09→T12` and
`T01→T02→T04→T05→T10→T12` — both length 6; counting the wave-4 serialization the realized merge
chain is 6 deep with T05/T10 serial-in-file adding the `registry.go` constraint.

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/regremote` package dir — T04 opens; T05 serializes as a sibling file.
2. `internal/onboard/onboard.go` — T11 only (config schema contract).
3. `cmd/nilcore/registry.go` — T09 then T10 (file = unit of ownership).
4. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` — T12 only.

**Foundation-before-orchestration holds:** the fetch client (T04) literally cannot be trusted to
hand bytes onward until `verify.Verify` (T02) exists; the cmd (T09) cannot install until the
`install` adapter (T08) routes through `selfimprove.Flow`. No edge points to a later task; the
work-selection rule walks waves 1→7 with no forced collision.

---

## §7 Per-invariant ledger

Every EXT-07 task, against the invariants it stresses and the one rule it can never break. The
invariants are **extended, not bypassed** — the point of the plan.

| Invariant | How EXT-07 preserves/extends it |
|---|---|
| **I1** frozen contract | `backend.CodingBackend.Run(ctx,Task)→Result` is **untouched**. A fetched artifact is a worktree **file** installed by the existing `registry`/`selfimprove` path, then discovered by the loop as a tool — no new field on `Task`/`Result`, no new interface on the contract. `go.mod`/`Makefile`/`channel.go` untouched. |
| **I2** verifier sole authority | A self-skill/-MCP install runs as a **verified task** inside `selfimprove.Flow.Run` (`selfimprove.go:84-91`); `make verify` re-runs; the install merges only on a green report (`selfimprove.go:88-91`). The fetch client's signature pass is *provenance*, **not** a "done" verdict — it never ships work on a self-report. `InstallSkill` additionally re-loads + rolls back a bad body (`registry.go:106-114`). |
| **I3** no ambient authority / install gate | The install merge routes through the **human gate** (`policy.NewConsoleApprover`, `selfimprove.go:93-97`; nil ⇒ deny). The **publish** credential is a scoped, SecretStore-held Ed25519 key resolved **by name**, never to the model, never on disk plaintext, never logged (`secrets.go:17-24`). The new outbound authority is one pinned registry host on an explicit egress allow entry (default-deny otherwise). No broad credential is held by default. |
| **I4** sandboxed execution | A fetched MCP `ServerSpec.Command` is **not** auto-executed by install (T07 writes config only); when later spawned via the operator `mcp` command it runs as a subprocess under the existing sandbox/egress model, and every tool call hits `mcp.Gate` (`client.go:130-135`). Fetched skill bodies never execute — they are read-only `skillTool` text. The fetch client itself runs no fetched code. |
| **I5** append-only audit | Every step appends a metadata-only event: `regremote_fetch`, `regremote_verify_failed` (with name/digest, never the artifact body), `regremote_verified`, and the existing `self_edit_accepted`/`self_edit_unverified`/`self_edit_gated`/`self_edit_merged` (`selfimprove.go:79-98`) on install, plus the publish `GateAction`. History is never mutated. |
| **I6** zero-dep core | **No new module.** Registry client = stdlib `net/http`/`net/url`; signatures = stdlib `crypto/ed25519` + `crypto/sha256`; JSON = stdlib `encoding/json` strict decoder (no JSON-schema lib). `CGO_ENABLED=0` preserved (§8). |
| **I7** untrusted-until-verified (the center) | A fetched artifact is **untrusted data** until `verify.Verify` passes signature+provenance against the operator-pinned trust store (T02) — and even then it is *provenance-verified, not safe*, and must still clear scope + the verifier + the human gate before installing. Every fetched human-readable field is `guard.Wrap`'d before any model sees it (`guard.go:14-37`); skill bodies stay behind the read-only `skillTool`; a fetched byte **never** becomes a controlling instruction. Error strings are harness-authored and never echo a fetched `Value`/URL. |
| **no-core-edit scope** | `selfimprove.DefaultScope` Deny-list (`selfimprove.go:36-41`) rejects any fetched artifact whose path targets the core/contracts; T08's test asserts an `internal/agent/...` target is rejected at scope-check. EXT-07 does **not** widen the Allow-list beyond the existing skills/tools prefixes. |
| **never-land** | Install merge and publish both ride the existing human gate; nil approver default-denies. EXT-07 constructs no new auto-land path. |

**The single line under all of them (matching `ROADMAP-EXTERNAL-INFRA.md:212`):** `I3` — no
ambient authority. EXT-07 adds exactly one new authority (fetch/publish to one pinned host); it is
scoped, gated, credential-stored, and never handed to the model — and only after the §0 gate clears.

---

## §8 Module justifications (registry / signature client)

**Zero new modules. `CGO_ENABLED=0` preserved.** Every piece uses the standard library, matching the
codebase's stdlib-first discipline (the egress proxy is a stdlib `http.Server`; secrets hand-roll
PBKDF2 to stay stdlib, `ROADMAP-EXTERNAL-INFRA.md:18`).

| Capability | Stdlib package(s) used | Why no module |
|---|---|---|
| Registry HTTP client (GET/PUT, host-pin, redirect-refuse, size cap) | `net/http`, `net/url`, `io` (`LimitReader`) | The codebase already runs a stdlib `http.Server` (`policy/egress_proxy.go`) and a stdlib JSON-RPC MCP client (`mcp/client.go:1-9`). A registry client is the same shape — no SDK needed. |
| Artifact signature (detached, verify + sign) | `crypto/ed25519` | Ed25519 is in the stdlib; small keys, fast verify, deterministic — no `golang.org/x/crypto` (which would be a new module). |
| Content integrity digest | `crypto/sha256` | stdlib. |
| Trust-store key parsing | `encoding/pem`, `encoding/base64`, `crypto/x509` (only `ParsePKIXPublicKey` if PEM-wrapped) | all stdlib. |
| Bundle/manifest/provenance schema | `encoding/json` (`Decoder` + `DisallowUnknownFields`) | stdlib strict decode — the exact pattern of `mcp.LoadConfig` (`config.go:30-43`) and `onboard`. **No JSON-schema module.** |
| Credential storage | existing `internal/secrets` (`SecretStore`, `ExternalStore`) | the signing key rides the shipped seam (`secrets.go:19-24`, `external.go:10-18`) — nothing new. |

If a future need arises for a signature format the stdlib cannot express (e.g. Sigstore/cosign
bundles, or TUF metadata), that is a **separate, justified, gated** module decision per `CLAUDE.md`
§2 invariant 6 — explicitly **out of scope** for the v1 EXT-07 plan, which is stdlib-only on purpose.

---

## §9 Default-off, byte-identical proof

The default `nilcore` binary is **byte-identical** with EXT-07 absent. Proven, not asserted:

1. **No network without a registry URL.** `registry fetch`/`publish` require an explicit
   `--registry`/`NILCORE_REGISTRY`; absent ⇒ a clear error and **no `regremote.Client` is ever
   constructed** (T09 test asserts no network call). The shipped `registry list`/`install` arms keep
   their **exact** local-only behavior (`cmd/nilcore/registry.go:35-88`) — `InstallSkill` still reads
   a LOCAL `Source` (`registry.go:91`).
2. **No existing package imports a `regremote` leaf.** Enforced by an import-graph test (the SWARM.md
   SW-T17 precedent): `internal/registry`, `internal/skills`, `internal/mcp`, `internal/selfimprove`,
   `internal/agent`, etc. import **nothing** under `internal/regremote`; only `cmd/nilcore` (the
   fetch/publish arms) and `internal/onboard` (the config type) do. The local registry spine is
   reachable and unchanged on the default path.
3. **No `init()` with global side effects** in the new leaves — so merely linking them cannot change
   behavior (enforced by an `init()`-free test, the SWARM.md precedent).
4. **The local registry's doc invariant still literally holds.** `internal/registry`'s "Sources are
   LOCAL paths" (`registry.go:8-9`) remains true: the remote client downloads + **verifies** bytes to
   a **local** file *before* `registry` ever sees them; `internal/registry` itself imports no
   `net/http` (T06 `deps_test`). The network authority lives only in `internal/regremote`.
5. **`onboard.Config` is additive + omitempty** (T11): every existing config parses unchanged under
   `DisallowUnknownFields`; `Registry` zero ⇒ today's wiring exactly.

The byte-identity argument is the same the local registry already makes (the `Version` field is
"omitted when absent so existing mcp.json files are byte-identical", `mcp/config.go:18-20`; skills'
`Version` is "byte-identical for existing skills", `skills.go:23-25`).

---

## §10 Risks

The product line, restated for EXT-07: **NilCore does not make a remote registry trustworthy by
fetching more skills. It makes it trustworthy by refusing to install anything it cannot verify the
provenance of, and refusing to ship anything the human did not gate.** A valid signature is necessary,
never sufficient.

| Risk | Vector | Mitigation (sourced to the design) |
|---|---|---|
| **Supply-chain compromise** (registry host serves a tampered artifact) | A MITM or a compromised registry returns altered bytes / a swapped manifest. | Content digest check (`sha256(artifact)==manifest.digest`, T02) + Ed25519 signature over the digest by an **operator-pinned** key (T01/T02) — a tampered byte fails the digest; an unsigned/wrong-key swap fails authenticity. HTTPS + host-pin + off-host-redirect-refuse (T04). The artifact is **never installed** on any verify failure (T04: bytes dropped). |
| **Signature spoofing** (attacker signs with their own key) | A correctly-formed signature by a key the operator never pinned. | The trust store has **no default key** (T01); `verify.Verify` fails closed on a non-pinned **or** revoked publisher *before* checking the signature math (T02). A valid signature by an untrusted key ⇒ **fail** (explicit T02 test). This is the headline test case. |
| **Malicious skill** (a *correctly-signed, trusted-publisher* artifact that is harmful) | A trusted publisher is compromised, or turns hostile, and ships a malicious skill/MCP spec. | **Signature proves provenance, not safety** (stated in the T02 docstring). Defense in depth: (a) install is **human-gated** (`selfimprove.go:93`) — the operator reviews before merge; (b) a skill is a **read-only** `skill_<name>` tool that only returns text (`skills.go:65-77`) — no write surface; (c) the skill body, when surfaced, is operator-installed text, not a controlling instruction, and any fetched field shown pre-install is `guard.Wrap`'d (I7); (d) a malicious **MCP** spec's command is **not** auto-executed (T07 writes config only), is presented for explicit operator approval, runs in the sandbox when spawned, and every call hits `mcp.Gate` (`client.go:130-135`); (e) the `selfimprove` scope **denies** any artifact targeting the core/contracts (`selfimprove.go:36-41`). |
| **Prompt injection via manifest/skill text** | A fetched description/body carries "ignore your instructions…". | Every fetched human-readable field is `guard.Wrap`'d before the model sees it (`guard.go:14-37`); `guard.Suspicious` flags injection phrases to the audit trail (`guard.go:41-`); error strings are harness-authored and never echo fetched text (T02/T08 tests). A fetched byte never becomes an instruction (I7). |
| **Credential leak** (the publish signing key) | The Ed25519 private key for publishing. | Held in the `SecretStore` by name, resolved per-op, never to the model, never logged, never on disk plaintext (`secrets.go:17-24`); zeroed after use where stdlib allows; publish is gated (T05/T10). The corporate-manager hook (`external.go:10-18`) can front a KMS. |
| **Quarantine bypass** (verified bytes written somewhere executable) | A path-traversal in `WriteVerified`. | `WriteVerified` writes to a temp file under a controlled dir with `worktreefs`-style `O_NOFOLLOW`/atomic-rename discipline (T04); `registry.EntryFromVerified` guards the path is the one the client produced (T06); install still routes through scope + verifier + gate. |
| **Revocation lag** (a key is compromised after pinning) | A pinned key must be distrusted. | Operator-maintained local revocation list (T01); `Trusted` returns false for revoked. (A remote CRL/OCSP is deliberately **out of scope** — it would add a standing external dependency; the operator updates the list, matching the per-host trust model.) |
| **Scope drift** (a follow-on quietly widens what can install) | A later PR adds the core to the Allow-list, or auto-installs on verify. | The §7 ledger + the SW-T18-style ARCHITECTURE note (T12) pin: install is human-gated, verify ≠ safe, the Allow-list is the existing skills/tools prefixes only. The `selfimprove.DefaultScope` is reused unchanged, not re-implemented. |

**Honest caveats (stated in the docstrings, the tests, and the README so the brief never
over-promises):**
- A valid signature proves **provenance, never safety** — a trusted-publisher malicious skill is
  stopped by the human gate + read-only-skill + sandbox + scope, not by the signature.
- Revocation is **operator-maintained and local** — no remote CRL/OCSP (that would be a standing
  external dependency, drifting toward EXT-06).
- The registry **host** is somebody's infrastructure pinned by the operator — NilCore does not
  operate a hosted marketplace (that would be a far larger thesis change; out of scope).
- MCP-as-data-source (a verdict contingent on a standing remote API) stays out of scope, consistent
  with the SWARM.md §13 boundary.

---

*EXT-07 is the gap NilCore is genuinely behind on for capability distribution — and the reason it is
behind is, on purpose, that remote fetch grants network authority the design refuses by default.
This plan adds that authority the way the rest of the system earns trust: a pinned trust store, a
provenance-verify gate, the read-only skill surface, the per-tool `mcp.Gate`, the `selfimprove`
human gate that has the only vote on "installed," and a verifier that still has the only vote on
"done." It ships only after the §0 thesis gate clears. <3*
