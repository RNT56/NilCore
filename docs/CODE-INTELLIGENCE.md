# Code intelligence — semantic codebase understanding

The capability that closes NilCore's one gap versus the frontier, and the one most responsible for whether a coding agent is *great*. Governed by `docs/PRINCIPLES.md` (especially #3 context engineering and #4 understand before you change). **Local-first, in-sandbox, zero code egress.**

## Goal

Find and understand the **right** code for any task with **minimal context**, **high precision**, and **safety** — and use that understanding to make edits surgical, to scope them, and to tighten the feedback loop. Scale to large codebases without ever reading the whole tree, and get better at a specific codebase over time.

## Why single strategies fail (and the answer)

| Approach | Strength | Why it's not enough |
|---|---|---|
| Read everything | complete | drowns the window, slow, buries signal |
| Embedding/vector RAG | concept reach | low recall on complex tasks; "similar" ≠ "relevant"; splits functions; can't answer structural questions |
| grep / lexical | exact, cheap | no concepts, no structure |
| LSP only | precise structure/types | per-symbol, single-language, no concept search |

The proven answer (and the 2026 frontier consensus): **fuse complementary lenses, follow structure over similarity, rank by architectural centrality, and return minimal-sufficient context — all computed locally.** NilCore aligns with **SCIP** (Source Code Intelligence Protocol) for the semantic layer and exposes results over **code-execution MCP** for composability.

## Principles (subsystem)

1. **Structure over similarity** — follow the graph (calls, implements, deps), don't just match text.
2. **Minimal-sufficient context** — return a curated bundle, never a dump.
3. **Precise facts vs leads** — tag provenance/confidence; never act on a guess as if it were a fact.
4. **Understanding drives the loop** — impact analysis tells the verifier which tests to run and the gate how much to fear a change.
5. **Stay current cheaply** — incremental, living updates; never full re-index.
6. **Compound over time** — fuse static structure with learned memory.
7. **Local and private** — everything computed in-sandbox; zero code egress.

## Architecture — four lenses + a fusion pipeline

### L1 — Lexical (precision)
ripgrep + exact symbol lookup. The cheapest, most precise lens: known identifiers, error strings, literal scans.

### L2 — Structural (the backbone)
- **tree-sitter** parses every language with a grammar into ASTs — broad, fast, incremental, no server required. Extracts symbols (functions, types, methods, modules) and references — the "tag map."
- A **code graph** stored in SQLite: nodes = symbols/files; edges = `calls`, `implements`, `imports`, `references`, `inherits`, `defines`, `type-of`. Queried with **recursive CTEs** for transitive reachability (call paths, dependency closure, blast radius). No graph DB — SQLite is enough and stays zero-dep-aligned.
- **LSP clients** (gopls, rust-analyzer, typescript-language-server, pyright, …) layer on **precise** types, definitions, references, and diagnostics where a server exists; tree-sitter is the always-on fallback. Aligned with **SCIP** so the graph speaks a standard.

### L3 — Semantic (concept reach)
Embeddings over **whole symbols** (function/type + its doc), not arbitrary chunks, via the Provider abstraction's embeddings endpoint; stored in SQLite (`sqlite-vec`). Used as an **entry point**: a semantic hit is **expanded along the graph** (pull callers/callees/types/tests) so retrieval is structure-aware, not pure similarity. Hybrid with L1 for recall.

### L4 — Repo Map (orientation)
A compact, **PageRank-ranked**, token-budgeted skeleton: the most architecturally central files and symbols with their signatures. The agent reads the map to orient *before* reading any file — the structural skeleton before the details. Importance = centrality in the reference graph, so the map shows the load-bearing parts.

### The retrieval pipeline → Context Bundle
A query planner routes a *need* through the right lenses and composes the result. Example — "fix login failing on expired tokens":
1. **Semantic** for "login / token / expiry" → candidate symbols.
2. **Graph-expand** → their validators, callers, and the tests that exercise them.
3. **Lexical-confirm** the exact identifiers.
4. **Assemble** a **Context Bundle** within a token budget.

Hierarchical narrowing (repo → file → symbol → line) keeps each stage cheap; each stage sees only what the prior surfaced.

## Novel concepts (the NilCore synthesis)

Proven techniques become an agent *spine* when fused with NilCore's loop, gate, and memory:

