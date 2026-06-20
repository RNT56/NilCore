# EXT-04 — Remote / managed vector index at org scale (gated execution plan)

> **STATUS: BLOCKED behind the §0 gate of `docs/ROADMAP-EXTERNAL-INFRA.md`.** This is a
> *ready-when-the-gate-clears* execution plan, not an eligible task set. No `EXT-04-T##` is an
> eligible task in the `CLAUDE.md` §5 work-selection sense until a recorded human thesis decision
> promotes EXT-04 into `docs/TASKS.md` (itself a serialized contract PR). The integrator never
> lands; the human gate is the only door to `main`. Until then, this file is design only.

**Read order:** `CLAUDE.md` → `docs/ROADMAP-EXTERNAL-INFRA.md` (§0 gate, §5 EXT-04) →
`docs/CODE-INTELLIGENCE.md` → `docs/SWARM.md` (depth template) → the seams cited below.

---

## Table of contents

- [§-1 Summary](#-1-summary)
- [§0 The gate — what must be true before any EXT-04 task is written](#0-the-gate)
- [§1 As-is: the seams this rides (sourced, cite file:line)](#1-as-is-the-seams-this-rides)
- [§2 Architecture (privacy-preserving remote index behind the Embedder/semantic seam)](#2-architecture)
- [§3 The task DAG (EXT-04-T01 … EXT-04-T12)](#3-the-task-dag)
- [§4 Per-task specs](#4-per-task-specs)
- [§5 Parallel wave map & critical path](#5-parallel-wave-map--critical-path)
- [§6 Per-invariant ledger](#6-per-invariant-ledger)
- [§7 Module justifications (the vector-DB client)](#7-module-justifications)
- [§8 Default-off byte-identical proof](#8-default-off-byte-identical-proof)
- [§9 Risks & honest caveats](#9-risks--honest-caveats)

---

## §-1 Summary

EXT-04 adds an **opt-in remote embeddings index** so retrieval reaches **across repos / org-wide**
instead of being confined to one worktree. It is built as a **second `semantic.Embedder`-adjacent
seam**: a `RemoteIndex` client that slots **behind the existing `semantic` provenance lens** in the
**fixed `retrieve.Retrieve` fusion order** — never adding a new provenance string, never reordering
the lenses, never changing the read-only `codeintel` tool's write/exec/egress posture. The design is
**privacy-preserving by construction (the Cursor model):** the remote service stores **only
embedding vectors plus an obfuscated path/line locator** — **never source code**; a hit returns a
locator, and the code behind it is **re-read locally** from the worktree before anything enters the
loop. Degradation is a strict ladder: **remote index → local pure-Go HNSW (`U2-T04`) → lexical** —
each rung is independently sufficient, and with the feature absent the default `nilcore` binary is
**byte-identical** (remote unconfigured ⇒ local HNSW; embed key unset ⇒ lexical). The remote client
is a **stdlib `net/http` client to the index's REST/JSON API — not a vendor SDK** — so `I6`
(`CGO_ENABLED=0`, justified module) holds; the index credential lives in `SecretStore` and is
injected as a per-request header only, **never to the model** (`I3`); retrieved hits stay
**untrusted data**, `guard.Wrap`'d before any prompt (`I7`). The whole item is **BLOCKED** behind the
§0 thesis gate.

---

## §0 The gate

EXT-04 may not begin until **all five §0 conditions** of `docs/ROADMAP-EXTERNAL-INFRA.md:13-21`
hold and are recorded in the promotion PR (the PR that adds these rows to `docs/TASKS.md` — a
serialized contract change). Restated against EXT-04 concretely:

1. **Recorded thesis decision (human-only).** A human owner explicitly decides NilCore's identity may
   expand from "local-first, single-worktree, pure-Go index" toward depending on a **standing remote
   index service** (self-hosted or managed) for cross-repo retrieval at scale. This decision is the
   gate; it is **not delegable** to the agent — it is exactly the outward-facing, standing-authority
   class the design reserves for a human (`docs/ROADMAP-EXTERNAL-INFRA.md:15`). The promotion PR
   names the owner and the chosen index (e.g. a self-hosted Qdrant/Weaviate/Milvus, or a managed
   Turbopuffer/Pinecone — the choice is fixed at gate time so §7's module budget is exact).
2. **Invariants extended, not bypassed.** EXT-04 must show concretely (this plan's §6) that it
   **extends** `I1`–`I7`. The load-bearing pair is `I6` (the new client is a justified, CGO-free
   module — §7) and `I3` (the index credential is scoped, `SecretStore`-held, never to the model).
   The bar is the **privacy-preserving** design: store **embeddings + obfuscated locator only**,
   re-read code locally; **source never ships to the index** (`docs/ROADMAP-EXTERNAL-INFRA.md:117`).
3. **The verifier still governs (`I2`).** EXT-04 touches retrieval, not "done" — but the proof
   obligation stands: nothing EXT-04 surfaces can ship work on a self-report. Retrieval is read-only
   input; `verify.Verifier.Check` remains the only authority on done, and the integrator never lands
   (`internal/integrate/integrate.go` never-land guarantee; the only base land is one gated
   `policy.GateAction{PromoteToBase}`, `internal/policy/gateaction.go:28-45`). A `deps_test` proves
   the new leaf imports no verifier/orchestrator package and cannot influence a verdict.
4. **Dependency budget justified (`I6`).** The vector-DB client is justified in **both** the PR and
   the CHANGELOG, **must not break `CGO_ENABLED=0`** (`.github/workflows/release.yml`), and must be a
   **stdlib HTTP client, not a vendor SDK** (§7) unless the gate explicitly accepts a named pure-Go,
   CGO-free, transitively-clean SDK. Default to stdlib + the existing seams, exactly as the embedder
   already does (`internal/embed/embed.go:11` "Stdlib only … net/http + encoding/json").
5. **Default-off, opt-in, reversible to remove.** The default `nilcore` binary stays **byte-identical**
   with the feature absent (§8). Remote retrieval is gated behind a config/env flag; nothing here
   becomes a hard requirement; removing the leaf restores the local-only behavior exactly.

If any condition cannot be met, EXT-04 stays on the roadmap, unbuilt.

---

## §1 As-is: the seams this rides

The single most important fact: **EXT-04 builds almost nothing new in the retrieval core — it adds a
client behind seams that already exist and already degrade.** It reuses and extends; it never
rebuilds the fusion pipeline.

| Seam (cite) | What EXT-04 reuses |
|---|---|
| `internal/codeintel/semantic/semantic.go:45-47` — the `Embedder` interface (`Embed(ctx, text) ([]float32, error)`) | The **provider-agnostic vectorizer seam**. EXT-04's local→remote transport is a sibling to it; the same `[]float32` flows local or remote. |
| `internal/codeintel/semantic/semantic.go:63-98` — `Index` + `Open(path, Embedder)`; nil Embedder ⇒ lexical | The **local pure-Go index that degrades to lexical** (`Search` branches on `emb != nil`, `semantic.go:181-205`). The remote index is the rung **above** it; absent ⇒ this local `Index` ⇒ (nil emb) lexical. |
| `internal/codeintel/semantic/hnsw.go:11,57,256` — `newHNSW`, cosine-distance graph `search` | The **`U2-T04` local pure-Go ANN** (the lower degrade rung). The remote client returns the same `[]Hit` shape (`semantic.go:51-54`) so it is a drop-in for the local graph's output. |
| `internal/codeintel/retrieve/retrieve.go:39-43` — `Retriever{Graph, Semantic, LSP}` | The **fusion struct**. EXT-04 makes `Semantic` resolve to a remote-or-local index via a new constructor; the `Retriever` shape and its consumers are untouched. |
| `internal/codeintel/retrieve/retrieve.go:21-27,47,82-101` — `Item.Provenance` (closed set) + `provRank` + the **fixed fusion order** (precise → semantic → lexical → graph-neighbor → repomap) | The **closed provenance vocabulary** and **fixed order**. EXT-04 slots **behind the existing `"semantic"` lens** — a remote hit is still `Provenance:"semantic"`. **No new provenance string, no reorder.** §2.4. |
| `internal/embed/embed.go:11,30-67` — `OpenAIEmbedder`, key as per-request header only, stdlib `net/http`+`json` | The **template for the remote client's HTTP discipline**: key in a header, never logged/persisted/prompted; `io.LimitReader`; bounded error tails (`embed.go:79,102-109`). The remote client mirrors this exactly. |
| `internal/tools/codeintel.go:23-43,111-124` — read-only `CodeintelTool`; `Semantic` opt-in via `NILCORE_EMBED_KEY`, else nil ⇒ lexical; "No writes, no execution, no network" | The **read-only construction (`I7`)** and the **opt-in/byte-identical wiring point**. EXT-04 adds one more opt-in branch here (remote) **without** widening the tool's write/exec posture; the tool still re-reads code locally and fences output as untrusted (`codeintel.go:221-238`). |
| `internal/tools/codeintel.go:189-219` — `openSemantic` (persistent cached local index under the user cache dir) | The **construction site** EXT-04 extends: when remote is configured it builds a `RemoteIndex`; else the existing local `openSemantic`; else lexical. |
| `internal/secrets/secrets.go:17-24` — `SecretStore` (`Get/Set/Delete/Name`, value never logged/prompted) ; `internal/secrets/external.go:9-18` — `ExternalStore` corporate-secret hook | The **credential boundary** for the index token (`I3`). The index key is `Get`-resolved by name; never argv, never disk-plaintext, never the model. |
| `internal/provider/provider.go:26-44` — `ResolveWith(spec, getenv)` env→SecretStore resolver | The **by-name credential resolution** pattern the remote client's key lookup mirrors. |
| `internal/integrate/integrate.go` never-land + `internal/policy/gateaction.go:28-45` `PromoteToBase` | The **`I2`/never-land guarantee** EXT-04 must leave intact (it touches retrieval only). |
| `docs/CODE-INTELLIGENCE.md:40-41,108` (L3 semantic, opt-in, byte-identical, no module rides in) ; `docs/ROADMAP-EXTERNAL-INFRA.md:109-124` (EXT-04 boundary) | The **doctrine**: local-first, opt-in, degrade-to-lexical, zero code egress — the line EXT-04 must not cross except via the gate. |

**The line `U2-T04` deliberately does not cross** (`docs/ROADMAP-EXTERNAL-INFRA.md:113`): *remote*.
`U2-T04` is local pure-Go ANN. EXT-04 is the remote rung above it — that "remote" word is the entire
reason this is gated and lives here, not in `docs/UPGRADE-PATH.md`.

---

## §2 Architecture

The organizing principle: **a remote vector index is just another implementation of "give me the
top-k symbol ids for this query vector," and it slots in at exactly one place — behind the
`retrieve.Retriever.Semantic` field, under the `"semantic"` provenance lens, in the unchanged fusion
order.** Everything else (the graph, repomap, lexical, LSP lenses; the Context Bundle; the read-only
tool) is untouched.

```
          codeintel tool (read-only)  |  cmd/nilcore live/build wiring
                       │
        config/env:  remote? ──yes──► RemoteIndex (net/http client)         ◄── EXT-04-T01..T04
                       │              │   .Search(query) → []semantic.Hit          (NEW leaf: internal/codeintel/remoteindex)
                       │ no           │   .Sync(localGraph) → push EMBEDDINGS+LOCATORS only (NEVER source)
                       ▼              ▼
        embed key set? ──yes──► semantic.Index (local HNSW, U2-T04)          ◄── REUSED unchanged
                       │              │   cosine over local vectors
                       │ no           ▼
                       └──────► lexical (semantic.go:269 searchLexical)       ◄── REUSED unchanged
                                      │
                                      ▼
        retrieve.Retriever{ Graph, Semantic: <whichever rung>, LSP }         ◄── FIXED order, FIXED provenance
          1a precise (LSP)  1b SEMANTIC (← remote OR local OR lexical)  2 graph-expand  3 repomap
                                      │   every hit id → re-read code LOCALLY from the worktree
                                      ▼
        Context Bundle (Items[Provenance ∈ closed set]) → guard.Wrap → loop  ◄── I7 untrusted-as-data
```

### 2.1 A remote-index client behind the Embedder/semantic seam

A **new leaf `internal/codeintel/remoteindex`** holds a `RemoteIndex` type that satisfies a tiny
**`SearchIndex`** interface — the search-shaped subset of what `retrieve` needs from
`*semantic.Index`:

```go
// internal/codeintel/remoteindex (EXT-04-T01)
type SearchIndex interface {
    Search(ctx context.Context, query string, k int) ([]semantic.Hit, error)
    Close() error
}
```

`*semantic.Index` already satisfies this (`Search`/`Close` exist, `semantic.go:101,181`), so the
**existing local index is the zero-config implementation of the same interface** — the remote one is a
sibling. `RemoteIndex` holds: the `Embedder` (to vectorize the *query* locally — the query text is
the agent's, not repo source, but we keep the discipline of "code never leaves" and vectorize
locally regardless), a stdlib `*http.Client`, a base URL, and the index credential (header-only, per
the `embed.OpenAIEmbedder` template, `embed.go:64-67`):

- `RemoteIndex.Search(query, k)`: embed the query locally → POST the **query vector** (not text) to
  the index's `/query` endpoint → receive `[{locator, score}, …]` → return `[]semantic.Hit` whose
  `ID` is the **de-obfuscated local path/symbol** (§2.2). Network failure ⇒ **return the wrapped
  error**; the caller degrades to the local rung (§2.3) — never a fatal, never a fabricated hit.
- `RemoteIndex.Sync(localGraph, files)`: the **indexing/push** path — embeds each symbol locally and
  uploads **only** `{vector, obfuscatedLocator}` (§2.2). This is the only write to the remote
  service and it is **opt-in** (an explicit `nilcore index sync` style action or a gated background
  step), never implicit in a retrieval.

`retrieve.Retriever.Semantic` is widened from the concrete `*semantic.Index` to the `SearchIndex`
interface (EXT-04-T05, additive — the existing local index still satisfies it, so all current callers
compile unchanged; this is a leaf-internal type relaxation, not a contract change).

### 2.2 The privacy-preserving embeddings-only design (the Cursor model)

**The bar (`docs/ROADMAP-EXTERNAL-INFRA.md:117`): store only embeddings + an obfuscated path/line;
re-read code locally; never ship source to the index.** Concretely:

- **What is pushed to the remote index, per symbol:** `(vector []float32, locator string)`. **That is
  all.** No source text, no doc comments, no symbol names, no file contents.
- **The locator is obfuscated.** A locator is `HMAC-SHA256(orgSecret, repoID + "\0" + relPath + "\0" +
  symbolName + "\0" + startLine)` truncated to a stable id, plus the org/repo id needed to scope a
  query. The HMAC key (`orgSecret`) lives in `SecretStore` (`I3`) and **never leaves the host**, so a
  breach of the remote index yields neither code nor reversible paths — only opaque ids and vectors.
  A **local, on-host sidecar map** (`locator → relPath:symbol:line`, stored in the existing local
  SQLite next to the HNSW cache, `codeintel.go:189-198`) lets a returned locator be resolved back to a
  worktree location **on the host that owns the org secret**.
- **Re-read code locally (the load-bearing step).** A remote hit returns a locator → resolve to
  `relPath:line` via the local sidecar → **open the file from the worktree and read the symbol body
  there** (the same `worktreefs`/`os.ReadFile` discipline the local path already uses,
  `codeintel.go:207`). The remote service is **never** the source of code — only of "which symbols are
  semantically near." This is exactly the Cursor model named in the prompt.
- **Embedding privacy posture (honest caveat, §9).** Vectors are derived from source and are not
  perfectly opaque (embedding-inversion research can recover *some* text from some embeddings). The
  design therefore (a) keeps locators HMAC-obfuscated so a vector cannot be tied to a path without the
  host secret, (b) documents the residual risk, and (c) makes the **whole feature opt-in** so an org
  that cannot accept embedding egress simply never enables it (the default binary never sends a vector
  anywhere — §8). For orgs that need *zero* derived-data egress, the self-hosted index option (gate
  choice, §0) keeps everything inside the org perimeter.

### 2.3 How local → remote → lexical degrade

A strict, independently-sufficient ladder, chosen at construction time and re-checked per call:

1. **Remote configured** (`NILCORE_REMOTE_INDEX_URL` + an index credential resolvable from
   `SecretStore`): build a `RemoteIndex`. Each `Search` that returns a transport error **falls
   through** to the local rung for that call (best-effort, mirroring how `r.LSP`/`r.Semantic` errors
   degrade silently in `retrieve.go:75,85` and how `openSemantic` returns nil on any failure,
   `codeintel.go:191-202`).
2. **Else embed key set** (`NILCORE_EMBED_KEY`, the existing trigger, `codeintel.go:119`): the local
   `semantic.Index` over the pure-Go HNSW (`U2-T04`) — unchanged.
3. **Else lexical**: nil Embedder ⇒ `semantic.go:269 searchLexical`, or `retrieve`'s lexical-over-node-
   names fallback (`retrieve.go:92-101`) — unchanged.

The ladder is **monotone**: each rung is a superset capability over the one below, and every rung
returns the same `[]semantic.Hit`/Context Bundle shape, so the consumer (the loop) cannot tell which
rung served a query except by the (unchanged) `"semantic"` vs `"lexical"` provenance tag.

### 2.4 Slotting into the fixed fusion order without changing the provenance vocabulary

`retrieve.Retrieve` (`retrieve.go:50-136`) runs lenses in a **fixed order** and tags each item with a
**closed `Provenance`** drawn from `{precise, semantic, lexical, graph-neighbor, repomap}`
(`retrieve.go:24,47`). EXT-04's discipline:

- A remote hit enters at **step 1b** (`retrieve.go:82-91`), the **existing `"semantic"` lens**, with
  `Provenance:"semantic"`. **No new provenance string is introduced** — a remote hit is
  indistinguishable in the vocabulary from a local-HNSW hit, because to the fusion layer it *is* the
  same kind of evidence ("a vector-similarity lead, to be confirmed by graph + lexical"). This keeps
  `provRank` (`retrieve.go:47`), the deterministic sort (`retrieve.go:123-131`), and every downstream
  consumer (renderers, the bundle) **byte-unchanged**.
- The **order is unchanged**: precise (LSP) still ranks first, semantic second, then lexical, graph-
  neighbor, repomap. A remote hit is still **graph-expanded** (step 2, `retrieve.go:104-113`) and
  **oriented by repomap** (step 3) exactly as a local semantic hit is — structure still dominates
  similarity (`docs/CODE-INTELLIGENCE.md:22` principle 1).
- Because the remote rung resolves *behind* the `Semantic` field, **`retrieve.go` itself needs only
  the one-line type relaxation** (`*semantic.Index` → `SearchIndex`, EXT-04-T05); the 1b block, the
  order, the provenance set, and the sort are **not edited**.

---

## §3 The task DAG

**Namespace `EXT-04-T01 … EXT-04-T12`.** One task = one branch (`task/EXT-04-T0x`) = one PR. Owns
sets are **pairwise disjoint** (package dir / single-file = unit of ownership). The privacy/transport
foundation (T01–T04) lands **before** the wiring tasks (T08–T10). Contract/serialized surfaces
(go.mod, config schema, docs) are each held by exactly one task.

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-04-T01 | `SearchIndex` interface + `RemoteIndex` skeleton + stdlib HTTP transport | — | `internal/codeintel/remoteindex/` (`remoteindex.go`, `transport.go`, `*_test.go`, `deps_test.go`) | **new leaf**; opens the package |
| EXT-04-T02 | Obfuscated locator (HMAC) + local sidecar map (locator↔relPath:symbol:line) | EXT-04-T01 | `internal/codeintel/remoteindex/` (`locator.go`, `sidecar.go`, `*_test.go`) | serial after T01 (same pkg) |
| EXT-04-T03 | `RemoteIndex.Search` (query-vector POST → `[]semantic.Hit` via sidecar) + per-call degrade | EXT-04-T02 | `internal/codeintel/remoteindex/` (`search.go`, `search_test.go`) | serial after T02 |
| EXT-04-T04 | `RemoteIndex.Sync` (push embeddings+locators ONLY; never source) | EXT-04-T03 | `internal/codeintel/remoteindex/` (`sync.go`, `sync_test.go`) | serial after T03 |
| EXT-04-T05 | Relax `retrieve.Retriever.Semantic` to the `SearchIndex` interface (additive) | EXT-04-T01 | `internal/codeintel/retrieve/retrieve.go` (1-field type relax + `*_test.go`) | **sole owner of retrieve.go** for its duration |
| EXT-04-T06 | Index-credential resolution by name (SecretStore header injection) | EXT-04-T01 | `internal/codeintel/remoteindex/` (`cred.go`, `cred_test.go`) | serial after T01 |
| EXT-04-T07 | `go.mod` justification (pure-Go vector-DB client OR confirm stdlib-only) | EXT-04-T01 (design fixed) | `go.mod`, `go.sum` | **contract (deps)** — serialized; may be a no-op if stdlib-only |
| EXT-04-T08 | `onboard.Config.RemoteIndex *RemoteIndexConfig` field + Validate clause | EXT-04-T01 | `internal/onboard/onboard.go`, `onboard_test.go` | **contract (config schema)** — serialized |
| EXT-04-T09 | Wire remote rung into the read-only `codeintel` tool (opt-in branch; re-read local) | EXT-04-T03, EXT-04-T05, EXT-04-T06, EXT-04-T08 | `internal/tools/codeintel.go` (additive branch + `*_test.go`) | **sole owner of codeintel.go** for its duration |
| EXT-04-T10 | `nilcore index sync` subcommand + dispatch wiring | EXT-04-T04, EXT-04-T06, EXT-04-T08 | `cmd/nilcore/index.go` (new), `cmd/nilcore/main.go` (one `case "index"` + usage) | **serialized cmd-wiring** (one new arm) |
| EXT-04-T11 | Privacy/degrade/byte-identical proof-test bundle | EXT-04-T09, EXT-04-T10 | `internal/codeintel/remoteindex/privacy_test.go`, `cmd/nilcore/index_test.go` | proof obligations (§6/§8) |
| EXT-04-T12 | Docs + CHANGELOG promotion | EXT-04-T11 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `docs/CODE-INTELLIGENCE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`, `README.md` | **contract (docs)** — serialized last |

> **Ownership note (mirrors SWARM.md's intra-package rule):** `internal/codeintel/remoteindex` is the
> **whole package** as the Owns unit for T01–T04 and T06 — so the work-selection rule correctly forbids
> two of them open at once on the same files; T01 opens the package and T02/T03/T04/T06 add sibling
> files serially. T05 (retrieve.go), T07 (go.mod), T08 (onboard.go), T09 (codeintel.go), T10 (main.go)
> are each held by exactly one task.

---

## §4 Per-task specs

#### EXT-04-T01 — `SearchIndex` interface + `RemoteIndex` skeleton + stdlib HTTP transport
- **Goal:** open the leaf with the search-shaped `SearchIndex` interface (which `*semantic.Index`
  already satisfies — the local index is the zero-config sibling), the `RemoteIndex` struct
  (embedder, `*http.Client`, base URL, cred resolver), and a hardened stdlib transport mirroring
  `internal/embed`.
- **Depends on:** — (reuses shipped `internal/codeintel/semantic`, `internal/embed`).
- **Owns:** `internal/codeintel/remoteindex/` (`remoteindex.go`, `transport.go`, `*_test.go`,
  `deps_test.go`).
- **Acceptance:** `SearchIndex interface { Search(ctx,query,k) ([]semantic.Hit,error); Close() error }`
  (compile-time `var _ SearchIndex = (*semantic.Index)(nil)` proves the local index satisfies it and
  the remote one is a drop-in); `RemoteIndex{Embedder semantic.Embedder, HTTP *http.Client, BaseURL
  string, key string}`; `New(cfg, embedder, getKey func() (string,error)) (*RemoteIndex, error)`;
  transport sets `content-type` + an `authorization` header **only** (key from `getKey`, never logged
  — mirror `embed.go:63-67`), uses `io.LimitReader` (`embed.go:79`) and bounded error tails
  (`embed.go:102-109`), honors `ctx`; **no key in any URL/query param, ever**; `deps_test.go` runs
  `go list -deps` and asserts **no** import of `internal/verify`, `internal/integrate`,
  `internal/agent`, `internal/super`, `internal/policy` (retrieval cannot influence a verdict — §6 I2).
- **Verify:** `make verify`; `go test -race ./internal/codeintel/remoteindex/...` with an `httptest`
  server: header set, no key in URL, ctx-cancel returns promptly, oversized body truncated; `var _`
  compile guard; `deps_test` green.
- **Notes:** **no vendor SDK** (§7) — `net/http`+`encoding/json` only at this layer; if the gate
  selected a pure-Go SDK it is confined to T07's go.mod justification and used behind this same
  interface so the leaf's import set is reviewable.

#### EXT-04-T02 — Obfuscated locator (HMAC) + local sidecar map
- **Goal:** the privacy primitive — a deterministic HMAC-obfuscated locator the remote index stores
  instead of any path/source, plus a host-local sidecar that maps a locator back to a worktree
  `relPath:symbol:line` so a hit can be **re-read locally**.
- **Depends on:** EXT-04-T01. **Owns:** `internal/codeintel/remoteindex/` (`locator.go`, `sidecar.go`,
  `*_test.go`).
- **Acceptance:** `Locator(orgSecret []byte, repoID, relPath, symbol string, startLine int) string` =
  `hex(HMAC-SHA256(orgSecret, repoID\0relPath\0symbol\0startLine))[:N]` (stdlib `crypto/hmac` +
  `crypto/sha256`); **pure, deterministic, irreversible without `orgSecret`**; the sidecar is a local
  SQLite table (reuse the `modernc.org/sqlite` already in tree, `semantic.go:28`, co-located under the
  user cache dir like `openSemantic`, `codeintel.go:189-198`) keyed `locator → (relPath,symbol,line)`;
  `Resolve(locator) (relPath,symbol,line, ok)`; a test asserts the locator embeds **no plaintext path/
  symbol** (locator string contains none of the inputs as substrings) and that two different secrets
  yield different locators for the same input.
- **Verify:** `make verify`; `go test -race`: determinism, irreversibility (no input substring leaks),
  sidecar round-trip, secret-sensitivity. Hermetic (`:memory:` sidecar).
- **Notes:** `orgSecret` is resolved from `SecretStore` at the wiring layer (T06/T09), never hard-coded;
  the sidecar never leaves the host (it is the inverse of the privacy boundary and must stay local).

#### EXT-04-T03 — `RemoteIndex.Search` + per-call degrade
- **Goal:** the read path — embed the query **locally**, POST the **query vector** (never text/source),
  receive `{locator,score}` rows, resolve each via the sidecar, return `[]semantic.Hit` whose `ID` is
  the **local** `relPath#symbol` (the shape `retrieve` already consumes, `retrieve.go:88`); a transport
  error returns wrapped (caller degrades).
- **Depends on:** EXT-04-T02. **Owns:** `internal/codeintel/remoteindex/` (`search.go`, `search_test.go`).
- **Acceptance:** `Search(ctx, query, k)` embeds via `RemoteIndex.Embedder.Embed` (local), POSTs
  `{vector, k, scope:repoID}` to `BaseURL/query`, decodes `{hits:[{locator,score}]}`, resolves each
  locator via the sidecar **dropping unresolved locators** (a locator with no local mapping ⇒ the
  symbol is not in this worktree ⇒ skip, never fabricate), returns `[]semantic.Hit` sorted/capped to
  `k` (reuse `semantic.Hit` ordering semantics, `semantic.go:51-54`); the request body **contains no
  source text and no plaintext path** (asserted); transport/non-2xx ⇒ wrapped error (no fabricated
  hits, no fatal).
- **Verify:** `make verify`; `go test -race` with `httptest`: body carries a vector + scope but **no
  source/path** (assert the marshaled request); unresolved locator dropped; 5xx ⇒ error; `k` cap; ctx
  cancel.
- **Notes:** this is where "re-read code locally" begins — `Search` returns *locations*, not bodies;
  the tool reads the body from disk (T09). The query vector leaving the host is the one derived-data
  egress, documented in §9.

#### EXT-04-T04 — `RemoteIndex.Sync` (push embeddings + locators ONLY)
- **Goal:** the write/index path — for each symbol in the local graph, embed locally and upload **only**
  `{vector, locator}`; **source never leaves**. Opt-in, explicit.
- **Depends on:** EXT-04-T03. **Owns:** `internal/codeintel/remoteindex/` (`sync.go`, `sync_test.go`).
- **Acceptance:** `Sync(ctx, symbols []SymbolDoc, orgSecret []byte, repoID string) (SyncStat, error)`
  where `SymbolDoc{relPath, symbol, startLine, text}` is built **host-side** from the worktree;
  per symbol: `vec = Embedder.Embed(text)` (local), `loc = Locator(...)`, **upsert the sidecar**
  (`relPath:symbol:line`), and POST **only** `{vector:vec, locator:loc, scope:repoID}` (a test asserts
  the upload body contains **neither `text` nor `relPath` nor `symbol`** — embeddings + opaque locator
  only); batched with bounded request size (`maxEmbedBytes` discipline, `codeintel.go:180`); content-
  hash skip for unchanged symbols (reuse the `semantic` cache idea, `semantic.go:108-152`) so re-sync
  is incremental; partial failure is best-effort + reported in `SyncStat`, never fatal.
- **Verify:** `make verify`; `go test -race` with `httptest`: **upload body contains no source/path/
  symbol** (the privacy keystone test); sidecar populated; incremental re-sync skips unchanged; partial
  failure tolerated.
- **Notes:** `Sync` is invoked only by the explicit `nilcore index sync` arm (T10) — **never** implicit
  in a retrieval, so a plain `nilcore run` never uploads anything.

#### EXT-04-T05 — Relax `retrieve.Retriever.Semantic` to `SearchIndex` (additive)
- **Goal:** widen one field type so `retrieve` can hold a local **or** remote index behind the
  unchanged `"semantic"` lens, in the unchanged fusion order, with the closed provenance vocabulary
  intact.
- **Depends on:** EXT-04-T01. **Owns:** `internal/codeintel/retrieve/retrieve.go` (1-field type
  relaxation + `*_test.go`). **Sole owner of retrieve.go** for its duration.
- **Acceptance:** `Retriever.Semantic` changes from `*semantic.Index` to `remoteindex.SearchIndex`
  (the local `*semantic.Index` still satisfies it ⇒ every existing caller compiles unchanged); the 1b
  block (`retrieve.go:82-91`), `provRank` (`retrieve.go:47`), the fixed order, and the deterministic
  sort (`retrieve.go:123-131`) are **byte-unchanged**; a test asserts a remote hit gets
  `Provenance:"semantic"` (not a new string) and is graph-expanded + repomap-oriented exactly like a
  local hit; **no new entry in the provenance set**.
- **Verify:** `make verify`; `go test -race ./internal/codeintel/retrieve/...`: existing retrieve tests
  green (local `*semantic.Index` still wired); a fake `SearchIndex` yields `"semantic"`-tagged,
  graph-expanded items; provenance set unchanged (a guard test enumerates the closed set).
- **Notes:** this is the **only** edit to the fusion core, and it is a type relaxation, not a logic
  change — the order and vocabulary are frozen by `I7`/the retrieval contract.

#### EXT-04-T06 — Index-credential resolution by name (SecretStore)
- **Goal:** resolve the index credential **and** the `orgSecret` by name from `SecretStore`, injected
  as a per-request header / HMAC key only — never argv, never disk-plaintext, never the model.
- **Depends on:** EXT-04-T01. **Owns:** `internal/codeintel/remoteindex/` (`cred.go`, `cred_test.go`).
- **Acceptance:** `ResolveCreds(store secrets.SecretStore, names CredNames) (indexKey string, orgSecret
  []byte, error)` calls `store.Get(name)` (mirror `provider.ResolveWith`, `provider.go:26-44`); a
  missing secret is a **clean refuse-to-enable** (remote rung off ⇒ degrade), never a panic; a test
  asserts the resolved values **never appear** in any log/event/error string (error references the
  **name** only, `secrets.go:18`); the key flows to the header (T01) and `orgSecret` to the HMAC (T02),
  nowhere else.
- **Verify:** `make verify`; `go test -race`: `Get` by name; missing ⇒ degrade signal; key/secret never
  in error/log output (a buffer-capture test greps the value and asserts absent).
- **Notes:** the `orgSecret` is the privacy linchpin — it must be host-only; `ExternalStore`
  (`external.go:9-18`) is the recommended backend for an org-wide secret (Vault/KMS), unchanged.

#### EXT-04-T07 — `go.mod` justification (vector-DB client) · contract (deps)
- **Goal:** if (and only if) the gate selected an index whose REST/JSON API needs more than stdlib,
  add **one** justified, **pure-Go, CGO-free** client module — else confirm stdlib-only and make this a
  documented no-op.
- **Depends on:** EXT-04-T01 (the transport design fixes whether a module is needed). **Owns:**
  `go.mod`, `go.sum`. **Contract — serialized.**
- **Acceptance:** **default outcome is stdlib-only** (the remote API is REST/JSON, handled by
  `net/http`+`encoding/json` — §7), so this task **adds nothing** and records "stdlib-only, no module"
  in the CHANGELOG; **if** a client module is unavoidable it (a) is named in the PR + CHANGELOG with a
  justification (`I6`, `docs/ROADMAP-EXTERNAL-INFRA.md:18,116`), (b) is **pure-Go** so
  `CGO_ENABLED=0` cross-compile across the release matrix stays green (`.github/workflows/release.yml`),
  (c) pulls in **no** transitive cgo/network-server deps (a `go mod graph` review), (d) is confined
  behind the T01 `SearchIndex` interface so it is removable.
- **Verify:** `make verify`; `CGO_ENABLED=0 GOOS=linux/darwin go build ./...` green; `go mod graph`
  review attached to the PR; if no module added, a CHANGELOG line stating so.
- **Notes:** **prefer stdlib HTTP to any vendor SDK** (the prompt's explicit bar, §7). Most managed
  vector indexes (Qdrant, Weaviate, Milvus, Turbopuffer, Pinecone) expose a plain REST/JSON API that a
  small stdlib client covers — exactly as `internal/embed` covers the OpenAI-compatible API stdlib-only.

#### EXT-04-T08 — `onboard.Config.RemoteIndex` field + Validate · contract (config schema)
- **Goal:** additively extend `onboard.Config` with one optional `RemoteIndex *RemoteIndexConfig`
  (`json:"remote_index,omitempty"`) + a Validate clause, v1-compatible.
- **Depends on:** EXT-04-T01. **Owns:** `internal/onboard/onboard.go`, `onboard_test.go`. **Contract —
  serialized.**
- **Acceptance:** `RemoteIndexConfig{BaseURL string, CredName string, OrgSecretName string, RepoID
  string}` (**no key material — names only**, mirroring how `pool.TierSpec` carries `provider:model`
  not keys, `SWARM.md` SW-T07); default-zero so every existing config parses unchanged under
  `DisallowUnknownFields`; `Validate()` gains a clause (BaseURL is a well-formed `https`/`http` URL,
  cred/org-secret names non-empty when `RemoteIndex` is set, loud error otherwise); old configs without
  `remote_index` parse; a config with `remote_index` round-trips parse/Save/Load.
- **Verify:** `make verify`; `go test ./internal/onboard/...`: round-trip; old config parses; Validate
  rejects a bad URL / empty names; **a test asserts the struct carries no key field**.
- **Notes:** **serialized** — `onboard.go` is the strict-decoded config schema (a stable interface),
  treated as a contract surface even though not on the frozen `§5` list (same posture as SWARM.md
  SW-T08). `onboard → remoteindex` is downward (no cycle).

#### EXT-04-T09 — Wire remote rung into the read-only `codeintel` tool (opt-in; re-read local)
- **Goal:** add **one** opt-in branch to the read-only tool: when remote is configured build a
  `RemoteIndex` for the `Semantic` field; **a remote hit's code is re-read from the worktree locally**;
  the tool's write/exec/egress posture is **unchanged** except for the one outbound call to the index
  (documented, opt-in).
- **Depends on:** EXT-04-T03, EXT-04-T05, EXT-04-T06, EXT-04-T08. **Owns:**
  `internal/tools/codeintel.go` (additive branch + `*_test.go`). **Sole owner of codeintel.go** for its
  duration.
- **Acceptance:** the existing `if key := os.Getenv("NILCORE_EMBED_KEY")` block (`codeintel.go:119-124`)
  gains a **preceding** opt-in branch: if `RemoteIndexConfig` is present **and** creds resolve (T06),
  set `r.Semantic = remoteindex.New(...)` (the remote rung); **else** the existing local `openSemantic`;
  **else** nil ⇒ lexical — the §2.3 ladder; the rendered bundle still re-reads/relativizes paths
  locally (`renderBundle`/`relOrSame`, `codeintel.go:226-254`) so **no host path leaks** and the body
  comes from disk; the tool's doc comment is updated to note the **one** opt-in outbound call (the
  "No network" guarantee becomes "no network unless remote index is explicitly configured"); output is
  still fenced as untrusted by the loop (`I7`, `codeintel.go:225`).
- **Verify:** `make verify`; `go test -race ./internal/tools/...`: remote-configured ⇒ `Semantic` is the
  remote rung; remote-unconfigured ⇒ **byte-identical** to today (local-or-lexical); a remote hit's
  rendered item carries a **worktree-relative** path (no host leak) and the code is read locally (a
  fake `RemoteIndex` returns a locator → the tool reads the real file); no write/exec surface added.
- **Notes:** the **only** behavioral change to the tool is the gated outbound call; with remote absent
  the tool is provably unchanged (§8).

#### EXT-04-T10 — `nilcore index sync` subcommand + dispatch wiring · serialized cmd-wiring
- **Goal:** the explicit operator front door to **push** embeddings+locators (the only write to the
  remote index), as a new dispatch arm — never implicit in `run`/`build`.
- **Depends on:** EXT-04-T04, EXT-04-T06, EXT-04-T08. **Owns:** `cmd/nilcore/index.go` (new),
  `cmd/nilcore/main.go` (one `case "index"` + a usage line). **Serialized cmd-wiring.**
- **Acceptance:** `registerIndexFlags` parses `--dir`, `--repo-id`, `--remote` (and reads
  `RemoteIndexConfig` from `--config`/env); `indexMain` builds the local graph over the worktree
  (reuse `sourceFilesUnder`/`BuildFile`, `codeintel.go:88-109`), constructs `SymbolDoc`s host-side,
  resolves creds (T06), calls `RemoteIndex.Sync` (T04), prints a `SyncStat` summary; unknown/missing
  remote config is FATAL at startup (fail-closed, never a silent no-op upload); add **one**
  `case "index"` + usage line; **no** other arm or shared helper edited; the default dispatch path
  (`nilcore`/`nilcore run`) reaches neither this arm nor any remote code (§8).
- **Verify:** `make verify`; `go test ./cmd/nilcore/...` (hermetic, `httptest` index): flag parse;
  missing remote config ⇒ FATAL; a sync run uploads **vectors+locators only** (assert request bodies);
  default dispatch unaffected; an import-graph test asserting no existing non-index package imports
  `internal/codeintel/remoteindex`.
- **Notes:** **serialized** — the only task editing `main.go`. Sync is opt-in and explicit so retrieval
  never silently writes to the index.

#### EXT-04-T11 — Privacy / degrade / byte-identical proof-test bundle
- **Goal:** make the §6/§8 obligations executable: no source ever leaves; the ladder degrades;
  default-off is byte-identical; creds never leak; retrieval cannot influence a verdict.
- **Depends on:** EXT-04-T09, EXT-04-T10. **Owns:**
  `internal/codeintel/remoteindex/privacy_test.go`, `cmd/nilcore/index_test.go`.
- **Acceptance:** **(privacy)** across `Sync` and `Search`, an `httptest` recorder captures **every**
  outbound body and asserts **no source text, no plaintext relPath, no symbol name** ever appears —
  only vectors + opaque locators (the keystone); **(creds)** a buffer-capture asserts the index key /
  orgSecret never appear in any log/event/error; **(degrade)** remote 5xx ⇒ falls to local HNSW ⇒ (no
  embed key) lexical, each rung returning a non-empty bundle for a hitting query; **(byte-identical)**
  with remote unconfigured, `codeintel` output equals the pre-EXT-04 baseline (golden), and an
  import-graph test asserts no existing package imports the remote leaf; **(I2)** the `deps_test`
  (T01) is re-asserted at the bundle level — no verifier/integrator/orchestrator import.
- **Verify:** `make verify`; `go test -race ./internal/codeintel/remoteindex/... ./cmd/nilcore/...`:
  all five obligations green.
- **Notes:** these tests are the discharge of the §0 gate's "invariants survive, not bypassed" — they
  are the evidence the promotion PR cites.

#### EXT-04-T12 — Docs + CHANGELOG promotion · contract (docs), serialized last
- **Goal:** promote this plan into the canonical docs + ledger; record the thesis decision and the
  privacy posture.
- **Depends on:** EXT-04-T11. **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`,
  `docs/CODE-INTELLIGENCE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `CLAUDE.md`, `CHANGELOG.md`,
  `README.md`.
- **Acceptance:** `docs/TASKS.md` EXT-04 DAG rows + specs (noting the reuse of the shipped semantic/
  retrieve seams, not a rebuild); `docs/CODE-INTELLIGENCE.md` an L3 note that the semantic lens may be
  served by a remote rung **behind the same provenance**, opt-in, degrade-to-local-to-lexical, **zero
  source egress (embeddings+locators only)**; `docs/ARCHITECTURE.md` a layer-map row for
  `internal/codeintel/remoteindex` with its import set (`semantic`/`embed`/`secrets`/stdlib —
  **never** verify/integrate/agent/super/policy) + the restated read-only/degrade rule;
  `docs/ROADMAP-EXTERNAL-INFRA.md` EXT-04 marked promoted with the recorded thesis owner + the privacy
  design as the met bar; `CLAUDE.md` one repository-map line (invariants **unchanged** — the point);
  `CHANGELOG.md` one `## [Unreleased]` line per merged EXT-04-T0x; `README.md` the `nilcore index sync`
  usage + the opt-in/byte-identical note + the honest caveats (query-vector egress; embedding-inversion
  residual risk; self-host option for zero derived-data egress).
- **Verify:** `make verify` (docs don't break the build); markdown lint; manual review that the
  layer-map import set matches the actual `go list -deps` of the leaf.
- **Notes:** **serialized — contract files.** Lands last. Per-task CHANGELOG lines are appended at each
  task's own merge; T12 reconciles trivial append conflicts on rebase.

---

## §5 Parallel wave map & critical path

A fleet executes in ordered **waves**; every task in a wave has all deps merged to `main` and a
pairwise-disjoint Owns set. `internal/codeintel/remoteindex` is **one package = one Owns unit**, so
T01→T02→T03→T04 (and T06 after T01) form a serialized sub-chain — the only intra-package
serialization.

```
WAVE 1  (1 — opens the leaf; everything imports it)
  └── EXT-04-T01  internal/codeintel/remoteindex/ (interface + skeleton + transport)

WAVE 2  (4 concurrent — disjoint Owns over the wave-1 leaf)
  ├── EXT-04-T02  remoteindex/ (locator + sidecar)       (T01)  ← lane A, in-package serial head
  ├── EXT-04-T05  retrieve/retrieve.go (type relax)       (T01)  ← sole retrieve.go owner
  ├── EXT-04-T07  go.mod (justify / confirm stdlib)       (T01)  ← SERIAL pt: deps contract
  └── EXT-04-T08  onboard/onboard.go (config field)       (T01)  ← SERIAL pt: config-schema contract

WAVE 3  (2 concurrent)
  ├── EXT-04-T03  remoteindex/ (Search + degrade)         (T02)  ← lane A serial
  └── EXT-04-T06  remoteindex/ (cred resolution)          (T01)  ← waits for the lane-A head to land to avoid pkg collision

WAVE 4  (1 — lane A serial)
  └── EXT-04-T04  remoteindex/ (Sync push)                (T03)

WAVE 5  (1 — SERIAL pt: tool surface)
  └── EXT-04-T09  tools/codeintel.go (opt-in branch)      (T03,T05,T06,T08)  ← sole codeintel.go owner

WAVE 6  (1 — SERIAL pt: cmd-wiring)
  └── EXT-04-T10  cmd/nilcore/index.go + main.go          (T04,T06,T08)      ← sole main.go editor

WAVE 7  (1 — proofs)
  └── EXT-04-T11  privacy/degrade/byte-identical tests     (T09,T10)

WAVE 8  (1 — SERIAL pt: docs contract)
  └── EXT-04-T12  docs/* + CLAUDE.md + README + CHANGELOG  (T11)             ← sole docs editor
```

**Peak concurrency = 4 (wave 2).** **Critical path (longest dependency chain) — 8 sequential merges:**

```
EXT-04-T01 → EXT-04-T02 → EXT-04-T03 → EXT-04-T04 → EXT-04-T10 → EXT-04-T11 → EXT-04-T12
                                     └→ (T09 via T03/T05/T06/T08) → EXT-04-T11 → EXT-04-T12
```
(longest is `T01→T02→T03→T04→T10→T11→T12` = 7 edges / 8 merges; the `…→T09→T11` branch is shorter.)

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/codeintel/remoteindex` package dir — T01 opens; T02/T03/T04/T06 serialize as sibling
   files (package = unit of ownership; in-package split would create cycles, so the chain is correct).
2. `internal/codeintel/retrieve/retrieve.go` — T05 only.
3. `go.mod` / `go.sum` — T07 only (deps contract; likely a no-op).
4. `internal/onboard/onboard.go` — T08 only (config schema).
5. `internal/tools/codeintel.go` — T09 only (tool surface).
6. `cmd/nilcore/main.go` — T10 only (cmd-wiring).
7. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — T12 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; the remoteindex sub-chain is
strictly increasing IDs; `onboard → remoteindex`, `retrieve → remoteindex`, `tools → remoteindex` are
all downward (leaf imported, never importing back). **Foundation-before-wiring holds:** the tool/cmd
arms cannot compile until `Search`/`Sync` (T03/T04) and the type relax (T05) exist.

---

## §6 Per-invariant ledger

The seven invariants hold **by reuse and by construction**, not by new mechanism. EXT-04 is the
strictest test of `I6`, `I3`, and `I7`.

| Invariant | How EXT-04 preserves / extends it |
|---|---|
| **I1** frozen contract | `backend.CodingBackend.Run(ctx,Task)→(Result,error)` is **untouched**. EXT-04 lives entirely in the retrieval read-path leaves (`remoteindex`, a 1-field relax in `retrieve`, an opt-in branch in the `codeintel` tool). No `Task`/`Result`/interface change; `internal/channel/channel.go` untouched. |
| **I2** verifier sole authority | EXT-04 touches **retrieval input only** — it cannot ship work. `verify.Verifier.Check` remains the only "done" authority; the integrator never lands (`internal/integrate/integrate.go`); the only base land is one gated `policy.GateAction{PromoteToBase}` (`gateaction.go:28-45`). A `deps_test` (T01, re-asserted T11) proves the remote leaf imports **no** `verify`/`integrate`/`agent`/`super`/`policy` — a hit can never enter a verdict path. |
| **I3** no ambient authority / secrets never to the model | The index credential and `orgSecret` resolve **by name** from `SecretStore` (`secrets.go:17-24`; mirror `provider.ResolveWith`, `provider.go:26-44`), injected as a per-request **header** and an HMAC key only — **never** in argv, URL, disk-plaintext, logs, events, or a prompt, **never to the model** (mirror `embed.go:63-67`). T06 + T11 prove the values never appear in any output string. `ExternalStore` (`external.go`) is the recommended org-wide backend, unchanged. |
| **I4** sandboxed execution | EXT-04 adds **no** model-emitted execution. The `codeintel` tool stays read-only/host-side/parse-only (`codeintel.go:29-43`); the only new effect is one **opt-in outbound HTTPS call** to the index (data egress, not code execution). No shell, no sandbox surface. Where the swarm/CLI run delegated coding backends, those still run in-box — unchanged. |
| **I5** append-only audit | Any retrieval/sync event is metadata-only and **redacted** (the locator is opaque by design; the credential is never logged — `secrets.go:18`). No history mutation. The append-only log is untouched in shape. |
| **I6** zero-dep core / `CGO_ENABLED=0` | The remote client is **stdlib `net/http`+`encoding/json`** (the `internal/embed` template, `embed.go:11`) — **no vendor SDK** (§7). T07 keeps `go.mod` stdlib-only by default; if a client module is unavoidable it is justified in PR+CHANGELOG, **pure-Go**, transitively cgo-free, and removable behind the `SearchIndex` interface. `CGO_ENABLED=0` cross-compile stays green. The sidecar reuses the **already-sanctioned** `modernc.org/sqlite` (`semantic.go:28`) — no new DB module. |
| **I7** untrusted-as-data / retrieval read-only | A remote hit is a **lead, not a fact** — it enters at the `"semantic"` lens (provenance unchanged, `retrieve.go:24,47`), is graph/lexical-confirmed, and the **code is re-read locally** before anything enters the loop; the bundle is `guard.Wrap`'d as untrusted exactly as today (`codeintel.go:225`). The `codeintel` tool gains **no** write/exec surface. The remote service is never a source of code or instructions — only of vector-similarity locators. |
| **Privacy bar (the §0 met-condition)** | **Source never ships.** The index stores **embeddings + an HMAC-obfuscated locator only**; the local sidecar (host-only) resolves a locator back to a worktree location; code is re-read locally (the Cursor model, `docs/ROADMAP-EXTERNAL-INFRA.md:117`). T04/T11 prove no source/path/symbol leaves the host. |

---

## §7 Module justifications (the vector-DB client)

**The bar (prompt + `docs/ROADMAP-EXTERNAL-INFRA.md:18,116`): prefer a stdlib-HTTP client to a vendor
SDK; any module is justified in PR + CHANGELOG and must keep `CGO_ENABLED=0`.**

- **Default outcome: no module.** Every realistic gate-chosen index — self-hosted **Qdrant**,
  **Weaviate**, **Milvus**, or managed **Turbopuffer**, **Pinecone** — exposes a plain **REST/JSON
  API** (and most also gRPC, which we deliberately *do not* use to avoid the `google.golang.org/grpc`
  dependency tree). A small stdlib client over `net/http`+`encoding/json` covers query + upsert + scope
  — **exactly as `internal/embed` covers the OpenAI-compatible API with stdlib only** (`embed.go:11`,
  "net/http + encoding/json"). This is the strong default and makes T07 a no-op.
- **Why not the vendor SDK.** Vendor SDKs (a) drag transitive trees (gRPC, protobuf, telemetry,
  retry/auth helpers) that bloat the binary and risk a cgo or network-server transitive, violating the
  zero-dep-core spirit (`I6`); (b) bind NilCore to one vendor's surface, defeating the "swap the index
  at gate time" flexibility; (c) hide the credential flow we must keep header-only and auditable. The
  hand-rolled stdlib client keeps the entire wire shape and the credential path **inside one reviewable
  leaf** — the same reason the codebase hand-rolls PBKDF2 to stay stdlib (`internal/secrets/file.go`,
  cited in the gate `docs/ROADMAP-EXTERNAL-INFRA.md:18`).
- **If a module is truly unavoidable** (an index with no stable REST surface): T07 adds **one** named,
  **pure-Go**, CGO-free client with (a) a PR+CHANGELOG justification, (b) a `go mod graph` review
  proving no transitive cgo/network-server dep, (c) `CGO_ENABLED=0` cross-compile across the release
  matrix green, (d) confinement behind the T01 `SearchIndex` interface so removing the module restores
  stdlib behavior. The sidecar adds **no** module — it reuses the sanctioned `modernc.org/sqlite`.

---

## §8 Default-off byte-identical proof (absent ⇒ local HNSW ⇒ lexical)

The default `nilcore` binary is **byte-identical** with EXT-04 absent — proven, not asserted (T11):

1. **No existing package imports the remote leaf.** An import-graph test (T10/T11) asserts no package
   except the new `index` arm and the opt-in `codeintel` branch references `internal/codeintel/
   remoteindex`. The default dispatch path (`nilcore` / `nilcore run` / `nilcore build`) reaches
   neither.
2. **The opt-in branch is gated on config/env.** `codeintel.go` (T09) only builds a `RemoteIndex` when
   `RemoteIndexConfig` is present **and** creds resolve; absent that, the code path is the **existing**
   `NILCORE_EMBED_KEY` branch (`codeintel.go:119-124`) — local HNSW — or, with no embed key, lexical
   (`semantic.go:269`). A golden test asserts `codeintel` output with remote unconfigured **equals the
   pre-EXT-04 baseline**.
3. **The ladder is the existing degrade made longer.** remote → local HNSW (`U2-T04`, `hnsw.go`) →
   lexical is monotone; removing the top rung leaves the **shipped** behavior exactly
   (`docs/CODE-INTELLIGENCE.md:41` "with it off, NilCore falls back to the lexical lens and the default
   binary is byte-identical"). EXT-04 adds a rung **above** that, never alters the rungs below.
4. **No `go.mod` change by default** (§7) ⇒ the linked binary is unchanged ⇒ byte-identity is a build
   fact, not a behavior assertion.
5. **No global-side-effect `init()`** in the new leaf (asserted), so merely linking the package (were it
   linked) could not change behavior — the same posture SWARM.md proves for its leaves.

---

## §9 Risks & honest caveats

- **Query-vector egress (the one derived-data leak).** Retrieval POSTs the **query vector** (derived
  from the agent's query text, not repo source) to the index, and `Sync` POSTs **symbol vectors**
  (derived from source). Vectors are not source, but **embedding-inversion** research can recover *some*
  text from *some* embeddings. Mitigation: HMAC-obfuscated locators (a vector cannot be tied to a path
  without the host secret), the **opt-in** posture (an org that cannot accept any derived-data egress
  never enables it — §8), and the **self-hosted index** gate option (everything stays in the org
  perimeter). Documented in the README (T12) so the product brief does not over-promise "zero egress."
- **The remote index is a standing service with availability/SLA obligations.** It can be down, slow,
  or rate-limited. Mitigation: every `Search` degrades per-call to local HNSW (§2.3) — a remote outage
  **never** breaks retrieval, only narrows it to single-worktree scope. This is the whole reason the
  ladder is monotone.
- **Stale index / locator drift.** A re-synced repo can leave the remote index ahead of or behind the
  worktree; an unresolved locator (symbol moved/deleted) is **dropped, never fabricated** (T03). The
  sidecar is the source of truth for "does this locator exist locally?" Re-sync is incremental
  (content-hash skip, T04) to keep drift small, but the honest bound is **eventual consistency**, not
  real-time Merkle sync (the Cursor/Turbopuffer full design is a larger follow-on, named not built).
- **`orgSecret` is the privacy linchpin — its loss de-obfuscates locators.** It must be a host-only
  `SecretStore` value (Vault/KMS via `ExternalStore` recommended), never synced, never to the model
  (`I3`). A leaked `orgSecret` + a breached index would link vectors to paths — but still not to
  source. Documented as the one secret whose blast radius is the privacy boundary.
- **Cross-repo scope means cross-repo trust.** An org-wide index returns hits from repos the current
  task may not own; a returned locator that does not resolve in the **current** worktree is dropped
  (T03), so cross-repo hits surface only when the code is actually present locally — retrieval never
  invents a path into a repo the host cannot read. (True cross-repo *reading* would be a separate,
  larger authority decision, out of scope.)
- **The EXT-01/EXT-06 line.** EXT-04 is a **retrieval** capability, not a fleet or a secret broker. The
  moment the remote index's credential is **distributed to many hosts by a central broker**, that is
  `EXT-06`; the moment retrieval state is used to **coordinate remote workers**, that is `EXT-01`. Both
  are out of scope here; named as future dependencies, never designed in. A `deps_test` keeps the leaf
  free of any RPC/control-plane import.
- **MCP-as-index is out of scope.** Pointing retrieval at an operator-configured MCP index server would
  make a lead contingent on a standing external API beyond the one gated index — drift toward
  `EXT-07`. v1 is the single, gate-chosen index over a stdlib HTTP client only.

---

*EXT-04 stays true to the file it lives in: it is a boundary defended, not a backlog burned down.
Build `docs/UPGRADE-PATH.md`'s local `U2-T04` pure-Go ANN first — it makes NilCore better at being
NilCore. Reach for this remote rung only when org-scale cross-repo retrieval is genuinely on the table,
and when it is, build it the way the rest of the system earns trust: a scoped credential in the
SecretStore, source that never leaves the host, a verifier that retrieval can never touch, and a
default binary that is byte-identical with the whole thing absent. BLOCKED behind §0 until a human
owner records the decision. <3*
