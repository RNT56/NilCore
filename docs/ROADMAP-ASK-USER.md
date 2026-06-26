# ROADMAP — `ask_user`: attended-only interactive clarification

**Status:** proposed (design approved, unimplemented). Namespace `AU-T##`. Slots after Phase 15 as **proposed Phase 16**.
**Revision:** v2 — adds **batched questions** (1–5 per ask, each with multi-select choices + always-available free-form), reviewed by a second adversarial panel. The batch is a presentation loop over the v1 single-flight primitive; no new session park state.
**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` (§Execution model, §Security) → `docs/PERSONA.md` §2 → this file.

> One-line goal: let the **core/native agent ask the human operator** — either **one sharp question** or a **short sequence (up to 5) of choice/free-form questions** (each allowing multi-select **and** a custom answer) — **only when a decision genuinely forks on something irreversible or expensive, and only when a human is synchronously reachable.** For planning checkpoints and roadblocks. Headless runs never ask and never block. This wires the behaviour `docs/PERSONA.md` §2 already promises but the code currently forbids.

---

## 0. Decisions taken (tunable defaults)

These were defaulted from the adversarial review; each is a one-line change if you want a different profile:

| Knob | Default | Note |
|---|---|---|
| Questions per `ask_user` call | **1–5** (your stated cap) | sequential a→b→c; `errorResult` on 0 or >5 |
| Choices per question | **0–6** (`0` = pure free-form question) | non-empty unique labels; exactly-1 choice → treated as free-form |
| `multiSelect` per question | **false** by default | model sets `true` to allow selecting several |
| Per-drive **ask budget** | a **conversational scale** (off · minimal · low · **normal** · high · max) | the operator dials it by talking ("ask me fewer questions" → the `set_ask_level` tool) or with the `/questions less\|more\|off\|normal` verb; sticky + persisted like Mode; normal = 3 asks/drive |
| Empty reply | **re-prompt once → "declined / you decide"**, continue | never wedges the batch |
| Wall-clock backstop | **resets per sub-answer**; returns partial answers on timeout | bounds operator *absence*, not deliberation |
| Phase-2 channels | **no `channel.go` change** (transport-agnostic) | the question is an `emit` event the channel already streams; the reply is an ordinary thread message `Turn` already routes — see §6b |

The budget is no longer a fixed flag — it is the **ask-level scale** in §0.2, which the operator moves up or down by one notch in conversation.

### 0.1 Concurrency: asking while subagents run (planned — multi-agent phase)

When the asking agent is a **supervisor** with sibling subagents in flight (the `build`/`swarm`/`supervise` machines), it should be able to choose what happens to that concurrent work while a human answers — NilCore already has the main-agent↔subagent and advisor↔executor tiers, so this is the human-ask analogue:

- **`let_run`** *(default)* — siblings keep working while the human answers. The ask gathers information; nothing irreversible happens, so there is no reason to stall throughput.
- **`pause`** — siblings finish their current step and park at the next safe boundary (when the answer may invalidate in-flight work).
- **`stop`** — siblings are cancelled (when the answer fundamentally redirects the run).

This is a **multi-agent concern and is NOT in Phase 1**: the interactive native loop has no siblings, so a Phase-1 ask simply parks the one drive. It is scoped as a later task (`AU-T08`, depends on the supervisor/concurrency machinery): add an optional `scope: let_run|pause|stop` field on `ask_user`, resolved by the supervisor against the concurrency scheduler (Kahn-wave pool) and the peer bus. Until then, supervised/project sub-drives leave `AskUser` nil (a worker asks its supervisor, never the human), so multi-agent runs are unaffected.

### 0.2 The ask-level scale (built)

A 1..6 ordinal — **off · minimal · low · normal · high · max** — sticky per conversation and persisted with `WorkState` (survives a restart, like Mode). It maps to the per-drive `ask_user` budget (off=0, minimal=1, low=2, normal=3, high=4, max=6). The operator moves it:

- **By talking** — saying "ask me fewer/more questions" → the model calls the `set_ask_level` tool (`less`/`more` = one notch; `off`/`normal`/a number = absolute), which the session applies and acks. Advertised only with `ask_user` (attended); fails closed headless.
- **By verb** — `/questions less|more|off|normal|<0–5>` (deterministic, principal-only, I7-safe); bare `/questions` (via `/status`) shows the current level.

At level **off** the `ask_user` tool refuses every call ("asking is turned off — proceed on assumptions"), so the operator can tell the agent to stop asking entirely. The per-drive counter lives in the native `Run` loop (per-drive, like `consecutiveFailures`); the level lives on the session, read live, so a mid-drive change takes effect at once.

---

## 1. The problem — a real contradiction, not a missing nicety

`docs/PERSONA.md` §2 ("Clarify vs act") specifies the intended behaviour verbatim:

> Default to **acting**. … Ask **exactly one** sharp, specific question … only when the ambiguity genuinely forks and no safe assumption resolves it, or when proceeding would require guessing on something **irreversible or expensive**.

This is **designed but entirely unwired**, and the code does the opposite:

| What PERSONA §2 promises | What the code actually does | Site |
|---|---|---|
| Ask one sharp question on a genuine fork | Base system prompt ends **"Do not ask the user questions; act."** — reused verbatim by the interactive `nilcore chat` front door | `internal/backend/native.go:209`; `cmd/nilcore/chat.go` `chatNativeBackend` (~`:718`) |
| A channel to the operator | **No tool** asks the human. `ask_advisor` → a strong *model*; `ask_supervisor`/`request_review` → a peer *agent* over the bus. `peerGuidance` is explicit: the supervisor "is a peer agent, not the user." | `native.go` dispatch (~`:588`–`792`), `:218` |
| Reach a human on a heavy-consequence decision | The **only** human path is the policy gate — **binary approve/deny**, triggered by `Classify()` detecting an irreversible *command* (push/merge/deploy/pay), **not** a decision fork | `internal/policy/policy.go:59`, `internal/policy/approver.go:26` |
| Carry the operator's answer back | `channel.Channel.Ask` returns `(bool, error)` — yes/no only — and `channel.go` is a **frozen contract file** | `internal/channel/channel.go:30` |
| — | The user→agent seam (`internal/inbox`) is **inbound-only**. No outbound, blocking loop→user channel exists | `internal/inbox/inbox.go` |
| — | There is **no attended/headless signal**. Interactivity is only *inferred* from which `policy.Approver` was wired — fragile | `internal/agent/orchestrator.go:77` |

Two facts uncovered during design shape the work and are easy to get wrong:

1. **`AwaitingGate` is dormant.** `session.Phase` `AwaitingGate` (`internal/session/state.go:44`) is **declared but never assigned** — the real gate blocks *inside* `agent.Orchestrator`/`ConsoleApprover` one layer below the session, invisible to the `Session.Phase` machine. So "make `AwaitingInput` mirror `AwaitingGate`" copies a precedent **that does not exist**. The parked-phase machinery is genuinely new.
2. **The chat front door already has a latent stdin race.** `ConsoleApprover` reads `os.Stdin` directly (`internal/policy/approver.go:28`) while the REPL reader scans the *same* `os.Stdin` (`cmd/nilcore/chat.go:916`) — competing `bufio` readers. `ask_user` forces unifying input (§4).

---

## 2. Design principles (invariant analysis)

- **I1 — frozen backend contract.** `backend.Task`/`Result`/`CodingBackend` are **untouched**. The capability rides a new **optional `backend.Native.AskUser`** field, gated nil like `Inbox`/`Peer`/`Wake`. `nil ⇒ byte-identical loop`. **The `AskHandle`/`AskQuestion`/`AskChoice`/`AskAnswer` types are declared *in* `internal/backend`** over backend-owned values (mirroring `Peer`, `native.go:187`–`200`); `internal/ask` owns its own structurally-identical types and an adapter converts at the seam, so `backend` never imports the leaf — it stays import-leaf.
- **I3 / I4 — no ambient authority; sandbox boundary.** A headless run has no synchronous principal, so `ask_user` must never block or fabricate an answer. Four structural layers (§3.6).
- **I5 — append-only log.** Metadata-only events, **one `ask_user`/`ask_user_answered` pair per sub-question** plus a `batch_open`/`batch_close` marker carrying `question_count`. `Detail` carries only counts/lengths/modes — **never** question text, choice labels, or custom text (those live only in the replayable transcript), same discipline as `inbox.Push` (`inbox.go:88`–`95`) and `advisor_consult` (`native.go:1002`).
- **I7 — untrusted input is data.** Folded answers are trusted **principal** input (un-`guard.Wrap`'d), but the trust rule is now **per-field**: `AskAnswer.Custom` is the operator's typed turn, **length-clamped**; `AskAnswer.Selected[i]` is a **verbatim copy of the model-authored** `AskQuestion.Choices[k].Label`, selected by valid index→label lookup *only* (an unresolved index is **dropped, never nudged** to a nearby label, never matched against typed text). Typed prose can only ever land in `Custom`. Each answer is exactly the **one principal turn** consumed at its sub-rendezvous, post-auth.
- **§1 north star — bounded loop.** A parked ask burns zero tokens but holds a worktree; the wall-clock backstop **and** the per-drive ask budget bound it.
- **§6 zero-dep core.** New leaf package is stdlib-only (`sync`, `bufio`, `strings`, `io`, `context`) + `internal/eventlog`.

---

## 3. The design

### 3.1 Seam — `backend.AskUser` capability (batched)

```go
// internal/backend/native.go — interface + value types declared locally over
// backend-owned types (like Peer), so backend never imports the concrete leaf.
type AskHandle interface {
    // Ask poses 1–5 questions in sequence and BLOCKS until the operator has
    // answered them all (or ctx/backstop fires), honoring ctx. Returns one
    // AskAnswer per question, in order.
    Ask(ctx context.Context, qs []AskQuestion) ([]AskAnswer, error)
}
type AskQuestion struct {
    Prompt      string
    Choices     []AskChoice // 0–6; empty ⇒ pure free-form question
    MultiSelect bool
}
type AskChoice struct{ Label, Detail string }
type AskAnswer struct {
    Selected []string // verbatim model-authored labels (index→label), never typed text
    Custom   string   // the operator's free-form turn, length-clamped
}