### The Context Bundle (the return type)
Not files, not hits — a **minimal-sufficient, structurally-coherent slice**: the precise symbols (signatures + bodies as needed), their immediate graph neighborhood (callers/callees/types/tests), each with a one-line **"why included,"** ranked and budget-bounded. This is the unit code intelligence hands to the loop, and it is what makes context engineering real.

### The Impact Set drives the loop *and* the gate
Before an edit, the agent asks the graph for the **transitive set** of call sites, implementers, dependents, and tests a change touches. The Impact Set:
- **scopes** the change (the agent knows what it's about to affect),
- tells the **verifier exactly which tests to run first** — turning principle #1's "smallest relevant check" from aspiration into precision (run the affected tests in seconds, then the full suite),
- feeds the **autonomy gate** — a large blast radius means more caution / a human checkpoint.

This wires codebase understanding directly into verification and safety. It is the most distinctive idea here.

### Verification-targeted retrieval + SBFL
The graph maps **symbols → the tests that exercise them**. After a failure, **spectrum-based fault localization** uses *which tests fail* (and which pass) to rank the most-likely-faulty symbols, closing the loop between "what broke" and "where to look." Retrieval becomes failure-aware.

### Living map
**Incremental** re-parse of only changed files (file-watching), never a full re-index. Within a task's **worktree**, the map and graph reflect the agent's *own in-progress edits* — it understands the code as it is *becoming*, not just the starting state.

### Memory-fused understanding
The static graph is **structure**; Phase-4 cross-project **memory** is accumulated **wisdom** (conventions, gotchas, where things live, the "why" behind decisions). Fused, understanding **compounds** across sessions — NilCore gets measurably better at a specific codebase over time. (Knowledge-graph + case-based reasoning, applied to one repo.)

### Provenance + confidence
Every fact carries its source — **precise** (LSP/AST) vs **lead** (embedding/memory) — and a confidence. The agent treats structural facts as ground truth and fuzzy matches as leads to verify. It never acts on "similar" as if it were "is." This is the epistemics that keeps a powerful retriever from confidently misleading the agent.

### Symbol-level addressing
The agent navigates and edits by **symbol** ("edit `Auth.Validate`"), resolved via the graph to the exact location, with edits validated **structurally** (still parses? types still hold?) before the verifier even runs. The structured edit tool (P1-T08) becomes graph-aware.

## Integration with the rest of NilCore

- **The loop** consumes Context Bundles instead of reading broadly (context engineering).
- **The verifier** runs the Impact Set's tests first (tight feedback).
- **The gate** weighs blast radius.
- **Memory** (Phase 4) fuses learned knowledge with the static graph.
- **MCP** — the whole thing can be exposed over code-execution MCP, so other agents (or NilCore's sub-workers) query it as a code API in the sandbox, zero egress.
- **Tools** — surfaced through the registry (P1-T08); results are concise Bundles, never dumps.

## Tech stack (all local, zero egress)

tree-sitter (multi-language AST, incremental) · LSP clients (precise facts, optional) · SQLite for the graph (recursive CTEs) and vectors (`sqlite-vec`) · code-aware embeddings via the Provider abstraction · ripgrep (lexical) · file-watching (incremental updates) · SCIP alignment for the semantic layer.

## Task cluster

Built as sibling sub-packages under `internal/codeintel/` so the tasks parallelize (disjoint ownership). See `docs/TASKS.md` for full specs.

| Task | Sub-package | What |
|---|---|---|
| P3-T09 | `ast/` | tree-sitter parsing + symbol/reference extraction (foundation) |
| P3-T10 | `graph/` | code graph in SQLite + structural queries (recursive CTEs) |
| P3-T11 | `repomap/` | PageRank-ranked, token-budgeted repo map |
| P3-T12 | `lsp/` | LSP client for precise facts, graceful fallback (SCIP-aligned) |
| P3-T13 | `semantic/` | symbol embeddings + structure-aware hybrid retrieval |
| P3-T14 | `retrieve/` | the fusion pipeline + Context Bundle assembly |
| P3-T15 | `impact/` | Impact Set (blast radius) + test-impact mapping + SBFL |
| P3-T16 | `live/` | incremental/worktree-aware updates + memory fusion |
