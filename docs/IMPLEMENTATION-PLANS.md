# Implementation Plans — the deferred / design-heavy items

> **SHIPPED (merged to `main`).** All four deferred items — **D1, D2, D3, D4** — are now implemented, merged to `main`, and `make verify`-green. The end-to-end task specs below are retained as the historical record / design rationale; each item carries a one-line shipped note under its heading. The one honest caveat that survives: D1's live browser run is **CI-only** (no Chromium in the hermetic unit tests), and the driver fails **closed** when no browser is present.

This document fully plans the Tier-1/Tier-2 items that were **deliberately not rushed** during the Phase 9/10 implementation, because doing each one *right* is a real piece of engineering (a sandbox-image build, a vector-search algorithm, a multi-language parser), not glue. Each is specced end-to-end here — goal, design + UX decision, task breakdown in the `docs/TASKS.md` format, best practices, and an invariant ledger — so the next agent can pick one up cold.

> **Read order placement.** Below the canon (`CLAUDE.md` → `docs/PREREQUISITES.md` → `docs/ARCHITECTURE.md` → `docs/PERSONA.md` → `docs/TASKS.md` → `CHANGELOG.md`) and alongside `docs/UPGRADE-PATH.md`. Everything here presumes the seven invariants and the frozen `backend.CodingBackend` contract; it never restates the law.

**What already shipped (the reusable halves are done):**
- `internal/tools/browser.go` — the `browser_view` tool (Box-bound, fails closed). _Needs: a browser in the image + screenshot delivery._
- `internal/embed` — a provider-backed `semantic.Embedder`. _Needs: activation wiring + a cache + an ANN index._
- `internal/forge` — gated draft-PR creation. _Needs: the trigger→PR auto-path._
- `internal/codeintel/ast` — the Go-first parser with the documented tree-sitter seam. _Needs: a CGO-free multi-language backend._

**Namespace:** these use `D#-T##` ids (Deferred), distinct from canonical `P#-T##` and the `U#-T##` proposals. Promoting any into `docs/TASKS.md` is a serialized contract task (`docs/TASKS.md` is a contract file).

---

## D1 — Behavioral verification: the browser binary + screenshot→model delivery

> **SHIPPED.** The sandbox image carries a headless browser and the pure-Go `nilcore-browser` driver (`cmd/tools/nilcore-browser`); `browser_view` hands the model a screenshot as a `model.ImageBlock`, and the composite verifier (opt-in via `NILCORE_BROWSER_VERIFY`) folds a browser behavioral check into the verdict so the verifier stays the sole authority on "done" (I2). **Caveat:** the live browser run is CI-only (no Chromium in the hermetic unit tests); the driver fails closed without a browser.

**Source:** `internal/tools/browser.go` (the tool, with its `NOT DONE` block), `internal/verify/browser.go` (the verifier-side check), `internal/backend/native.go` (the tool-dispatch), `internal/model/model.go` (`ImageBlock`, shipped P9-T01), `images/sandbox/` (P0-T03), `internal/sandbox/sandbox.go` (container egress).

**End-to-end goal.** `browser_view` (and the `NILCORE_BROWSER_VERIFY` behavioral verifier) actually *see* a running app: a real headless browser navigates the local/preview URL inside the sandbox, captures the DOM/console **and a screenshot**, and the screenshot reaches a vision-capable model as a `model.ImageBlock` so the agent reasons over what rendered — closing the verifier loop on behavior, not just compilation.

**Two pieces remain (both real work):**

### D1-T01 — Headless-browser binary + `nilcore-browser` driver in the sandbox image
- **Goal:** the sandbox image carries a headless browser and a tiny driver that satisfies the contract `browser_view` already speaks (`{title, text, console, screenshot_b64}` JSON on stdout, exit non-zero on failure).
- **Depends on:** —  **Owns:** `images/sandbox/`, `cmd/tools/nilcore-browser/` (new, build-tagged)
- **Acceptance criteria:**
  - `images/sandbox/Dockerfile` installs a headless Chromium (pinned version, non-root) and a `nilcore-browser` driver on `$PATH`.
  - `nilcore-browser --url <u> --format json` navigates via the DevTools protocol (CDP over a pure-Go client or `chromedp`-style; **if a module is added it needs I6 justification** in the PR + CHANGELOG and must keep `CGO_ENABLED=0`), prints the JSON observation, and exits non-zero on navigation error / missing selector.
  - A documented image build/tag command; the driver is operator-trusted (it ships in the image), never model-emitted.