type Native struct {
    // … existing optional seams …
    AskUser AskHandle // nil ⇒ ask_user absent; byte-identical loop
}
```

Distinct from `policy.Approver` (yes/no on an irreversible *command*); this is the model's open decision fork. The drive blocks on **one** `Ask(ctx, qs)` for the whole batch — the per-question sequencing lives in `internal/ask.Box` (§3.7), **not** in the session.

### 3.2 Prompt — wire PERSONA §2, gate it on reachability

- **Remove** the unconditional `"Do not ask the user questions; act."` (`native.go:209`); replace with an act-first, state-your-assumptions sentence so the **headless default still never asks**.
- **Add** `askGuidance` (sibling of `peerGuidance`/`sleepGuidance`), appended by `systemFor()` **only when `AskUser != nil`**. It carries PERSONA §2 plus the batch rules: default to acting; use `ask_user` only on a genuine fork or an irreversible/expensive guess; **batch only INDEPENDENT questions** (all asked before any answer is seen) — *"if question b depends on the answer to a, ask a alone, then call `ask_user` again"*; prefer one sharp question; never to confirm reversible work; never to re-ask what the conversation/task/files already answer.
- **Mode-aware (§3.9):** the "prefer a cheap run-a-command probe" clause is **omitted** when composed for a read-only mode (`/discuss`, `/plan`) — those drives have no shell.

All `native.go` edits land in **one** task (§6).

### 3.3 The tool — a batch of up to 5 questions

```jsonc
// name: ask_user
{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array", "minItems": 1, "maxItems": 5,
      "description": "1–5 INDEPENDENT questions, presented one after another (a, b, c…). Batch only questions you can ask before seeing any answer; for a dependent follow-up, call ask_user again.",
      "items": {
        "type": "object",
        "properties": {
          "question":    { "type": "string" },
          "choices": {
            "type": "array", "minItems": 0, "maxItems": 6,
            "items": { "type": "object",
              "properties": { "label": {"type":"string"}, "detail": {"type":"string"} },
              "required": ["label"] },
            "description": "0–6 options. Omit/empty for a pure free-form question. The operator may always add a custom answer."
          },
          "multiSelect": { "type": "boolean", "default": false }
        },
        "required": ["question"]
      }
    }
  },
  "required": ["questions"]
}
```

**Decode-time validation** (→ `errorResult`, never park): `questions` length 1–5 (don't silently clamp — a >5 batch is a model error worth surfacing); each `choices` 0–6 with **non-empty, unique** labels; non-empty `question`; a question with exactly one choice is auto-promoted to free-form (a menu of one is malformed). This **replaces** v1's `minItems: 2` choices schema.

**Co-emission rule:** if `ask_user` appears in an assistant turn **alongside any other tool_use block**, it is **not** parked — it returns `errorResult("emit ask_user alone — no other tool calls in the same turn")` and the co-emitted tools run normally. This avoids freezing a half-built turn behind a human wait (the Anthropic API requires all `tool_result` blocks to lead the user turn — `native.go:580`). **Single-flight** otherwise: a second `ask_user` in one turn → `errorResult("one ask_user at a time — put all your questions, up to 5, in a single call")` (the wording nudges batching, not serial retries).

### 3.4 Reply encoding — choices, multi-select, custom (normative)

The front door presents each question in turn (`[a/3]`, `[b/3]`, …) with a numbered menu (if any) and an explicit free-form line. **Per-question resolution** maps the operator's one reply line to `(Selected, Custom)`:

1. Trim the line. **Empty** → re-prompt **once**; a second empty → *declined* (`Selected=[]`, `Custom=""`; the model sees "operator declined Qk — you decide"); continue the batch. *(This is the universal non-wedging terminal.)*
2. **Single-select** question (`multiSelect=false`): a **bare in-range integer** `k` → `Selected=[label_k]`. **Anything else** (prose, out-of-range, `"1,3"`, `"2 but staging"`) → `Custom =` the whole line **verbatim**, `Selected=[]`. *(Unchanged from v1 §3.4 — no "leading index + trailing custom" parsing for single-select.)*
3. **Multi-select** question (`multiSelect=true`): split on the **first `;`** → `idxPart` / `customPart`. If `idxPart` is non-empty **and every** comma/space token in it is a valid in-range integer → `Selected =` those labels, **deduped, in menu order**; `Custom = customPart` (may be empty). **Otherwise** (any non-integer or out-of-range token) → `Custom =` the whole line verbatim, `Selected=[]` (never silently drop a token).
   Examples: `1,3` → {L1,L3}; `1,3 ; only on staging` → {L1,L3}+custom; `do X` → custom only; `1 9` (9 out of range) → free-form verbatim.
4. **I7 per-field:** `Selected` entries are *always* exact copies of model-authored labels via index lookup; an unresolved index is dropped, never nudged; typed prose only ever becomes `Custom`. `Custom` is length-clamped.

The model receives **one** `tool_result` block summarising the batch (labels **quoted** so a comma-bearing label is unambiguous; selected order = menu order):

```
operator answered 3 of 3 questions:
Q1. <question a>
   → chose: "Label A", "Label C"   (note: only on staging)
