# Provider upgrade — OpenAI · OpenRouter · OpenAI-compatible (+ web search)

**Phase 15.** Bring the OpenAI / OpenRouter / generic-OpenAI-compatible family up to state-of-the-art
— configurable endpoints, reasoning controls, structured outputs, prompt-cache accounting, OpenRouter
provider-routing, and web search — **without touching the frozen `backend.CodingBackend` contract,
without adding a Go module, and keeping the default binary byte-identical until an operator opts in.**

Read order: `CLAUDE.md` §2 (invariants) → `docs/ARCHITECTURE.md` (providers) → this file → `docs/TASKS.md`
(the Phase-15 queue rows + specs).

> **Status:** planned. Wave 1 (P15-T01 · P15-T03 · P15-T12) implementable today off `main`.
> Web-search waves (P15-T07 · T08 · T09) carry a **cross-phase dependency**: they extend
> `model.BuiltinTool`, which lands with the computer-use work — they unblock once that foundation is in `main`.

---

## 0. Scope & non-goals

In scope: the `openai` adapter, the `openrouter` adapter (which reuses it), and a **new generic
`openai-compatible` vendor** so an operator can point the chat/coding model at any compatible endpoint
(vLLM, Ollama, LM Studio, llama.cpp, Together, Groq, Fireworks, Azure OpenAI). Anthropic stays the
reference adapter (already complete); it is touched only where parity informs the design.

**Deliberately deferred / out of scope:**

- **OpenAI Responses API.** OpenAI now recommends the Responses API for new projects (better reasoning,
  40–80% better cache utilisation, first-class built-in tools). But **OpenRouter and every
  OpenAI-compatible server speak Chat Completions only**, so migrating would *fork* the shared adapter.
  We keep **Chat Completions as the portable baseline** (one adapter serves all three families) and flag
  Responses as a future OpenAI-only path, to be taken only if the cache/reasoning wins justify the fork.
- **Batch API and `GET /generation`** — they do not fit the synchronous `Provider.Complete` seam.

---

## 1. Where we are today

`internal/provider` exposes three vendors behind the `model.Provider` seam (`anthropic`, `openai`,
`openrouter`); OpenRouter reuses the OpenAI Chat Completions adapter at a different base URL. Anthropic is
complete; OpenAI/OpenRouter are correct for the basics (non-stream + streaming, role mapping, tool_calls,
base64 vision, `finish_reason`→`stop_reason`, usage, ctx-aware interrupt). The decisive gaps:

- The chat adapter's `baseURL` is an **unexported, hardcoded field with no setter** — so an arbitrary
  OpenAI-compatible endpoint is **unreachable today** (the only generic path is `openrouter`, which targets
  `openrouter.ai`, not the operator's endpoint). The embeddings client (`internal/embed`) already proves the
  fix — an exported, overridable `BaseURL` — so the template exists in-repo.
- The adapter hardcodes `"max_tokens"`, which **gpt-5.x / o-series reject** — they require
  `max_completion_tokens`. So NilCore cannot currently drive any OpenAI reasoning model. **(correctness blocker)**
- No reasoning-effort control, no structured outputs, no prompt-cache / reasoning-token accounting, none of
  OpenRouter's distinguishing routing controls, no attribution headers, and `resilience.go` retries every
  error blindly (the structured `{error:{…}}` envelope + `Retry-After` are never parsed).

A sandboxed, `guard.Wrap`'d **client-side web search/fetch** already ships
(`internal/tools/websearch.go` · `webfetch.go`) but is not selected by provider capability.

---

## 2. State-of-the-art gap matrix

| Capability | Providers | Today | Target | Importance |
|---|---|---|---|---|
| `max_completion_tokens` | OpenAI/Azure | ❌ hardcoded `max_tokens`, rejected by gpt-5.x/o-series | selectable single key (REPLACE, never both) | **must (blocker)** |
| Reasoning-effort control | OpenAI `reasoning_effort`, OpenRouter `reasoning{}` | ❌ always model default | operator-set `none…xhigh` (+ budget on OR) | **must** |
| Configurable base URL + `openai-compatible` vendor | vLLM/Ollama/LM-Studio/llama.cpp/Together/Groq/Fireworks/Azure | ❌ hardcoded unexported `baseURL`; resolver errors on other vendors | operator-set full-prefix BaseURL + new vendor | **must (core ask)** |
| Web search — native | OpenAI `web_search_options`, OpenRouter `web` plugin | ❌ missing | server-side via `model.BuiltinTool` seam | high |
| Web search — client fallback | compat / self-hosted / non-search OpenAI | 🟡 shipped, not capability-selected | auto-selected, sandboxed, `guard.Wrap`'d, mutually exclusive with native | high |
| Structured outputs (`json_schema`, strict) | OpenAI/OpenRouter | ❌ prose parsing only | opt-in constrained decoding | high |
| Prompt-caching accounting | OpenAI (auto), OpenRouter | ❌ `cached_tokens` dropped → over-charges | decode + meter at reduced rate | high |
| Reasoning-token accounting | OpenAI/OpenRouter | ❌ folded into output count | `Usage.ReasoningTokens` | high |
| OpenRouter provider routing | OpenRouter/compat | ❌ no `provider` object | typed `order/allow_fallbacks/require_parameters/data_collection/zdr/sort/max_price` | medium |
| OpenRouter `models[]` fallback + served-model id | OpenRouter | ❌ missing; `response.model` not decoded | server-side single-call failover; meter prices the model that served | medium |
| Attribution headers (`HTTP-Referer`/`X-Title`) | OpenRouter | ❌ never sent | static config strings (never the key) | low |
| `tool_choice` / `service_tier` / `parallel_tool_calls` | OpenAI/OpenRouter | ❌ always auto/default; read-side multi-tool works but untested | force/suppress tools, flex economics, disable parallel | low–med |
| Typed error envelope + `Retry-After` | all | ❌ raw body tail; resilience retries everything blindly | `model.APIError` → terminal vs retryable, honour `Retry-After` | high |

---

## 3. Target architecture & key decisions (all invariant-safe)

The whole upgrade lives behind the existing `model.Provider` seam. `backend.CodingBackend`,
`Task`, `Result`, and `Provider.Complete` keep their exact signatures (**I1**); the provider package stays
**stdlib-only** with no new `go.mod` dependency (**I6**); every new request field is `omitempty` and set once
in `newRequest`, so a zero-valued configuration produces a **byte-identical** request body and headers.

- **Configurable BaseURL + `NewOpenAICompatible(model, opts…)`** — mirror the proven
  `embed.OpenAIEmbedder.BaseURL`. BaseURL is the **full prefix**; the adapter appends only
  `/chat/completions` after `strings.TrimRight(base,"/")`, never auto-adding `/v1` — so Groq (`/openai/v1`),
  Fireworks (`/inference/v1`), Azure (`/openai/v1/`) all work. Defaults reproduce today byte-for-byte.
- **New `openai-compatible` (alias `compat`) vendor** in `ResolveWith` — `split()` is unchanged (a URL would
  mis-parse as the vendor), so BaseURL/auth/key-env come from `onboard.Config` / `NILCORE_*`, never inline.
- **Anti-key-exfiltration (I3).** Compat defaults to a **dedicated `NILCORE_COMPAT_API_KEY`**; `ResolveWith`
  *rejects* a canonical key-env name (`OPENAI_API_KEY`, …) on a compat BaseURL with a **key-free error**, so a
  real OpenAI/Anthropic key can never be silently shipped to an untrusted self-hosted host.
- **Auth descriptor** `{HeaderName, ValuePrefix}` — Bearer (default), Azure (`api-key`, raw value), None (no
  header when key empty, for local servers). Default reproduces `authorization: Bearer` exactly.
- **`max_tokens` single-key marshal** — one `MaxTokens int` marshalled into exactly one configured key name
  (`max_tokens` default; `max_completion_tokens` for reasoning models). Emitting both (which reasoning models
  reject) is structurally impossible.
- **Usage widening + typed error (the one serialised seam, P15-T03).** `model.Usage` gains additive
  `ReasoningTokens`/`CachedTokens`/`CostUSD`; a new `model.APIError{StatusCode,Retryable,RetryAfter,Type,Code,
  Message}` (key-free `Error()`) is parsed from the `{error:{…}}` envelope + `Retry-After`. `resilience.go`
  consumes it via `errors.As`: terminal `4xx` stops without failover; retryable `429/5xx` retries; `429`
  honours `Retry-After` as the backoff floor. Because this governs **every** provider's failover, it ships
  with two mandatory proof gates: (1) an untyped error still retries exactly as today; (2) a terminal
  `APIError` stops with no failover while a `429` honours `Retry-After`. `docs/ARCHITECTURE.md` is updated in
  the same PR.
- **SOTA request fields (P15-T05).** Additive `omitempty`: `reasoning_effort`, `response_format` (json_schema
  strict), `parallel_tool_calls` (`*bool`), `tool_choice`, `service_tier`, optional `prompt_cache_key`.
  Decode widens to read `completion_tokens_details.reasoning_tokens`, `prompt_tokens_details.cached_tokens`,
  `usage.cost`, and the served top-level `model` id.
- **OpenRouter typed extras (P15-T06).** A typed `openRouterExtras` sub-struct (provider routing object,
  `models[]` fallback, `reasoning`, transforms/plugins) merged **only** when the OpenRouter base is configured;
  `require_parameters:true` by default; `HTTP-Referer`/`X-Title` set from static config strings (never the key).

---

## 4. Web search — hybrid, native-first, with the I7 fence

Two paths, **mutually exclusive**, with exactly one `web_search` advertised to the model at a time:

1. **Native (server-side)** rides the existing `model.BuiltinTool` seam — the same invariant-clean pattern as
   computer-use. The adapter consumes a web-search `Builtin` **before** `Tool.MarshalJSON` runs (so the generic
   OpenAI `tools[]` path stays byte-identical): OpenRouter renders it as a `tools`-array entry
   (`{"type":"openrouter:web_search",…}`); OpenAI renders it as the top-level `web_search_options` field, emitted
   **only** when the configured model is search-capable. The provider does the fetch — NilCore makes no HTTP call.
2. **Client-side fallback** — the already-shipped sandboxed `web_search`/`web_fetch` tools
   (`internal/tools/websearch.go` · `webfetch.go`): sandbox-only via `Box.ExecWithEnv` (**I4**), Brave key as
   `$NILCORE_SEARCH_KEY` never in command/log/prompt (**I3**), results `guard.Wrap`'d (**I7**), host
   auto-allowlisted. Selected automatically for any endpoint with no native search.

**The I7 fence (P15-T08).** Provider-returned citation snippets are **attacker-influenceable web content** — a
trusted TLS transport does not make the *content* trusted. They are **never** decoded into a
`Block{Type:"text"}` (which would re-enter conversation history *and* the agent's own `emitReasoning` channel
un-fenced). Instead the adapter `guard.Wrap`s the payload at the decode boundary into a distinct
`Block{Type:"web_search_result"}`; `native.go` runs `guard.Suspicious`, emits `injection_flagged` (parity with
the shell path), and appends it as **fenced DATA only** — never via `emitReasoning`; `textBlocks` already drops
non-text blocks. This mirrors the client-fetch fence exactly.

**The capability switch (P15-T09)** guarantees native and client are mutually exclusive, so the model can never
emit a `web_search` tool_use with no handler (which would leave a tool_use without its required tool_result and
corrupt the turn).

---

## 5. Cross-phase dependency

P15-T07/T08/T09 extend `model.BuiltinTool` (web-search variant) and a new `web_search_result` block. That
`BuiltinTool` seam arrives with the computer-use work; until it is in `main`, the web-search waves cannot build.
**Wave 1 (T01/T03/T12) and the non-web SOTA waves (T02/T04/T05/T06/T10/T11) do not depend on it** and proceed
independently. Sequence the web-search waves after the `BuiltinTool` foundation merges.

---

## 6. Task DAG — `task/P15-T01 … T14`

| ID | Title | Owns | Depends | Effort |
|---|---|---|---|---|
| **T01** | Configurable BaseURL + Auth + options constructor | `provider/openai.go` (+test) | — | M |
| **T02** | `openai-compatible` vendor + dedicated-key validation | `provider/provider.go` (+test) | T01 | M |
| **T03** | ⚠ Usage widening + `model.APIError` + resilience *(serialised seam)* | `model/{model,apierror,resilience}.go` (+tests), `docs/ARCHITECTURE.md` | — | L |
| **T04** | `max_tokens` single-key marshal (REPLACE) | `provider/openai_maxtokens.go` (+test) | T01 | M |
| **T05** | SOTA request fields + widened usage/model decode | `provider/openai.go` (+tests) | T01,T03,T04 | L |
| **T06** | OpenRouter typed extras (routing, `models[]`, transforms, headers) | `provider/openrouter_extras.go` (+test) | T05 | M |
| **T07** | Web-search `BuiltinTool` variant + adapter render *(needs BuiltinTool in main)* | `model/builtin.go`, `provider/openai_websearch.go` (+tests) | T05 | M |
| **T08** | 🔒 I7 fence: `web_search_result` block + `guard.Wrap` + native.go handler | `model/model.go`, `provider/openai.go`, `backend/native.go` (+tests) | T05,T07 | L |
| **T09** | Exactly-one-web-tool capability switch | `cmd/nilcore/webcap.go` (+test) | T07,T08 | M |
| **T10** | Onboarding config + wizard for compat vendor | `internal/onboard/*` (+tests) | T02 | M |
| **T11** | Metering/pricing for new ids + authoritative `usage.cost` | `meter/pricer.go` (+test) | T03 | M |
| **T12** | Egress allowlist extensibility (sandbox only) | `policy/egress.go` (+test) | — | S |
| **T13** | Eval coverage (compat, reasoning, structured, native search) | `eval/provider-compat/` | T06,T08,T09 | M |
| **T14** | 📄 Docs: PREREQUISITES + ARCHITECTURE + TASKS *(contract, serialised)* | `docs/{PREREQUISITES,ARCHITECTURE,TASKS}.md` | T02,T03,T06,T08,T09,T10,T11,T12 | M |

## 7. Parallel execution waves

Each wave's `Owns` sets are pairwise-disjoint; every dependency resolves to a strictly earlier wave.

| Wave | Tasks (parallel) | Disjointness |
|---|---|---|
| 1 | T01 · T03 · T12 | `provider/openai.go` ‖ `model/*` ‖ `policy/egress.go` |
| 2 | T02 · T04 | `provider.go` ‖ new `openai_maxtokens.go` |
| 3 | T05 *(alone)* | sole owner of `openai.go` |
| 4 | T06 · T07 · T10 · T11 | `openrouter_extras.go` ‖ `builtin.go`+`openai_websearch.go` ‖ `onboard/*` ‖ `meter/*` |
| 5 | T08 *(alone)* | co-edits `openai.go`+`native.go`+`model.go` |
| 6 | T09 *(alone)* | new `cmd/nilcore/webcap.go` |
| 7 | T13 *(alone)* | new `eval/provider-compat/` |
| 8 | T14 *(alone)* | serialised contract docs |

**Critical path:** T01 → T05 → T07 → T08 → T09 → T13 → T14. Peak fan-out: Wave 1 (3) and Wave 4 (4).
`docs/ARCHITECTURE.md` is edited only by T03 (Wave 1, interface-level) and T14 (Wave 8, prose) — different,
sequential waves, both trivial appends. `CHANGELOG.md` is owned by no task: one `[Unreleased]` entry is
appended per PR and resolved by rebase at the merge gate (CLAUDE.md §6).

## 8. Best practices · testing · rollout

**Best practices:** stdlib-only provider package, no SDK (I6); every new request field `omitempty`, set once in
`newRequest`; secrets resolve by env *name* and ride only a per-request header, all error messages key-free
(I3); auth/attribution headers emitted only when applicable; `max_tokens` swap REPLACES the key; prefer
OpenRouter's authoritative `usage.cost`, preserve the conservative pricing floor for unknown ids; bump
`onboard.CurrentConfigVersion` with a `Migrate` step (because `DisallowUnknownFields`); document tool/vision on
self-hosted endpoints as per-model capability flags, not guarantees.

**Testing:** table-driven, hermetic, **no network** — each flavour (OpenAI / OpenRouter / compat with
Bearer/Azure/None) against its own `httptest.Server` that captures the outbound request. **Byte-identity is the
central guardrail**: a captured baseline + an extended `TestNormalToolByteIdentical` (nil-Builtin *and*
non-web-Builtin) asserts every task leaves the default body unchanged when its feature is off. Golden SSE
fixtures for streaming; parallel-tool-call fixtures (closing today's untested gap); an injection-string fixture
proving the I7 fence; the two resilience proof gates; pricing regression for known ids.

**Rollout:** opt-in by construction, no flag day — each task ships dark until configured. T03 is the one
fleet-wide behaviour change (terminal errors now stop instead of retrying) and merges only behind its proof
gates. Standard gate per task: rebase on `main` → `make verify` green → squash-merge with sign-off → one
CHANGELOG line stating the invariant preserved.