- **Verify:** a `browser-e2e` CI job (like the `sandbox-linux` job) runs `nilcore-browser` against a fixture static server and asserts the JSON shape + a non-zero exit on a 404. Hermetic unit tests cover the JSON encoder.
- **Notes:** the browser runs **only** on the container backend with egress to the target host (the namespace backend has no egress — `internal/sandbox/namespace_linux.go`); `browser_view` already gates on `*sandbox.Container` + the allowlist and fails closed when the driver is absent. _Best practice: pin the browser version; treat the rendered DOM/console as untrusted (the tool already `guard.Wrap`s it, I7)._

### D1-T02 — Screenshot → `model.ImageBlock` delivery through the loop
- **Goal:** a captured screenshot reaches the model as an image, so it can reason over the render — not just the text observation.
- **Depends on:** D1-T01  **Owns:** `internal/backend/native.go`, `internal/tools/`
- **Design decision (best UX, lowest blast radius):** the tool-dispatch (`internal/backend/native.go:567-582`) turns a tool result into a single **text** `tool_result` block. Rather than widen the `tools.Tool` contract to return blocks (ripples everywhere), have the loop, **after** a `browser_view` `tool_result`, append a follow-up **user** turn carrying `model.ImageBlock(mediaType, screenshot_b64)`. A user image turn works for **both** providers (Anthropic + OpenAI content-part arrays — P9-T01 handles both); a tool-role image does not (OpenAI tool messages are text-only). The text `tool_result` stays the textual observation; the image rides as the next user turn.
- **Acceptance criteria:**
  - A tool may surface an optional image (e.g. `browser_view` returns its text result plus, via a small typed seam, a `model.ImageBlock`); the loop appends it as a user turn after the tool_result. nil image ⇒ byte-identical (no extra turn).
  - The screenshot base64 is **not** dumped into the text result (it already is not — the tool acknowledges "screenshot: captured").
  - A unit test (fake model capturing the messages) asserts the image block is appended only when a screenshot is present and is absent otherwise.
- **Verify:** `make verify`; the message-assembly test above.
- **Notes:** keep the seam minimal and nil-gated so the frozen `CodingBackend` contract is untouched (I1); the image is data the model reasons over, the **verifier** (I2) still decides "done".

**Invariant ledger:** I1 (no contract change — image rides in `[]Block`), I2 (browser check feeds the verdict), I4 (browser in the sandbox, container-only), I6 (any browser/CDP module justified + CGO-free), I7 (DOM/console fenced).

---

## D2 — Semantic retrieval, activated end-to-end (Embedder wiring + cache + pure-Go HNSW)

> **SHIPPED.** A content-hash-cached, pure-Go HNSW vector index (`internal/codeintel/semantic/hnsw.go`) replaces the old linear cosine scan and is activated opt-in via `NILCORE_EMBED_KEY` (an OpenAI-compatible embedder, `internal/embed`). Off ⇒ lexical fallback, byte-identical. No new module — pure stdlib, `CGO_ENABLED=0` held (I6).

**Source:** `internal/embed` (the Embedder, shipped P10-T03), `internal/codeintel/semantic/semantic.go` (the JSON-in-SQLite store + linear cosine scan + nil-Embedder lexical fallback), `internal/codeintel/retrieve/retrieve.go` (the `Retriever{Graph, Semantic, LSP}` + fusion order + provenance vocab), `internal/tools/codeintel.go:106` (`Retriever{Graph: g}` — `Semantic` nil today), `internal/codeintel/live/live.go` (the live index lifecycle), `go.mod` (CGO_ENABLED=0).