Q2. <question b>
   → "<free-form answer>"
Q3. <question c>
   → declined (you decide)
```

The model's view is identical across console / TUI / channel — it never sees indices or the `AskAnswer` struct.

### 3.5 Blocking, budget & backstop

Dispatch the `ask_user` case **like `ask_supervisor`** (`native.go:702`, blocking under the **drive ctx**) — **not** like `sleep` (which ends the drive). The drive parks on one `n.AskUser.Ask(ctx, qs)`:

- **Resolve** → the formatted batch summary folds as the `tool_result` for that `tool_use` ID; the **same** drive continues.
- **ctx cancel** → clean interrupted `Result`, never a fault.
- **Wall-clock backstop** — bounds operator **absence**: the timer (default 30 min, flag-tunable) **resets on each sub-answer**. On timeout, the `AskAnswer`s collected **so far are returned** ("operator answered K of N before timing out — proceeding on assumptions"); never silently dropped. Distinct result text from budget exhaustion.
- **Per-drive ask budget** — a counter in the native `Run` loop (per-drive, like `consecutiveFailures` at `native.go:809` — **not** on the session-lived `AskHandle`, which would leak across drives). Default **3 calls/drive**. On exhaustion, `ask_user` returns `"ask budget exhausted — proceed on your best assumptions and state them"` (distinct from the absence/timeout text, so the model can treat the two differently). Reuses the backstop's proceed-on-assumption fold path.

### 3.6 Headless fail-closed — four structural layers

1. **Advertise-gate** — tool appended to `toolDefs` only when `AskUser != nil`.
2. **Prompt-gate** — `askGuidance` appended only when wired.
3. **Dispatch-gate** — nil `AskUser` → `errorResult("unknown tool: ask_user")`, never blocks (the `sleep`/peer nil-guard pattern, `native.go:677`–`680`). **Adversarial test is a blocking acceptance criterion.**
4. **No-Session-on-headless-path** — the orchestrator's headless `agent.Orchestrator → backend.Native` path has **no `Session`**, so a parked phase is not representable headless.

Headless sites leave `AskUser` nil: durable-resume (`resumeInflight`, `main.go:1283`), webhook, `watch`, `schedule`/cron, `swarm`, any serve session with no live receive loop. The attended signal is its **own** field — **never inferred from which Approver was wired** (`watch` wires a `ConsoleApprover` yet must stay headless).

### 3.7 Phase machine & park (batch = loop over the v1 primitive)

This is the load-bearing correction from the v2 review. The session park stays at **single-park granularity**; the per-question sequencing lives in the leaf:

- The `Session` owns the outbound seam `*ask.Box` and a **wrapping `AskHandle`** that flips `Phase = AwaitingInput` under `s.mu` **once** on entry to `Ask(ctx, qs)` and restores `Working` **once** on its return. `Session.Phase` has **no per-sub-question granularity** — no "sub-index k" state. (`AwaitingInput` is the *first real* parked-phase wiring; built from scratch, since `AwaitingGate` was never assigned.)
- `ask.Box` runs the **N-question collection loop internally**: for each question it requests the next principal line via its REPL-reader resolve channel, applies §3.4 resolution + the at-most-one re-prompt, advances its cursor, and after the last question returns `[]AskAnswer` once. So **"batch" is a presentation loop over the proven one-line rendezvous** — each sub-answer is still exactly one principal turn (preserving the v1 no-ambiguity and one-turn-per-answer properties).
- **Event log:** one `ask_user`/`ask_user_answered` pair **per sub-question** (preserving "one principal turn per `ask_user_answered`"), bracketed by `batch_open`/`batch_close` carrying `question_count` — all metadata-only.
- `RunSupervise`/`RunProject` sub-drives leave `AskUser` nil (a subagent asks its **supervisor** via the peer bus, never the human).

### 3.8 Park interactions & disambiguation

While a batch is outstanding (`Phase == AwaitingInput`):

- **Every non-control principal line is consumed as the next sub-answer** — `classifyInterrupt` is **bypassed for answer lines** (so an answer that legitimately begins with `!`, e.g. `"!important: staging"`, is never mis-read as a steer). There is **no inbox queueing mid-batch** — this diverges from v1's single-question "a second line queues," and is stated deliberately.
- **Redirect / abort** is via the **`/cancel` control verb** (parsed by the principal-only `ParseControl`, so it's cleanly distinguishable from an answer) or **Ctrl-C** — *not* a `!` prefix. `/cancel` cancels the drive ctx; the parked `Ask` unwinds; any already-collected answers are left in History (record stays complete) with no dangling `ask_user` lacking a matching `answered`/`unanswered` event; Session → `Idle`. The operator can also simply **answer with redirecting free-form text** ("actually, do X instead"), which the model receives as trusted principal input and can act on.
- **No gate can interleave mid-batch** — the drive is parked on `Ask`, running no tools, so the §4 single-reader unification holds *by construction*; record this as a safety property.
- **Budget** — parking burns zero model calls/tokens.
- **Trust/log** — per §3.4 (per-field) and §2 (I5/I7).

### 3.9 Modes (`/discuss`, `/plan`)

`AskUser` is wired **independently of Mode** — it changes nothing, so it's read-only-safe and **most** useful in a planning conversation that naturally forks (multi-select shines here). An attended `/plan` or `/discuss` drive **advertises `ask_user`**; its `askGuidance` **omits** the "run a probe" clause (shell is off).

### 3.10 Checkpoint / resume

A pending ask is **in-memory only — never persisted.** A drive killed mid-batch resumes via the **headless** path (no `AskUser`) and **cannot re-pose** the question — it proceeds on assumption or stops at a gate (safe I3 default). *Optional:* fold a one-line "a fork was pending" marker (not its body) into the bounded `WorkState`.

---

## 4. The line-REPL input-unification decision (must-fix)

`ask_user` cannot ship attended until the chat front door has **one** stdin authority. Today `ConsoleApprover` (`approver.go:28`) and the REPL reader (`chat.go:916`) compete for `os.Stdin`. **Decision:** route **both** the gate and the `ask_user` answer through the **single REPL reader + `sess.Turn`**, resolved by `Phase` — finally making `AwaitingGate` *real* and retiring `ConsoleApprover`'s direct read. The session's "parked rendezvous resolved by the next principal turn" primitive (`AU-T03`) serves **both** `AwaitingInput` and `AwaitingGate`; `ask.Box` reuses it N times for a batch. TUI: ask and gate serialize on the **same modal**, rendering a batch as a **sequential stepper** (question k of N) in one modal session — never N competing modals. A bounded, deliberate scope expansion that fixes a pre-existing bug.

---

## 5. Phasing

- **Phase 1 — chat/console MVP. Touches NO contract file.** Batched `ask_user` over the line-REPL + TUI and serve-live-console, with the input unification.
- **Phase 2 — channel contract extension (serialized).** Exactly one new method on `channel.Channel` (returning flattened `[]string` — §0) so Telegram/Slack reuse the identical model. Done alone, updating `docs/ARCHITECTURE.md` in the same PR.

---

## 6. Task DAG (`AU-T##`)

