# EXT-03 — In-editor surface + custom inline-edit / next-edit models (GATED execution plan)

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` → `docs/PRINCIPLES.md` (principle 2) → `docs/ROADMAP-EXTERNAL-INFRA.md` (§0, §4) → `docs/SWARM.md` (depth template) → this file.

> **STATUS: BLOCKED behind the §0 gate of `docs/ROADMAP-EXTERNAL-INFRA.md`.** This is a *ready-when-the-gate-clears* blueprint, not an eligible task set. None of the `EXT-03-T##` tasks below may be promoted into `docs/TASKS.md` until a **recorded human thesis decision** (§0.1) exists. The plan is split so the **low-bar path can clear a narrow gate independently of the high-bar path** (§0). The integrator never lands here — promotion of any task into the work queue is itself a serialized contract change.

---

## Table of contents

- [§S Summary](#s-summary)
- [§0 The gate — split low-bar vs high-bar](#0-the-gate)
- [§1 As-is: the seams this reuses (sourced)](#1-as-is-the-seams-this-reuses)
- [§2 Architecture](#2-architecture)
- [§3 The task DAG (EXT-03-T01 … EXT-03-T12)](#3-the-task-dag)
- [§4 Per-task specs](#4-per-task-specs)
- [§5 Parallel wave map & critical path](#5-parallel-wave-map--critical-path)
- [§6 Per-invariant ledger](#6-per-invariant-ledger)
- [§7 Module justifications (stdlib-only proof)](#7-module-justifications)
- [§8 Default-off byte-identical proof](#8-default-off-byte-identical-proof)
- [§9 Risks & honest caveats](#9-risks--honest-caveats)

---

## §S Summary

EXT-03 wants what Cursor (Fusion), Copilot (Next Edit Suggestions), and Windsurf ship: a **low-latency in-editor surface** — Tab autocomplete, next-edit prediction, inline-diff apply — backed by purpose-built models, exposed through an editor extension or an LSP server. NilCore today has **no editor surface by design** (CLI / headless / chat / phone) and consumes the LSP **inbound** as a client spawning `gopls` (`internal/codeintel/lsp/lsp.go:1-13,191-228`); a custom next-edit model is the **sharpest break** from principle 2 *"the harness wins; borrow the intelligence"* (`docs/PRINCIPLES.md:13-15`).

The plan resolves the tension by separating two paths the roadmap already names (`docs/ROADMAP-EXTERNAL-INFRA.md:100-105`):

1. **Low-bar (cheapest-first):** a *self-hosted / custom OpenAI-compatible endpoint* plugs into `model.Provider` with **zero contract change** — the base-URL swap already exists (`internal/provider/openai.go:30-51`), so "a different / faster / self-hosted model" needs **no new module, no new contract, no editor**. This is "borrow the intelligence from a different shelf," not "re-encode it."
2. **High-bar (the thesis break):** a **bespoke trained Fusion/NES-class model** *and* a **net-new editor / LSP-server surface that lives OUTSIDE the core and calls the loop**. This is the identity change the §0 gate exists for.

Everything is a **new leaf package**, an **additive optional interface** (à la `model.Streamer`, `internal/model/stream.go:20-64`), or a **net-new out-of-tree surface** that imports the core but is never imported by it — so `Provider.Complete` / `backend.CodingBackend.Run` (I1) and `go.mod` (I6) are untouched, and the default `nilcore` binary is byte-identical with the feature absent.

---

## §0 The gate

An `EXT-03-T##` task is **not** an eligible task under `CLAUDE.md` §5 until the relevant gate below is cleared and **recorded** in the PR that promotes it into `docs/TASKS.md` (itself a serialized contract change). The gate is **split** so the low-bar path can ship without committing to the high-bar one.

### §0.A — Low-bar gate (self-hosted / custom OpenAI-compatible endpoint)

*This is a narrow gate, not the full thesis decision.* All must hold and be recorded:

1. **Thesis-narrow decision.** A human owner records that pointing NilCore's existing `openai`/`openrouter` provider at an **operator-configured, self-hosted, OpenAI-compatible base URL** (a local vLLM/TGI/Ollama-OpenAI endpoint, or a private inference gateway) is in-scope. This is a *configuration* decision, not an identity change — it adds **no new code path the model can reach**, only an operator-typed endpoint string.
2. **Invariants survive (I1, I3, I6).** The endpoint plugs into `model.Provider` (`internal/model/model.go:82-87`) through the *existing* `provider.NewOpenAI`/`NewOpenRouter` adapter (`internal/provider/openai.go:29-51`) — **no contract change** (I1). The endpoint key resolves by name through the SecretStore `cred` resolver, never to the model (I3). **No new module** (I6) — it is the same stdlib `net/http` adapter on a different `baseURL`.
3. **Verifier still governs (I2).** A faster/cheaper model changes *who writes the diff*, never *who blesses it*: `verify.Verifier.Check` still decides "done"; the integrator still never lands (`internal/integrate` re-verify + the one gated `policy.GateAction{PromoteToBase}`, `internal/policy/gateaction.go`).
4. **Default-off, byte-identical.** Absent an operator-set base URL, behavior is **exactly today's** — the default model selection is unchanged; the new config field is `omitempty` and zero-valued.

> **Why low-bar clears cheaply:** items 1–4 are satisfiable today by *configuration alone* (the base-URL swap is shipped). The only *code* the low-bar path adds (EXT-03-T01/T02) is a tiny **explicit base-URL config field** so the swap is operator-discoverable and validated, instead of relying on the implicit OpenRouter alias. It crosses no thesis line by itself.

### §0.B — High-bar gate (custom-trained model + editor / LSP-server surface)

*This is the full §0 thesis gate of `docs/ROADMAP-EXTERNAL-INFRA.md:11-21`.* All must hold and be recorded:

1. **Recorded thesis decision (not delegable).** A human owner explicitly decides NilCore's identity may expand from "tiny self-hosted harness, no editor surface, no custom model" toward **(a)** owning/operating a low-latency inference endpoint for a bespoke Fusion/NES model **and/or (b)** shipping an editor product (an LSP-server mode + an extension distribution channel). This is the irreversible, outward-facing decision the whole design reserves for a human (`docs/ROADMAP-EXTERNAL-INFRA.md:15`).
2. **Invariants extended, not bypassed (I1 load-bearing).** The editor surface drives the loop through `backend.CodingBackend.Run` for *agentic* edits and through a **new optional `model.Completer` interface** (à la `Streamer`) for *inline* completion — **neither widens `Provider.Complete` nor `CodingBackend`** (`docs/ROADMAP-EXTERNAL-INFRA.md:96-97,104-105`). The custom model is **config + endpoint, never a module** (I6).
3. **Verifier governs every apply (I2).** Inline completion is *suggestion-only* until accepted; an **accepted next-edit that the editor applies still routes a real file change through the same worktree-confined file tools + the verifier** — no inline apply ships work the verifier didn't see. The agentic "apply this edit set" path is the existing gated land.
4. **Security review of the out-of-core surface.** The LSP-server / extension is a **new untrusted-input boundary** (editor buffers, workspace files, an open socket). A review confirms: it is `O_NOFOLLOW`/worktree-confined for any write; buffer contents are `guard.Wrap`'d data not instructions (I7, `internal/guard/guard.go:18`); the endpoint credential is SecretStore-scoped (I3); the surface imports the core but the core never imports it (dependency direction, `docs/ARCHITECTURE.md:457-484`).
5. **Default-off, opt-in, reversible to remove.** The default `nilcore` binary remains byte-identical; the LSP-server is a *separate build target / subcommand* that links nothing into the default path; a bespoke model is selected only by an explicit `provider:model@endpoint` config.

If §0.B cannot be met, the high-bar tasks (EXT-03-T05…T12) stay on this roadmap, unbuilt — **the low-bar path (T01…T04) may still proceed under §0.A alone.**

---

## §1 As-is: the seams this reuses (sourced)

The single most important fact: **the low-bar path is ~95% already in the tree.** The base-URL swap, the optional-interface precedent, the LSP transport, and the gated land all ship today.

| Seam (sourced) | What EXT-03 reuses |
|---|---|
| `internal/provider/openai.go:29-51` | `NewOpenAI(key, modelID)` and `NewOpenRouter(key, modelID)` build one `OpenAI{key, model, baseURL, http}` struct; OpenRouter is **literally the same adapter with a different `baseURL`** (line 50). Any OpenAI-compatible self-hosted endpoint is a third base-URL. |
| `internal/model/model.go:82-87` | `Provider{Complete(ctx,…)(Response,error); Model() string}` — the frozen seam (I1). A self-hosted endpoint satisfies it unchanged. |
| `internal/model/stream.go:20-64` | `Streamer` — the **template for an optional, additive capability**: "A Provider MAY also implement Streamer; the loop type-asserts for it and falls back to Complete… purely ADDITIVE (invariant I1)." `model.Completer` (inline completion) follows this exact pattern. |
| `internal/model/model.go:14-58` | `Message`/`Block`/`ImageSource`/`ImageBlock` — **multimodal already paved** (`docs/ROADMAP-EXTERNAL-INFRA.md:102`): an editor can pass a screenshot/diagram through the same `[]Block` seam with no contract change (the image shape is additive, `model.go:19-26`). |
| `internal/codeintel/lsp/lsp.go:37-54,114-228,280-382` | A complete **JSON-RPC 2.0 + Content-Length framing** implementation (read/write/`call`/`notify`/`Initialize`/`Spawn`). Today it is a *client* (`Spawn` launches `gopls`). The wire machinery (`writeMessage`/`readResponse`/`readContentLength`) is **reusable verbatim by an LSP-server surface** living in a sibling package — the server answers requests instead of sending them, over the same framing. |
| `internal/onboard/onboard.go:53,229` | `Config` struct, strict-decoded (`DisallowUnknownFields`, line 229). An additive `omitempty` field for the endpoint keeps every existing config byte-compatible. |
| `cmd/nilcore/main.go:1402` | `buildBackend(name, prov, cred, …)` already constructs a backend from a named provider — the agentic "apply edit set" path the editor calls into. |
| `internal/policy/gateaction.go` (`PromoteToBase`, nil-approver-denies) | The one gated land. An accepted edit set that targets base routes here; nil approver default-denies. |
| `internal/eventlog/eventlog.go:86,200` | `Log.Append` / `Verify` — every completion request, accept, and apply appends a metadata-only event; the chain stays verifiable (I5). |
| `internal/guard/guard.go:18` | `Wrap(source, content)` — editor buffer / workspace file contents enter the loop as **wrapped data** (I7). |
| `internal/termui` | text rendering for the optional CLI-side status, no Charm in default. |

---

## §2 Architecture

The organizing principle: **three strictly-separated layers, lowest first.** Each is independently shippable; each higher layer depends only on shipped seams below it.

```
  ┌─────────────────────────────────────────────────────────────────────────────────────────┐
  │ LAYER 3 — OUT-OF-CORE editor surface (high-bar §0.B)                                       │
  │   cmd/nilcore-lsp/ (separate build target)  +  examples/editor-ext/ (distribution)        │
  │     speaks LSP-SERVER over internal/lsprpc framing → calls the loop, never embedded in it  │
  │       • inline completion  → model.Completer (optional iface)  [suggestion-only]           │
  │       • next-edit apply    → file tools + verify.Verifier      [verifier governs I2]       │
  │       • agentic edit set   → backend.CodingBackend.Run         [frozen I1, gated land]     │
  └───────────────────────────────────────────┬───────────────────────────────────────────────┘
                                              │ imports core; core NEVER imports it (dep direction)
  ┌───────────────────────────────────────────┴───────────────────────────────────────────────┐
  │ LAYER 2 — additive optional capability (high-bar §0.B, but contract-free)                  │
  │   internal/model/completer.go : type Completer interface { Complete-for-FIM/NES }          │
  │     • OPTIONAL — loop/surface type-asserts, falls back to Provider.Complete (à la Streamer)│
  │     • a self-hosted FIM/edit endpoint implements it; Provider.Complete UNCHANGED (I1)      │
  └───────────────────────────────────────────┬───────────────────────────────────────────────┘
                                              │
  ┌───────────────────────────────────────────┴───────────────────────────────────────────────┐
  │ LAYER 1 — the zero-contract provider base-URL path (low-bar §0.A) — ~95% SHIPPED            │
  │   internal/provider/openai.go : NewOpenAICompatible(key, modelID, baseURL)  (thin wrapper) │
  │     + onboard.Config.Endpoint (operator-typed, omitempty, SecretStore key by name)         │
  │       → ANY self-hosted OpenAI-compatible model:endpoint → model.Provider, ZERO contract Δ │
  └─────────────────────────────────────────────────────────────────────────────────────────┘
```

### §2.1 Layer 1 — the zero-contract provider base-URL path (low-bar)

`internal/provider/openai.go` already proves the only thing that matters: `baseURL` is a field (`openai.go:22-27`), and `NewOpenRouter` is `NewOpenAI` with a different one (`openai.go:50`). EXT-03-T01 adds **one thin constructor** `NewOpenAICompatible(key, modelID, baseURL)` (the explicit, validated form of the implicit OpenRouter swap) and EXT-03-T02 adds **one optional `onboard.Config.Endpoint` field** so an operator can type `provider=openai-compatible, model=my-fusion-7b, endpoint=http://127.0.0.1:8000/v1`. The key resolves by name through the existing `cred` resolver (I3). **No contract change, no module, no editor.** This *is* "borrow the intelligence" — a different/faster/self-hosted model on the existing shelf.

### §2.2 Layer 2 — `model.Completer`, an additive optional interface (high-bar, contract-free)

Inline completion (FIM / fill-in-the-middle) and next-edit prediction are a **different interaction shape** than chat `Complete` — they want a prefix/suffix and a tight latency budget, not a full message thread. The roadmap fixes the rule: "new capabilities are optional interfaces à la `model.Streamer`, never a contract change" (`docs/ROADMAP-EXTERNAL-INFRA.md:105`). EXT-03-T05 adds `internal/model/completer.go`:

```go
// Completer is the OPTIONAL low-latency inline-completion counterpart to
// Provider.Complete. A Provider MAY also implement Completer; the editor surface
// type-asserts for it and falls back to a Complete-shaped prompt when absent.
// Implementing Completer never changes Provider — it is purely ADDITIVE (I1).
type Completer interface {
    // Suggest returns inline edit suggestions for a cursor position given the
    // surrounding buffer (prefix/suffix) and recent edits. It honors ctx and a
    // tight deadline; an empty result is valid (no suggestion). It is
    // SUGGESTION-ONLY — it never writes a file (the verifier governs apply, I2).
    Suggest(ctx context.Context, req CompletionRequest) (CompletionResult, error)
}
```

The endpoint that implements it is the **same self-hosted OpenAI-compatible endpoint from Layer 1** (a FIM-capable model), or a bespoke trained model behind the same `provider:model@endpoint` config. `Provider.Complete` is untouched; a provider that does not implement `Completer` falls back exactly as the loop falls back from `Streamer` to `Complete`.

### §2.3 Layer 3 — the net-new LSP-server / editor surface, OUTSIDE the core (high-bar)

The roadmap is explicit: "an LSP-server mode or extension is net-new and lives **outside the core**; it would call the loop, not embed in it (preserving the dependency direction)" (`docs/ROADMAP-EXTERNAL-INFRA.md:103`). Two pieces:

- **`internal/lsprpc`** (EXT-03-T06, new leaf) — the **server** half of the JSON-RPC framing. The transport machinery already exists for the *client* (`internal/codeintel/lsp/lsp.go:280-382`: `writeMessage`/`readResponse`/`readContentLength`). The server leaf reuses the **identical Content-Length framing** but dispatches inbound requests to handlers instead of correlating outbound responses. It is a pure protocol leaf — no loop, no model — so it has zero risk to the core.
- **`cmd/nilcore-lsp`** (EXT-03-T09, separate build target) — the actual server binary. It wires `internal/lsprpc` to: (a) `textDocument/completion` / a custom `nilcore/inlineEdit` → `model.Completer` (suggestion-only); (b) `nilcore/applyEdit` → the worktree-confined file tools + `verify.Verifier` (I2 governs every apply); (c) `nilcore/agentEdit` → `backend.CodingBackend.Run` (I1, the gated land for anything touching base). It **imports the core; the core never imports it** — `cmd/nilcore-lsp` is a sibling of `cmd/nilcore`, dispatched by no existing arm.
- **`examples/editor-ext/`** (EXT-03-T10, docs/sample) — a minimal VS Code / Neovim client config that points at `nilcore-lsp`. Distribution (a marketplace listing) is **explicitly out of scope** and named as an EXT-07 dependency.

### §2.4 Multimodal is already paved

An editor passing a screenshot (a failing-render, a design mock) needs no new seam: `model.Block` carries an image additively (`internal/model/model.go:19-58`), and the OpenAI adapter already translates it to an `image_url` content part (`internal/provider/openai.go:224-231`). EXT-03-T11 only documents the editor→`ImageBlock` path; no contract change.

### §2.5 Why this honors principle 2

Principle 2 says *"keep the harness small, sharp, and yours, and let the model supply the fluency"* (`docs/PRINCIPLES.md:13-15`). The low-bar path is the *purest* expression of it — swap the model behind an unchanged seam. The high-bar path is the one true tension: a *bespoke trained model* re-encodes intelligence into NilCore's own infra. The plan quarantines that tension to the **endpoint config + an optional interface + an out-of-core surface**, so even a bespoke model is "borrowed through a config string," never welded into the loop.

---

## §3 The task DAG

**Namespace `EXT-03-T01 … EXT-03-T12`.** One task = one branch (`task/EXT-03-T0x`) = one PR. Owns sets are pairwise disjoint (package dir = unit of ownership). The **low-bar block (T01…T04) is independently mergeable under §0.A**; the **high-bar block (T05…T12) is blocked behind §0.B** and additionally depends on the low-bar block for the endpoint plumbing.

| ID | Title | Gate | Depends on | Owns | Note |
|---|---|---|---|---|---|
| EXT-03-T01 | `NewOpenAICompatible` constructor + base-URL validation | §0.A | — | `internal/provider/openai_compatible.go`, `internal/provider/openai_compatible_test.go` | new file in `provider`; **does not edit `openai.go`** |
| EXT-03-T02 | `onboard.Config.Endpoint` field + Validate clause | §0.A | EXT-03-T01 | `internal/onboard/onboard.go`, `onboard_test.go` | **contract (config schema)** — serialized |
| EXT-03-T03 | provider resolver wiring (`provider:model@endpoint`) | §0.A | EXT-03-T02 | `internal/provider/resolve.go` (or the resolve file), `resolve_test.go` | additive resolver arm; sole owner for its duration |
| EXT-03-T04 | low-bar docs + CHANGELOG (self-hosted endpoint) | §0.A | EXT-03-T03 | `docs/PROVIDERS.md` (or README provider section), `CHANGELOG.md` | **contract (docs)** — serialized; low-bar ships here |
| EXT-03-T05 | `model.Completer` optional interface | §0.B | — | `internal/model/completer.go`, `completer_test.go` | additive optional iface, à la `Streamer` |
| EXT-03-T06 | `internal/lsprpc` server-framing leaf | §0.B | — | `internal/lsprpc/` | new leaf; reuses lsp framing pattern |
| EXT-03-T07 | OpenAI-compatible FIM `Completer` impl | §0.B | EXT-03-T05, EXT-03-T03 | `internal/provider/openai_fim.go`, `openai_fim_test.go` | new file; impl on existing `OpenAI` struct |
| EXT-03-T08 | inline-edit session core (suggest/accept/apply, verifier-governed) | §0.B | EXT-03-T05, EXT-03-T06 | `internal/inlineedit/` | new leaf; apply → file tools + `verify` |
| EXT-03-T09 | `cmd/nilcore-lsp` server binary | §0.B | EXT-03-T06, EXT-03-T07, EXT-03-T08 | `cmd/nilcore-lsp/` | **separate build target**; never in `cmd/nilcore` |
| EXT-03-T10 | editor-extension sample client | §0.B | EXT-03-T09 | `examples/editor-ext/` | sample only; distribution = EXT-07 |
| EXT-03-T11 | editor→`ImageBlock` multimodal doc + glue | §0.B | EXT-03-T09 | `internal/inlineedit/image.go`, `*_test.go` | reuses paved `ImageBlock`; additive |
| EXT-03-T12 | high-bar docs + ARCHITECTURE + CHANGELOG promotion | §0.B | EXT-03-T09, EXT-03-T10, EXT-03-T11 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs)** — serialized last |

> **Owns-disjointness note.** `internal/provider` is touched by T01 (new file `openai_compatible.go`), T03 (new file `resolve.go`), and T07 (new file `openai_fim.go`) — each a **distinct new file**, none editing `openai.go`. Because `CLAUDE.md` §5.3 keys ownership by *file set*, these are disjoint and could run concurrently **if** §0.A and §0.B were both cleared; the DAG below sequences T07 after T03 only for the endpoint dependency, not for collision. `onboard.go` (T02) and the contract docs (T04, T12) are the serialized surfaces.

---

## §4 Per-task specs

### EXT-03-T01 — `NewOpenAICompatible` constructor + base-URL validation  · §0.A
- **Goal:** make the implicit base-URL swap **explicit, validated, and operator-discoverable** without touching `openai.go`. One thin constructor that builds the existing `OpenAI` struct with an arbitrary `baseURL`.
- **Depends on:** — (reuses the shipped `OpenAI` struct, `internal/provider/openai.go:22-51`).
- **Owns:** `internal/provider/openai_compatible.go`, `internal/provider/openai_compatible_test.go`.
- **Acceptance:** `NewOpenAICompatible(key, modelID, baseURL string) (*OpenAI, error)` returns an `OpenAI` whose `baseURL` is the operator's endpoint (the same struct `NewOpenRouter` returns); `validateBaseURL` rejects empty, non-`http(s)`, and a URL with embedded credentials/query (must be a clean `scheme://host[:port]/v1` form) with a closed error set; an empty `modelID` is an **error** here (unlike OpenRouter's Fusion fallback — a self-hosted endpoint has no default alias); the constructor **adds no field** to `OpenAI` and **does not edit `openai.go`** (it lives in a sibling file in the same package); the key is held in the struct exactly as `NewOpenAI` holds it (per-request header, never disk — I3, `openai.go:151`).
- **Verify:** `make verify`; `go test ./internal/provider/...`: a constructed provider's `Model()` returns `modelID`; a request (against an `httptest.Server` standing in for the self-hosted endpoint) POSTs to `<baseURL>/chat/completions` and round-trips a `model.Response`; `validateBaseURL` rejects each bad form; empty `modelID` errors; existing `openai`/`openrouter` tests stay green (no edit to `openai.go`).
- **Notes:** stdlib-only (`net/http` + `net/url`). This is the entire "custom model, cheap path" — no module, no contract, no editor.

### EXT-03-T02 — `onboard.Config.Endpoint` field + Validate clause  · §0.A · contract (config schema)
- **Goal:** one optional, strict-decode-compatible config field so an operator can name a self-hosted endpoint.
- **Depends on:** EXT-03-T01. **Owns:** `internal/onboard/onboard.go`, `internal/onboard/onboard_test.go`.
- **Acceptance:** `Config` gains `Endpoint string \`json:"endpoint,omitempty"\`` (the OpenAI-compatible base URL; empty ⇒ today's behavior); `Validate()` gains a clause — if `Endpoint != ""` it must (a) parse via the same `validateBaseURL` (reuse, not re-implement) and (b) require the provider be `openai`/`openai-compatible` (a self-hosted endpoint is meaningless for the Anthropic adapter), erroring loudly otherwise; **every existing config parses unchanged** under `DisallowUnknownFields` (`onboard.go:229`) because the field is zero-valued and `omitempty`; a config with `endpoint` round-trips parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: old config (no `endpoint`) parses; `endpoint` round-trips; `Validate` rejects a bad URL and an `endpoint` set against the `anthropic` provider; default-zero is byte-compatible.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface), treated as a contract surface (mirrors SWARM SW-T08). `onboard → provider` (for `validateBaseURL`) is downward, no cycle.

### EXT-03-T03 — provider resolver wiring (`provider:model@endpoint`)  · §0.A
- **Goal:** route the resolver so that when `Endpoint` is set (or a `model` spec carries an `@endpoint` suffix) the resolver builds via `NewOpenAICompatible`; otherwise behavior is **exactly today's**.
- **Depends on:** EXT-03-T02. **Owns:** the provider resolve arm (`internal/provider/resolve.go` or the existing resolve file — sole owner for the task's duration), `resolve_test.go`.
- **Acceptance:** `ResolveWith` (the existing `spec, cred` resolver) gains one arm: a spec of the form `openai-compatible:<model>` **with** an endpoint (from config or an `@`-suffix) ⇒ `NewOpenAICompatible(cred(name), model, endpoint)`; the key still resolves **by name** through the `cred` getter (I3 — the resolver never sees raw key material in the spec); an absent endpoint for a non-`openai-compatible` spec is the unchanged path (byte-identical); a malformed `@endpoint` is a loud error (fail-closed, never a silent fallback to the public API).
- **Verify:** `make verify`; `go test ./internal/provider/...`: `openai-compatible:m@http://h/v1` resolves to a provider hitting `h`; key fetched by name (a fake `cred` records the lookup, never the value in the spec); no-endpoint path identical to today; malformed endpoint errors; existing resolve tests green.
- **Notes:** additive arm only; the default resolution path is untouched (default-off proof, §8).

### EXT-03-T04 — low-bar docs + CHANGELOG (self-hosted endpoint)  · §0.A · contract (docs)
- **Goal:** document the self-hosted / custom OpenAI-compatible endpoint as a first-class, **default-off** operator capability — the low-bar path ships complete here.
- **Depends on:** EXT-03-T03. **Owns:** `docs/PROVIDERS.md` (or the README provider section), `CHANGELOG.md`.
- **Acceptance:** a "Self-hosted / custom model endpoint" section: the `provider=openai-compatible, model=…, endpoint=…` config, the SecretStore key-by-name note (I3), the **default-off / byte-identical** guarantee (absent `endpoint` ⇒ unchanged), and the explicit statement that this is "borrow the intelligence from a self-hosted shelf — no bespoke training, no editor, no new module"; one `CHANGELOG.md` `## [Unreleased]` line per merged low-bar task.
- **Verify:** `make verify` (docs don't break the build); markdown lint; a manual check that the config example validates against EXT-03-T02's `Validate`.
- **Notes:** **serialized — contract docs.** After this merges, the **low-bar path is DONE** and is independently useful even if §0.B never clears.

---

### EXT-03-T05 — `model.Completer` optional interface  · §0.B
- **Goal:** the additive, contract-free seam for inline completion / next-edit prediction — the `Streamer` analogue.
- **Depends on:** — (reuses `model.Provider`/`Message`/`Block`). **Owns:** `internal/model/completer.go`, `internal/model/completer_test.go`.
- **Acceptance:** `Completer interface { Suggest(ctx, CompletionRequest) (CompletionResult, error) }` with a doc-comment modeled verbatim on `Streamer` (`stream.go:20-58`): **optional** (callers type-assert and fall back to `Complete`), **honors ctx + a tight deadline**, **suggestion-only (never writes a file)**, **same trust posture** (the buffer is data); `CompletionRequest{Prefix, Suffix, Path, Language, RecentEdits []Edit}` (all plain data, no instructions); `CompletionResult{Suggestions []Suggestion}` where `Suggestion{Text, Range}`; a compile-time `var _ Completer` example in the test; **`Provider` is not edited** (I1 — the file adds only the new interface + value types).
- **Verify:** `make verify`; `go test ./internal/model/...`: a fake `Completer` is type-assertable; a `Provider` that does **not** implement `Completer` still satisfies `Provider` (fallback path compiles); value types marshal as plain data; `Provider.Complete` signature unchanged (a golden interface-shape test).
- **Notes:** stdlib-only. This is the I1 keystone for the editor surface — inline completion never touches `Provider.Complete` or `CodingBackend`.

### EXT-03-T06 — `internal/lsprpc` server-framing leaf  · §0.B
- **Goal:** the **server** half of LSP JSON-RPC — reuse the shipped Content-Length framing pattern (`internal/codeintel/lsp/lsp.go:280-382`) to *answer* requests rather than *send* them, as a pure protocol leaf with no model/loop dependency.
- **Depends on:** — (mirrors the framing in `internal/codeintel/lsp`, but is a distinct leaf to keep the client/server concerns separate). **Owns:** `internal/lsprpc/` (`server.go`, `frame.go`, `*_test.go`, `deps_test.go`).
- **Acceptance:** `Server{Handle(method string, h HandlerFunc)}` + `Serve(ctx, rw io.ReadWriteCloser) error`; `frame.go` reuses the **exact** Content-Length read/write logic (`Content-Length: N\r\n\r\n` + N bytes); a request dispatches to its registered handler, an unknown method returns a JSON-RPC method-not-found error (fail-closed, never a panic); notifications (no id) are dispatched without a response; the leaf imports **only stdlib** (`bufio`/`encoding/json`/`io`/`context`/`sync`) — **no model, no loop, no orchestrator**; `deps_test.go` asserts no import of `agent`/`super`/`backend`/`model`.
- **Verify:** `make verify`; `go test -race ./internal/lsprpc/...`: round-trip a request through an in-memory `net.Pipe`, assert the framed response; unknown method ⇒ method-not-found; a notification gets no reply; malformed frame ⇒ clean error not panic; `deps_test` asserts the pure-protocol import set.
- **Notes:** pure leaf, zero core risk. Distinct from `internal/codeintel/lsp` (client) by responsibility; could later share a `framing` sub-package, but v1 keeps them separate to avoid touching the shipped client.

### EXT-03-T07 — OpenAI-compatible FIM `Completer` impl  · §0.B
- **Goal:** implement `model.Completer` on the existing `OpenAI` struct for a FIM-capable self-hosted endpoint, so the **same Layer-1 endpoint** serves both chat and inline completion.
- **Depends on:** EXT-03-T05, EXT-03-T03. **Owns:** `internal/provider/openai_fim.go`, `internal/provider/openai_fim_test.go`.
- **Acceptance:** `func (o *OpenAI) Suggest(ctx, model.CompletionRequest) (model.CompletionResult, error)` POSTs to the endpoint's completion route (OpenAI `/completions` FIM with `prompt`/`suffix`, or the operator endpoint's documented FIM shape) and maps the reply to `CompletionResult`; honors ctx + a caller-supplied deadline (it never blocks longer than the request ctx); the key rides the same per-request header (I3); the buffer prefix/suffix are sent as **data** (no system-prompt injection from buffer content, I7); **`openai.go` is not edited** (impl in a sibling file); a provider built by `NewOpenAI` (no FIM endpoint) is free to return a clean "not supported" error so the surface falls back to a `Complete`-shaped prompt.
- **Verify:** `make verify`; `go test ./internal/provider/...`: against an `httptest.Server` FIM stub, `Suggest` round-trips prefix/suffix → suggestions; ctx-cancel mid-request returns promptly with the ctx error; the key is in the header not the body; existing tests green.
- **Notes:** stdlib-only. The same struct now satisfies both `Provider` and (optionally) `Completer` — the type-assert in the surface picks the inline path when available.

### EXT-03-T08 — inline-edit session core (suggest / accept / apply, verifier-governed)  · §0.B
- **Goal:** the headless core of an inline-edit session: hold buffer state, call `model.Completer` for suggestions, and on **accept** route the resulting file change through the **worktree-confined file tools + `verify.Verifier`** — so no inline apply ships work the verifier didn't see (I2).
- **Depends on:** EXT-03-T05, EXT-03-T06. **Owns:** `internal/inlineedit/` (`session.go`, `apply.go`, `*_test.go`, `deps_test.go`).
- **Acceptance:** `Session{Suggest(ctx, pos) ([]Suggestion, error); Accept(ctx, id) (ApplyResult, error)}`; `Suggest` is **read-only** (type-asserts the provider for `Completer`, falls back to a `Complete`-shaped prompt; buffer is `guard.Wrap`'d data, I7); `Accept` writes the accepted edit **only** through the existing worktree-confined file tools (`O_NOFOLLOW`, atomic temp+rename — never an arbitrary path, I4-boundary discipline) and **then runs `verify.Verifier.Check`**; `ApplyResult{Verified bool, Output string}` reports the verifier verdict — an apply whose verify is red is reported red (the editor may revert), never silently kept; an `Accept` targeting **base** is out of scope for inline (that is the agentic gated land, EXT-03-T09's `agentEdit`); every `Suggest`/`Accept`/apply appends a **metadata-only** event (I5); `deps_test.go` asserts no orchestrator import.
- **Verify:** `make verify`; `go test -race ./internal/inlineedit/...` (hermetic, fake `Completer` + temp worktree + canned `verify.Verifier`): `Suggest` writes nothing; `Accept` writes through the confined file tool then verifies; a red verify ⇒ `Verified:false`; an injection phrase in the buffer never becomes an instruction (it is `guard.Wrap`'d); events are metadata-only and the chain verifies.
- **Notes:** this is the I2 keystone for inline — *suggestion is free, apply is verified*. The session is headless (no LSP, no editor) so it is fully unit-testable; EXT-03-T09 wires it to the wire.

### EXT-03-T09 — `cmd/nilcore-lsp` server binary  · §0.B
- **Goal:** the out-of-core editor surface — a **separate build target** that serves LSP, wiring `internal/lsprpc` to inline completion (Layer 2), inline apply (verifier-governed, T08), and agentic edits (the loop, gated land). It **imports the core; the core never imports it.**
- **Depends on:** EXT-03-T06, EXT-03-T07, EXT-03-T08. **Owns:** `cmd/nilcore-lsp/` (`main.go`, `handlers.go`, `*_test.go`).
- **Acceptance:** `nilcore-lsp` is its **own `package main`** under `cmd/nilcore-lsp/` — **not** a subcommand of `cmd/nilcore` and reached by no existing dispatch arm (`main.go:87-107`); it registers handlers on `lsprpc.Server`: `textDocument/completion` + a custom `nilcore/inlineEdit` → `inlineedit.Session.Suggest` (suggestion-only); `nilcore/applyEdit` → `inlineedit.Session.Accept` (file tools + `verify`, I2); `nilcore/agentEdit` → `backend.CodingBackend.Run` via the existing `buildBackend` (`main.go:1402`, I1) for multi-file agentic edits, with any land routed through the **one** gated `policy.GateAction{PromoteToBase}` (nil approver default-denies); the endpoint/model come from `onboard.Config` (Layer 1, T02/T03); the server holds **no broad authority** — it operates on a single operator-opened worktree, confined writes only (I3/I4); it links **zero Charm** and adds nothing to the default `nilcore` binary.
- **Verify:** `make verify`; `go test ./cmd/nilcore-lsp/...` (hermetic, fake provider + fake sandbox + in-memory pipe): an `inlineEdit` request returns suggestions and writes nothing; an `applyEdit` writes-then-verifies and reports the verdict; an `agentEdit` targeting base is denied with a nil approver; an **import-graph test** asserting `cmd/nilcore` does **not** import `cmd/nilcore-lsp` and **no `internal/` core package imports it** (dependency direction, §6); a build test that `nilcore-lsp` compiles `CGO_ENABLED=0`.
- **Notes:** the surface is **net-new and outside the core** exactly as the roadmap requires (`docs/ROADMAP-EXTERNAL-INFRA.md:103`). It is a second binary, not a feature of the first — so the default-off / byte-identical proof (§8) is structural, not asserted.

### EXT-03-T10 — editor-extension sample client  · §0.B
- **Goal:** a minimal, *sample* editor client (VS Code `settings.json` + a Neovim LSP config) pointing at `nilcore-lsp`, so an operator can drive the surface — **distribution is explicitly out of scope** (an EXT-07 marketplace dependency).
- **Depends on:** EXT-03-T09. **Owns:** `examples/editor-ext/` (sample config + a README).
- **Acceptance:** a documented VS Code + Neovim config that launches `nilcore-lsp` over stdio and binds Tab/accept keys to the `nilcore/inlineEdit`/`nilcore/applyEdit` requests; a clear statement that this is a **reference client, not a published extension** — packaging/marketplace distribution is `EXT-07` (remote registry) and gated separately; no code in `internal/` or `cmd/nilcore` changes.
- **Verify:** `make verify` (no Go impact); a manual smoke note that the sample config connects to a locally-run `nilcore-lsp`.
- **Notes:** keeps the editor *product* ambition honest — NilCore ships the *server*, not a marketplace presence. The latter is a deliberately-deferred EXT-07 cross-dependency.

### EXT-03-T11 — editor→`ImageBlock` multimodal doc + glue  · §0.B
- **Goal:** let an editor pass a screenshot/diagram through the **already-paved** image seam — additive glue + documentation, no contract change.
- **Depends on:** EXT-03-T09. **Owns:** `internal/inlineedit/image.go`, `internal/inlineedit/image_test.go`.
- **Acceptance:** a helper that turns an editor-supplied image (base64 + media type) into a `model.ImageBlock` (`model.go:56-58`) attached to the agentic-edit request's `[]Block`; **no new field, no contract change** (the image shape is already additive, `model.go:19-26`; the OpenAI adapter already handles it, `openai.go:224-231`); a non-vision endpoint degrades cleanly (the block is simply not understood — no crash).
- **Verify:** `make verify`; `go test ./internal/inlineedit/...`: an image helper produces a well-formed `ImageBlock`; the block marshals byte-identically to the shipped image path; a text-only request is unchanged.
- **Notes:** the roadmap's "multimodal already paved" line (`docs/ROADMAP-EXTERNAL-INFRA.md:102`) made real — the editor reuses U1-T01's image blocks with zero seam work.

### EXT-03-T12 — high-bar docs + ARCHITECTURE + CHANGELOG promotion  · §0.B · contract (docs), serialized last
- **Goal:** promote the high-bar surface into the canonical docs once §0.B is cleared and the surface ships.
- **Depends on:** EXT-03-T09, EXT-03-T10, EXT-03-T11. **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md`.
- **Acceptance:** `docs/TASKS.md` gains the EXT-03 DAG rows + specs (noting the low-bar block reuses shipped provider seams); `docs/ARCHITECTURE.md` gains an "In-editor surface (EXT-03, out-of-core)" subsection — the three-layer model, the **dependency-direction rule restated** (the surface imports the core; the core never imports it), `model.Completer` as an optional interface (I1), inline-apply-is-verifier-governed (I2), the endpoint-is-config-not-a-module rule (I6), plus the four new leaf/binary rows in the layer-map with import sets; `docs/ROADMAP-EXTERNAL-INFRA.md` updates the EXT-03 entry to mark the low-bar path **shipped** and the high-bar path **shipped-behind-§0.B**; `CLAUDE.md` gains one repository-map line for `cmd/nilcore-lsp` (no invariant text changes — the invariants are unchanged, which is the point); `CHANGELOG.md` one `## [Unreleased]` line per merged high-bar task; `README.md` an "In-editor surface" section with the default-off note and the honest caveat that distribution is EXT-07.
- **Verify:** `make verify` (docs don't break the build); markdown lint; a manual check that the layer-map import sets match `go list -deps` of each new leaf/binary.
- **Notes:** **serialized — contract files.** Lands last. Per-task CHANGELOG lines are appended at each task's own merge; T12 reconciles trivial append conflicts on rebase.

---

## §5 Parallel wave map & critical path

A fleet executes in ordered **waves**; every task in a wave has all deps merged and a pairwise-disjoint Owns set. The **low-bar block (Waves L1–L3) gates on §0.A only** and can ship complete and useful before §0.B is ever decided. The **high-bar block (Waves H1–H4) gates on §0.B** and depends on the low-bar endpoint plumbing.

```
══ LOW-BAR (gate §0.A) — independently shippable ════════════════════════════════════
WAVE L1  (1)   EXT-03-T01  internal/provider/openai_compatible.go            (—)
WAVE L2  (1)   EXT-03-T02  internal/onboard/onboard.go   ← SERIAL: config schema   (T01)
WAVE L3  (1)   EXT-03-T03  internal/provider/resolve.go                        (T02)
WAVE L4  (1)   EXT-03-T04  docs/PROVIDERS.md + CHANGELOG  ← SERIAL: docs        (T03)
                          └── LOW-BAR DONE: self-hosted custom model usable.

══ HIGH-BAR (gate §0.B) — blocked until the full thesis decision ════════════════════
WAVE H1  (2 concurrent — no-dep new leaves)
  ├── EXT-03-T05  internal/model/completer.go            (—)
  └── EXT-03-T06  internal/lsprpc/                        (—)

WAVE H2  (2 concurrent)
  ├── EXT-03-T07  internal/provider/openai_fim.go         (T05, T03)
  └── EXT-03-T08  internal/inlineedit/                    (T05, T06)

WAVE H3  (1 — SERIAL pt: the out-of-core binary)
        EXT-03-T09  cmd/nilcore-lsp/                      (T06, T07, T08)

WAVE H4  (2 concurrent)
  ├── EXT-03-T10  examples/editor-ext/                    (T09)
  └── EXT-03-T11  internal/inlineedit/image.go            (T09)

WAVE H5  (1 — SERIAL pt: docs contract)
        EXT-03-T12  docs/* + ARCHITECTURE + CLAUDE + README + CHANGELOG   (T09,T10,T11)
```

**Peak concurrency = 2** (Waves H1, H2, H4 — a small surface by design).

**Critical path (longest dependency chain) — 9 sequential merges:**
```
EXT-03-T01 → EXT-03-T02 → EXT-03-T03 → EXT-03-T07 → EXT-03-T09 → EXT-03-T12
   (low-bar T01→T04 is a 4-merge sub-chain that branches off and completes independently)
   high-bar chain: T05/T06 → T08 → T09 → {T10,T11} → T12
```
The dominant chain is `T01 → T03 → T07 → T08 → T09 → T12` (the endpoint plumbing feeding the FIM impl feeding the session feeding the binary feeding docs).

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/onboard/onboard.go` — EXT-03-T02 only (config schema).
2. `docs/PROVIDERS.md` / low-bar `CHANGELOG` — EXT-03-T04 only.
3. `cmd/nilcore-lsp/` opening — EXT-03-T09 only (the out-of-core binary).
4. `docs/*` / `ARCHITECTURE.md` / `CLAUDE.md` / `README.md` prose — EXT-03-T12 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; `cmd/nilcore-lsp` imports the core but no core package imports it (T09's import-graph test); `internal/inlineedit` imports `model`/`lsprpc`-free apply path + `verify`/`guard` but never the orchestrator (T08's `deps_test`).

---

## §6 Per-invariant ledger

The seven invariants hold **by reuse and additivity**, not by new mechanism. The whole point is that this column is empty of *changes* — EXT-03 adds a surface and an endpoint, not a way to ship work the verifier did not bless or a credential the model can reach.

| Invariant | How EXT-03 preserves / extends it |
|---|---|
| **I1** frozen contract (**load-bearing**) | `Provider.Complete` (`model.go:82-87`) and `backend.CodingBackend.Run` are **untouched**. Inline completion is `model.Completer` — an **optional, additive** interface the surface type-asserts and falls back from, exactly like `Streamer` (`stream.go:20-64`); a provider that lacks it still satisfies `Provider`. Agentic edits route through the existing `buildBackend`/`Run`. The custom model rides `provider:model@endpoint` config — **never a `Task`/`Result`/interface change.** |
| **I2** verifier sole authority | A faster/custom model changes *who drafts*, never *who blesses*. Inline `Suggest` is **suggestion-only** (writes nothing); inline `Accept` writes through confined file tools **then runs `verify.Verifier.Check`** and reports the verdict (red apply = reported red, EXT-03-T08); agentic edits land only through `integrate` re-verify + the one gated `PromoteToBase`. No inline apply ships work the verifier didn't see. |
| **I3** no ambient authority | The endpoint key resolves **by name** through the existing `cred`/SecretStore resolver (EXT-03-T03) — the spec carries `provider:model@endpoint`, never key material; the key rides a per-request header (`openai.go:151`), never disk/log/prompt/model. The LSP server holds **no broad authority** — one operator-opened worktree, confined writes only. |
| **I4** sandboxed execution | The editor surface performs **scoped file I/O only** through the worktree-confined file tools (`O_NOFOLLOW`, atomic temp+rename) — never arbitrary execution; an agentic edit's commands run in the existing sandbox via `buildBackend` (`main.go:1402`). The inline path executes nothing — it suggests text. |
| **I5** append-only audit | Every completion request, accept, and apply appends a **metadata-only** event via `eventlog.Log.Append` (`eventlog.go:86`); the chain stays `Verify`-able (`eventlog.go:200`); no completion content or key is logged. |
| **I6** zero-dep core (**load-bearing**) | The custom model is **config + endpoint, never a module** (`docs/ROADMAP-EXTERNAL-INFRA.md:105`): the self-hosted endpoint reuses the shipped stdlib `net/http` OpenAI adapter on a different `baseURL`. `internal/lsprpc` reuses the shipped Content-Length framing — **stdlib only.** `cmd/nilcore-lsp` compiles `CGO_ENABLED=0`. **No `go.mod` change** (see §7). |
| **I7** untrusted-as-data | Editor buffer contents, workspace files, and any fetched/IDE-supplied content enter the loop `guard.Wrap`'d (`guard.go:18`) — a buffer comment that says "ignore your instructions" is data, never control. The completion request's prefix/suffix are sent as data, never spliced into a system prompt. |
| **Never-land / thesis** | The editor never auto-lands: an `agentEdit` touching base routes through the **one** gated `policy.GateAction{PromoteToBase}` (nil approver default-denies). The dependency direction is preserved structurally — `cmd/nilcore-lsp` imports the core; **no core package imports it** (T09 import-graph test). The bespoke-model tension (principle 2) is quarantined to an endpoint string + an optional interface, never welded into the loop. |

**The single line under all of them:** `I3` — *no ambient authority.* The editor surface adds an open socket and an endpoint, both allowed only because the socket operates on one confined worktree and the endpoint credential is SecretStore-scoped and never handed to the model — and only after the relevant §0 gate clears.

---

## §7 Module justifications

**No new module is required for any task.** Default to stdlib + the existing seams (`docs/ROADMAP-EXTERNAL-INFRA.md:18`).

| Capability | Stdlib / existing seam used | Why no module |
|---|---|---|
| Self-hosted OpenAI-compatible endpoint | `net/http` + `net/url` (the shipped `OpenAI` adapter, `openai.go`) | The endpoint is a different `baseURL` on the **existing** adapter — the cheap path is literally "a config string" (`docs/ROADMAP-EXTERNAL-INFRA.md:101`). |
| `model.Completer` optional interface | stdlib `context` only | An interface declaration + value types — no dependency. |
| LSP-server JSON-RPC framing | `bufio`/`encoding/json`/`io` (the framing already hand-rolled in `internal/codeintel/lsp/lsp.go:280-382`) | NilCore already speaks LSP framing in stdlib; the server half reuses the same pattern. **No LSP SDK module** — consistent with the codebase hand-rolling PBKDF2 to stay stdlib (`internal/secrets/file.go`). |
| FIM completion call | `net/http` (the shipped adapter) | Same wire shape, a different route. |
| Inline-edit session + apply | the shipped worktree-confined file tools + `internal/verify` + `internal/guard` | Reuse, not re-implement. |
| Editor image input | `model.ImageBlock` (shipped, `model.go:56-58`) | Multimodal already paved. |
| `cmd/nilcore-lsp` binary | stdlib `os`/`io` + the core packages | A second `package main`, no module. |

**`CGO_ENABLED=0` survives** (`.github/workflows/release.yml`): nothing here links C; the LSP server and the endpoint adapter are pure Go. **`go.mod` is UNTOUCHED** by every task.

---

## §8 Default-off byte-identical proof

The default `nilcore` binary is **byte-identical** with EXT-03 absent — proven structurally, not asserted:

1. **The editor surface is a separate binary.** `cmd/nilcore-lsp` is its own `package main`; the default `nilcore` dispatch (`main.go:87-107` — `chat`/`serve`/`report`/…) reaches **neither** a swarm-style new arm nor the LSP server. There is **no `case "lsp"`** in `cmd/nilcore` — the surface is reached only by building/running the second binary. (EXT-03-T09's import-graph test asserts `cmd/nilcore` does not import `cmd/nilcore-lsp`.)
2. **The low-bar endpoint is opt-in and zero-valued.** `onboard.Config.Endpoint` is `omitempty` and defaults to `""`; the resolver's new arm (T03) is reached **only** when an operator sets `provider=openai-compatible` + an endpoint. Absent that, `ResolveWith` takes the **unchanged** path — byte-for-byte today's resolution. Every existing config parses identically under `DisallowUnknownFields` (T02 acceptance).
3. **`model.Completer` is additive and unreferenced by the default path.** It is a new interface in `internal/model`; the native loop and the default backends never type-assert for it (only `cmd/nilcore-lsp` does). Merely declaring it changes no behavior — a `Provider` that doesn't implement it is unaffected.
4. **No new `init()` with global side effects.** Every new leaf (`internal/lsprpc`, `internal/inlineedit`, the new provider files) is import-inert — a `deps_test`/`init`-free assertion (mirroring SWARM SW-T17) proves merely linking them (if a future arm did) cannot change behavior.
5. **No existing package imports a new leaf.** `internal/lsprpc`, `internal/inlineedit`, and `cmd/nilcore-lsp` are imported only by `cmd/nilcore-lsp` (and tests). The default binary links none of them — the same proof shape `swarm`/`pool` use (`docs/SWARM.md:458-463`).

The four-point structural argument (separate binary + opt-in zero-valued config + additive-unreferenced interface + import-inertness) means the default-off guarantee is **established by construction**, exactly as `-concurrency 1` and the `schedule`/`watch` cases establish it for swarm.

---

## §9 Risks & honest caveats

- **The bespoke-model thesis break (the real one).** A *self-hosted endpoint* (low-bar) is firmly inside principle 2 — borrow a different model. A *bespoke trained Fusion/NES model* (high-bar) is the genuine break: it re-encodes intelligence into NilCore's own infra (`docs/ROADMAP-EXTERNAL-INFRA.md:95`). The plan **does not hide this** — it quarantines the tension to an endpoint config + an optional interface, so even a bespoke model is "borrowed through a config string," but the **decision to train/host one is the §0.B gate** and is the biggest identity change on the roadmap. The plan deliberately makes the low-bar path *complete and useful on its own* so the high-bar break is never forced.
- **The editor surface is a product, not a feature.** Shipping `nilcore-lsp` + a sample client is the *server* side. A **published, distributed extension** (a marketplace listing, auto-update) is `EXT-07` (remote registry) and gated separately — EXT-03-T10 ships a *reference client*, never a marketplace presence. Over-promising "an editor product" is the trap; the plan ships the protocol surface and names the distribution gap.
- **Inline latency is the operator's endpoint problem.** "Low-latency" Tab completion depends on the *endpoint* the operator points at; NilCore's surface adds only the framing + the suggestion plumbing. The plan does not promise sub-100ms completion — it promises a correct, verifier-governed surface over whatever endpoint the operator self-hosts. A slow endpoint is a slow Tab, honestly.
- **Inline apply is verified, not instant-trust.** Cursor-style instant inline apply trades verification for speed; NilCore's `Accept` **runs the verifier before reporting an apply green** (EXT-03-T08). This is slower than a naive apply and is the deliberate I2 cost — the surface is "verified inline edit," not "blind inline edit." Stated in the docs so the brief does not over-promise frontier-editor latency.
- **The dependency-direction risk.** The standing temptation is for a future PR to let a core package import the editor surface "for convenience." T09's import-graph test and T12's ARCHITECTURE note pin the rule so the surface can never quietly invert into the core — the same posture SWARM uses to forbid an HTTP scoreboard endpoint (`docs/SWARM.md:750-751`).
- **An open socket is a new untrusted boundary.** The LSP server accepts editor traffic (buffers, file paths, requests). The §0.B.4 security review is non-optional: every path is `O_NOFOLLOW`/worktree-confined, every buffer is `guard.Wrap`'d (I7), the endpoint credential is SecretStore-scoped (I3). A served, multi-client, network-listening editor backend would be `EXT-02`/`EXT-05` — out of scope; v1 is a single-operator stdio server.
- **MCP-as-completion-source is out of scope.** v1 completion is the self-hosted endpoint + stdlib only. Pointing the surface at a hosted completion API as a *standing dependency* drifts toward `EXT-06`/`EXT-07`; named, not designed in.

*The whole point of the invariant ledger (§6) is that it is empty of changes: NilCore stays a small, verifying, sandboxed, contract-frozen core. EXT-03 adds an opt-in endpoint and an out-of-core surface — it does not add a custom model to the loop, a credential to the model, or an inline apply the verifier did not bless. Build the low-bar path under a narrow gate; reach for the high-bar path only when the thesis itself — an editor product, a bespoke model — is on the table, and only after §0.B clears.*