**End-to-end goal.** Turn the built-but-dormant semantic index into a fast, real lens: embed the repo with the Embedder, **cache** so unchanged files never re-embed, and search via a **pure-Go ANN** so it scales past a linear scan — degrading to lexical when no key is set.

**Design + UX decision.** Semantic search is **opt-in via `NILCORE_EMBED_KEY`** (+ optional `NILCORE_EMBED_MODEL`), *not* silently on: embedding has real API cost and first-index latency, and a tiny tool should never surprise an operator with a bill. When the key is set, the codeintel tool opens a **persistent** index at a per-repo cache path (`paths.DataDir()`), so the cost is paid once and amortized; absent the key it is exactly today's behavior (lexical), byte-identical.

### D2-T01 — Content-hash cache in the semantic index
- **Goal:** never re-embed an unchanged document.
- **Depends on:** —  **Owns:** `internal/codeintel/semantic/`
- **Acceptance criteria:**
  - The `docs` schema gains a `hash` column; `Add(id, text)` computes a stable content hash (stdlib `sha256`), and **skips the Embed call** when the stored hash for `id` matches (the row + vector are reused). A changed `id` re-embeds and replaces. Nil-Embedder lexical mode and the existing `Add`/search contracts are preserved.
  - The migration is additive (a nullable column; existing rows re-embed once on next touch).
- **Verify:** `make verify`; a test with a counting fake Embedder asserts: first `Add` embeds, an unchanged re-`Add` does **not** embed, a changed re-`Add` does.
- **Notes:** stdlib only (I6); the JSON-in-one-column / cgo-free property holds.

### D2-T02 — Pure-Go ANN (HNSW) replacing the linear scan
- **Goal:** sub-linear vector search at repo scale, **CGO-free**.
- **Depends on:** D2-T01  **Owns:** `internal/codeintel/semantic/`
- **Acceptance criteria:**
  - `searchVector`'s `SELECT … WHERE vec IS NOT NULL` + per-row Go cosine (`semantic.go:120-148`) is replaced by a pure-Go HNSW (in-package, or a justified pure-Go module). Vectors persist in SQLite or a pure-Go on-disk graph; **no** FAISS/hnswlib/`sqlite-vec` (all cgo — they break `CGO_ENABLED=0`, the release invariant). Embedder seam + nil-Embedder lexical fallback preserved.
  - Results slot into the closed provenance vocabulary (`"semantic"`), and the fixed `Retrieve` fusion order is unchanged.
- **Verify:** `make verify`; a recall test (ANN vs the old exact scan on a fixture corpus, recall ≥ threshold) + a sub-linear-scaling assertion; a `CGO_ENABLED=0 GOOS=linux/darwin` cross-compile check. If a module is added: `go.mod` is a **contract file** → dedicated serialized task + I6 justification in the PR/CHANGELOG.
- **Notes:** prefer hand-rolled pure-Go HNSW to keep the dependency count at three.

### D2-T03 — Activation wiring (opt-in, persistent, cached)
- **Goal:** wire the Embedder + cached index into the live retrieval path, opt-in.
- **Depends on:** D2-T01, D2-T02, (P10-T03 done)  **Owns:** `cmd/nilcore/`, `internal/tools/`
- **Acceptance criteria:**
  - When `NILCORE_EMBED_KEY` is set, the codeintel tool opens a persistent `semantic.Index` at a per-repo cache path with an `embed.NewOpenAI(key, model)` Embedder, indexes the gathered files (bounded by `maxIndexedFiles`, hash-cached), and sets `retrieve.Retriever.Semantic`. Absent the key ⇒ `Semantic` nil ⇒ lexical fallback (byte-identical).
  - First-index latency/cost is bounded and logged; a key/embedder failure degrades cleanly to lexical (never fails the query).
- **Verify:** `make verify`; a test (mock embeddings server) asserts the semantic lens is used (provenance `semantic`) when keyed and lexical otherwise.