> Owns sets are disjoint among concurrently-open branches. `internal/backend/native.go` is claimed exclusively by `AU-T02`. `channel.go`, `docs/ARCHITECTURE.md`, `docs/TASKS.md` are contract files — only `AU-T06` touches them, serialized.

### Phase 1

**AU-T01 — `internal/ask`: outbound seam + batch collection loop (leaf)**
*Owns:* `internal/ask` · *Depends on:* — · *Phase 1*
- `ask.Box.Ask(ctx, qs)` runs the **N-question sequential collection loop** over a cap-1, single-flight resolve channel; `Resolve(line) bool` feeds the current sub-question; owns the cursor + per-question **at-most-one re-prompt**; ctx-cancel abandons; an internal **wall-clock backstop that resets per sub-answer** returns partial answers.
- Owns the §3.4 resolution (single-select bare-integer; multi-select `;`-split grammar; dedupe/menu-order; I7 per-field index→label) and its own `Request/Choice/Answer` value types.
- Stdlib-only + `eventlog` metadata audit; imports no loop/channel/session machinery.
- *Acceptance:* table-driven resolution tests — `"1"`, `"1,3"`, `"1,3 ; note"`, `"2 but staging"`, `"do X"`, `""` (→reprompt→declined), `"1 1 2"` (dedupe), `"1 9"` (out-of-range→free-form); N-question roundtrip; double-batch rejected; ctx-cancel + per-answer-reset timeout; race-clean; metadata-only audit. `make verify` green.

**AU-T02 — `backend.Native`: batched `ask_user` tool + `AskUser` seam + prompt + budget (single native.go task)**
*Owns:* `internal/backend/native.go` (+ `internal/backend/ask.go` for the local interface/value types) · *Depends on:* `AU-T01` · *Phase 1*
- Declare `AskHandle`/`AskQuestion`/`AskChoice`/`AskAnswer` **in `internal/backend`** (backend does **not** import `internal/ask`); optional `Native.AskUser`.
- Advertise `ask_user` iff set; **decode-time validation** (1–5 questions, 0–6 unique non-empty labels, free-form promotion); **co-emission rejection**; **single-flight**; park on `AskUser.Ask` under the drive ctx; fold the formatted batch summary as the `tool_result`; ctx-cancel/timeout/budget paths with **distinct result texts**; nil `AskUser` → fail-closed `errorResult`.
- **Per-drive ask-budget counter in the `Run` loop** (default 3); replace the `native.go:209` no-ask sentence; add `askGuidance` (batch + independent-questions rule, mode-aware) via `systemFor` iff set.
- Per-sub-question `ask_user`/`ask_user_answered`/`ask_user_unanswered` events + `batch_open`/`batch_close`, metadata-only (I5).
- *Acceptance:* fake-`AskHandle` tests — advertised iff set; a 3-question batch resolves and resumes the **same** drive; **`write`+`ask_user` in one turn → ask_user errorResult, write runs**; second `ask_user` → single-flight error; budget exhaustion vs timeout give different texts; **adversarial nil-`AskUser` hallucinated call fails closed**; base prompt no longer contains the categorical no-ask sentence; nil seam ⇒ byte-identical loop. `make verify` green.