**Invariant ledger:** I3 (embed key via SecretStore/per-request header, never to the model — `internal/embed`), I6 (CGO-free, justified deps), I7 (retrieved context is read-only data — `codeintel` tool is SAFE-by-construction).

---

## D3 — Multi-language code intelligence (CGO-free)

> **SHIPPED.** A language-parser seam plus a pure-Go Python backend (`internal/codeintel/ast/{go.go,python.go}`) — Go output identical, CGO-free, not tree-sitter. The live + codeintel index walks now cover Go and Python (`ast.SupportedExtensions`). No new module; `CGO_ENABLED=0` held (I6).

**Source:** `internal/codeintel/ast/ast.go` (Go-first `go/parser`, with the explicit scope note that "a tree-sitter backend … slots in behind it later without changing callers (kept out now to preserve the zero-cgo build)"), `internal/codeintel/live/live.go:37-53` + `internal/tools/codeintel.go:131-153` (the two `.go`-suffix gates), `internal/codeintel/graph/graph.go:93-137` (`BuildFile` REPLACE-on-rebuild), `go.mod`.

**End-to-end goal.** Code intelligence (symbols → graph → repo-map → retrieval) covers more than Go, so a non-Go repo gets structural context.

**Design + UX decision (the crux is I6).** The common tree-sitter Go bindings are **cgo**, which would break the `CGO_ENABLED=0` release matrix bound to the pure-Go SQLite driver. Two CGO-free routes, in preference order:
1. **Pure-Go per-language parsers** behind the existing `Symbol`/`Reference` seam — zero new heavy dependency for languages where a pure-Go parser exists. Best for a first additional language.
2. **A wasm tree-sitter runtime** (e.g. a pure-Go wasm host running compiled tree-sitter grammars). This is a **new module** and a `go.mod` (contract) change → dedicated serialized task with explicit I6 justification; it buys many languages at once.
Pick (1) to prove the seam, then evaluate (2) for breadth — this is the explicit decision the `ast.go` scope note defers.

### D3-T01 — Language-backend seam + a second pure-Go language
- **Goal:** a pluggable parser interface behind `Symbol`/`Reference`, with a second language implemented CGO-free.
- **Depends on:** —  **Owns:** `internal/codeintel/ast/`
- **Acceptance criteria:**
  - A `LanguageParser` interface (file → `[]Symbol`, `[]Reference`, `[]Call`) with the Go backend refactored to satisfy it (behavior-identical for Go), plus one additional language via a pure-Go parser. No caller of the per-file API changes.
  - `CGO_ENABLED=0` still builds for the release matrix.
- **Verify:** `make verify`; a fixture file in the new language yields the expected symbols/refs; Go behavior is unchanged (existing tests pass).

### D3-T02 — Broaden the index gates + preserve REPLACE-on-rebuild
- **Goal:** the live + standalone index walks the supported language set, not just `.go`.
- **Depends on:** D3-T01  **Owns:** `internal/codeintel/live/`, `internal/tools/`
- **Acceptance criteria:**
  - The two `.go`-suffix gates (`live.IndexDir`, `goFilesUnder`) become a supported-extension set; `graph.BuildFile`'s atomic REPLACE-on-rebuild-per-file is preserved (only edges FROM the file's symbols pruned; inbound edges kept), so the incremental live index never leaks stale nodes/edges.
- **Verify:** `make verify`; a second-language fixture indexes into the graph; an edited non-Go file replaces only its own contribution.
- **Notes:** if a wasm/tree-sitter module is added (route 2), `go.mod` is a contract file → its own serialized task + I6 justification.

**Invariant ledger:** I6 (CGO-free is the binding constraint — the whole reason this was deferred), I7 (parsed code is data).

---

## D4 — Trigger → gated auto-PR (closing the SCM loop)

> **SHIPPED.** `nilcore watch --open-pr` / `schedule --open-pr` open a draft PR via `internal/forge` only after the human gate (the push runs inside the approved prepare step; token from the SecretStore; the agent never merges; credentials scrubbed from logs). The nil-gated orchestrator `KeepBranch` preserves the verified branch; default disposable cleanup is byte-identical (I2/I3).