**AU-T03 — session: `AwaitingInput` + single-park rendezvous primitive**
*Owns:* `internal/session/state.go`, `internal/session/session.go`, `internal/session/drivers.go` · *Depends on:* `AU-T01` · *Phase 1*
- Add `Phase AwaitingInput`; thread `AskUser AskHandle` (session-local interface) through `DriveInput`/`NativeRun`; supervisor/project drives nil.
- Session owns an `ask.Box` and a **wrapping `AskHandle`** that flips `Phase` under `s.mu` **once** on park entry / restores `Working` **once** on the single `Ask` return — **no per-sub-question Phase state**. Build the generic "parked rendezvous resolved by the next principal turn" primitive (reused by the gate in `AU-T04`); while `AwaitingInput`, a non-control line routes to `ask.Box.Resolve` (bypassing `classifyInterrupt`); `/cancel`/Ctrl-C abort; **no inbox queueing mid-batch**.
- *Acceptance:* fake-driver tests — an **N-question batch**: N lines resolve N sub-answers and resume `Working`; `/cancel` mid-batch unwinds to `Idle` leaving collected answers in History with no dangling event; an `!`-leading answer line is **not** treated as a steer. No contract file touched. `make verify` green.

**AU-T04 — chat front door: unify input, wire Asker, sequential render, make `AwaitingGate` real**
*Owns:* `cmd/nilcore/chat.go`, `cmd/nilcore/tui.go` · *Depends on:* `AU-T02`, `AU-T03` · *Phase 1*
- Interactive chat (line-REPL + TUI) sets `NativeRun.AskUser` from the session `ask.Box` adapter; headless `cmd` paths leave it unset.
- **Retire `ConsoleApprover`'s direct `os.Stdin` read in chat** (§4): gate + `ask_user` resolve through the single REPL reader + the `AU-T03` rendezvous; `AwaitingGate` becomes assigned. TUI renders a batch as a **sequential stepper in one shared modal** (gate + ask serialize). `ask_user` available in attended `/plan`/`/discuss`.
- *Acceptance:* scripted-provider test — a chat drive emitting a 3-question `ask_user` parks, renders a→b→c, accepts choice/multi-select/free-form answers, continues to finish; **interleave a gate and an `ask_user` and assert no input lost/misrouted**; a multi-question ask renders sequentially in one modal with no gate firing during it; attended `/plan` advertises `ask_user` and its guidance omits "run a command." `make verify` green.

**AU-T05 — serve-live wiring + headless-suppression proof**
*Owns:* `cmd/nilcore/main.go` (serve wiring) + a guarding test · *Depends on:* `AU-T02`, `AU-T03` · *Phase 1*
- A live, attached serve session wires `AskUser` (console-style for phase 1); resume/detached/`informGateApprover` paths leave it nil.
- *Acceptance:* a test builds each headless entry point's `backend.Native` (`watch`/`schedule`/`webhook`/`swarm`/resume) and asserts `AskUser == nil` and `ask_user` absent from `toolDefs`; resumed drive can't re-pose; the adversarial headless test passes. `make verify` green.

### Phase 2 — contract (serialized)

**AU-T06 — `channel.Channel.AskQuestions` over Telegram/Slack (dedicated serialized contract task)**
*Owns:* `internal/channel/channel.go`, `internal/channel/telegram`, `internal/channel/slack`, `docs/ARCHITECTURE.md` · *Depends on:* `AU-T04`, `AU-T05` · *Phase 2*
- Extend `Channel` with **one** method: `AskQuestions(ctx, threadID string, qs []Question) ([]string, error)` — input carries the batch structure (so transports can render checkboxes for `multiSelect`); **return is a flattened `[]string`** (one resolved per-question string), keeping `Selected/Custom` structure harness-side and the frozen surface minimal. **Keep `Ask(...) (bool, error)` verbatim** for the gate.
- Telegram/Slack render each question with inline buttons (checkboxes when `multiSelect`) + a free-text field, presented as N sequential exchanges; non-rendering transports fall back to a numbered menu. Sender `Auth` (`P2-T07`) gates **who** may answer.
- Update `docs/ARCHITECTURE.md` (Channel seam) in **this** PR (DoD). Run alone, no parallel reader of `channel.go`.
- *Acceptance:* every transport + fake satisfies the widened interface; per-thread routing (thread A's batch answers resolve A, never leak to B). `make verify` green.

**AU-T07 — `ChannelAsker` + serve-live over channels**
*Owns:* `cmd/nilcore/serve_asker.go` (+ serve wiring) · *Depends on:* `AU-T06` · *Phase 2*
- A `ChannelAsker` satisfying `backend.AskHandle` by delegating to `Channel.AskQuestions`, wired only for a live/attached, authorized serve thread; resume/detached stay nil.
- *Acceptance:* end-to-end multi-thread test — thread A parked on a batch resolves on A's messages, thread B queues normally, no cross-leak; metadata-only events. `make verify` green.

### Promotion / meta
- Append `AU-T01..AU-T07` to `docs/TASKS.md` (contract file — own serialized append) and a `CHANGELOG.md` line per merged task.

---

## 6b. Implementation status (branch `feat/ask-user-phase1`)

**Phase 1 (AU-T01–T05) is IMPLEMENTED end-to-end on this branch + the ask-level scale (§0.2); `make verify` green, lint clean.** Built:

- **`internal/ask`** (new leaf) — `Box`: the outbound rendezvous + sequential per-question collection, the §3.4 resolution rules, the one re-prompt, and the per-answer wall-clock backstop. Tests: the full resolution table, batch roundtrip, timeout (partial), cancel, single-flight, re-prompt.
- **`internal/backend`** (`ask.go` + `native.go`) — `AskHandle`/`AskQuestion`/`AskChoice`/`AskAnswer` + `ErrAskTimeout` (declared in backend; `ask` imports them, backend stays import-leaf); the `ask_user` + `set_ask_level` tools (advertised iff `AskUser` wired); decode-time validation; co-emission rejection; single-flight; per-drive budget via `MaxAsks`; the prompt rewrite (removed the categorical no-ask line; `askGuidance` gated on the seam + mode-aware). Tests: advertised-iff-wired, answer-flows-back, co-emission-rejected, nil-fails-closed, budget-off, set_ask_level dispatch, guidance gating, validation.
- **`internal/session`** (`ask.go` + `asklevel.go` + edits) — `AwaitingInput` phase; the session-owned `askAdapter` (one park-flip around the whole batch); `Turn` answer-routing (bypasses `classifyInterrupt`; `/cancel` aborts); the ask-level scale + `SetAskLevelSpec`; `AskLevel` persisted in `WorkState`; the `/questions` control verb. Tests: park→resolve→resume, park→cancel, the level scale + round-trip.
- **`internal/emit`** — `KindAsk`; **`internal/termui`** renders the question with a `?` marker.
- **`cmd/nilcore/chat.go`** — `EnableAskUser` on the interactive session (attended); `n.AskUser` wired in `chatNativeBackend`; REPL settles the spinner and shows the prompt while `AwaitingInput`; `/questions` + `/status` ask-level; help text.

**Phase 2 (channels) is ALSO IMPLEMENTED — and the `channel.go` contract extension turned out to be UNNECESSARY.** The original `AU-T06/T07` plan assumed the *transport* must run the Q&A (a new `Channel.AskQuestions`). But the session's ask machinery is **transport-agnostic**: the question is just an `emit` event the per-thread `channelEmitter` already streams to the thread as a message, and the operator's reply is just an ordinary authorized inbound message that serve's `intake → Session.Turn` already routes — to the ask box, via the Phase-1 `AwaitingInput` `Turn` branch. So Phase 2 = **`cmd/nilcore/main.go`** wires `n.AskUser` in `serveNativeBackend` and calls `sess.EnableAskUser(out)` for live serve threads (the headless `resumeInflight` builds no `Session`, so it stays nil — fail-closed); **`internal/server`** renders `KindAsk` with a `❓` marker (`surfaceLine`) and handles the `/questions` verb + `/status` ask-level. **Zero change to the frozen `channel.go` contract** (I1/§5 untouched — better than the spec's planned extension). Tests: a server-level end-to-end round-trip (a drive parks on `ask_user`, the question streams out as an Update, an authorized thread reply resolves it) + the `❓` marker, race-clean.