**Source:** `internal/forge` (gated draft-PR, shipped P9-T05), `internal/trigger` + `cmd/nilcore/{watch.go,schedule.go,webhook.go}` (the self-start paths), `internal/policy/gateaction.go` (`OpenPR`), `internal/tools/githard.go` (hardened host-side git), `internal/secrets` (the token store).

**End-to-end goal.** When a self-started (issue / CI / cron / watch) task converges **verified**, optionally open a **draft PR** for it — only through the human gate, never autonomously.

### D4-T01 — Configured, gated trigger→PR path
- **Goal:** wire `forge.GatedOpen` into the self-start completion, behind the gate.
- **Depends on:** —  **Owns:** `cmd/nilcore/`, `internal/forge/`
- **Design + UX decision:** after a verified reversible self-start, resolve `owner/repo` from `git remote get-url origin`, base = the configured base branch (default `main`), push the worktree's `task/<id>` branch with hardened git (`HardenArgs`/`HardenedEnv`), then `forge.GatedOpen` with the path's approver and a token from `secrets.SecretStore` (`NILCORE_FORGE_TOKEN`). **Headless paths (cron/webhook) deny-default** (no human ⇒ no PR); the interactive `watch`/console path opens the PR on a Yes. Opt-in via `--open-pr` (off ⇒ today's behavior).
- **Acceptance criteria:**
  - `--open-pr` on `watch`/`schedule`/`serve --webhook`: a verified self-start offers a gated draft PR; approve ⇒ branch pushed + PR opened (mocked transport in tests); deny / headless-no-approver ⇒ no push, no PR. Token never logged / model-visible (I3).
  - `owner/repo` parse handles SSH + HTTPS remotes; a missing remote/token degrades cleanly (logs, no PR).
- **Verify:** `make verify`; tests for remote parsing, the approve/deny/headless paths (mock forge + a fake pusher), no-token-leak.
- **Notes:** the agent **never merges** (`forge` opens drafts only); push/PR stay irreversible `GateAction`s through the human gate (I3). The structured-tool git stays `status|diff|add|commit|log`; push is an orchestrator-level gated action, never a tool op.

**Invariant ledger:** I2 (only verified work is offered as a PR), I3 (gated, token in SecretStore, never to the model), I6 (stdlib `net/http` forge client).

---

## Sequencing & best practices (all of D1–D4)

- **One task = one branch = one PR**, `task/<ID>`, `Owns` disjoint, Definition-of-Done (six items), merge = the human gate — exactly as `CLAUDE.md` §5.
- **Additive & nil/flag-gated**: every item is opt-in (`NILCORE_BROWSER`, `NILCORE_BROWSER_VERIFY`, `NILCORE_EMBED_KEY`, `--open-pr`); the default binary stays byte-identical. This is the precedent set by `NILCORE_LSP_COMMAND` / `NILCORE_LIVE_INDEX`.
- **CGO-free is the recurring hard constraint** (D2-T02 HNSW, D3 tree-sitter): the release matrix is `CGO_ENABLED=0` because of the pure-Go SQLite driver. Any new module is a `go.mod` (contract-file) change needing explicit I6 justification in the PR **and** CHANGELOG; prefer hand-rolled pure-Go (the codebase already hand-rolls PBKDF2 for exactly this reason).
- **The verifier stays the sole authority on "done" (I2)** and **secrets never reach the model (I3)** in every one of these — a browser check, a deployed preview, an embedding call, a PR push: all gated, sandboxed, or SecretStore-bound.
- **Best-design bias:** opt-in over silently-on where there is cost/latency (semantic embedding), follow-up-user-turn over contract-widening (screenshot delivery), pure-Go over cgo (HNSW, parsers), gated-draft over autonomous-merge (forge). Each choice keeps the harness small and the trust model intact.

*Promoting any of D1–D4 into the canonical `docs/TASKS.md` DAG is itself a serialized contract task. Build them the way NilCore builds everything: small, reversible, verified steps. <3*