**Deferred (as designed):** the §4 line-REPL/`AwaitingGate` unification was **not needed** — the interactive chat native drive does not invoke the console approver (its `run` tool is `CommandGuard`-gated, and `Orchestrator.Gate` is reached only from project/selfimprove/trigger/mcp), so no gate can race the REPL reader during a native ask. It remains a prerequisite for wiring `ask_user` into a drive that *can* gate (a chat `/execute` that escalates to supervise/project), tracked with **AU-T05b**. Telegram/Slack get a plain-message question + text reply today (the transport-agnostic path); native **inline buttons / checkboxes** for choices are an optional polish (**AU-T06b**, the only thing the old `AskQuestions` contract method would have bought). **`AU-T08`** (the §0.1 concurrency scope) is unbuilt.

---

## 7. Open risks & deferred

- **Interruption profile vs persona.** Up to 5 questions × 3 calls is generous for a "default to acting, ask exactly one sharp question" character (PERSONA §2). Managed by the prompt (act-first; batch only independent forks) + the per-drive budget; the budget default is the lever (§0).
- **Intra-call branching is intentionally absent.** Questions in a batch are independent (all asked before any answer). True branching ("finish, or continue with b that depends on a") = the model calling `ask_user` again, bounded by the budget. The prompt must teach this distinction (`AU-T02`).
- **Serve attendedness must be a real runtime fact** (is the receive loop running for *this* thread?); when fuzzy, **fail closed to no-ask**.
- **Restart drops a parked batch** (resumes headless by design — §3.10).
- **Labels that look like integers / contain commas** are disambiguated by quoting in the `tool_result` and by index→label lookup (never label-text matching); unique-label validation at decode time backs this.
- **Sub-agent → human asks** are out of scope for phase 1 (workers use `ask_supervisor`).

---

## 8. Definition of done (per `CLAUDE.md` §5)

Each `AU-T##` is Done only when: acceptance criteria met · `make verify` green · no §2 invariant violated and changes stay within `Owns` · `docs/ARCHITECTURE.md` updated in the same serialized PR if an interface changed (`AU-T06`) · a `CHANGELOG.md` entry added · a PR opened against `main`. Merge is the gate.
