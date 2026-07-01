# NilCore — One Conversational Front Door (Session · Steer/Queue · Auto-Route)

> **Status: SHIPPED.** The conversational front door is implemented and wired —
> `cmd/nilcore/chat.go` is a live REPL over `internal/session` (router + four drivers + inbox + emit),
> with queue/steer mid-work messaging. A later workstream added what the original design did not: a
> **user-set mode** (`/discuss` `/plan` `/execute` `/auto`) that overrides the auto-router and is
> enforced structurally (read-only registry + `backend.Native.DisableShell` for the read-only modes),
> read-only **context attach** (`/add <path>`, the `ReadRoots` tool seam), and sandboxed **web access**
> (`/add <url>` + `web_fetch` behind the `-allow-egress` allowlist proxy). See §"Modes, context, and
> web" below and the CHANGELOG. The sections that follow describe the original auto-route design
> (still the behavior in `ModeAuto`, the default).
>
> Produced by a grounded multi-agent design pass
> (4 architects → adversary → synthesis → plan), validated against the codebase and the seven
> invariants. Adds a single conversational entrypoint where the agent infers mode from the goal and
> the user can queue or steer messages mid-work. Nothing changes the frozen `backend.CodingBackend`
> contract — the loop seams are additive and gated (nil = byte-identical), like `Advisor`/`Peer`.

# NilCore — One Conversational Front Door (Session + Interruptible Loop + Steer/Queue)

## 1. Overview

Today the **user** picks the mode: `nilcore -goal` (one native task), `nilcore build` (supervisor/project), `nilcore serve` (chat, one-message-one-task, no mid-task input). This design adds **one conversational front door**: the user opens a chat — interactive terminal (`nilcore chat`) or an existing serve channel — and just talks, from a typo fix to "plan and ship a whole service." The harness decides internally which machine runs (native loop / supervisor / project loop) and the conversation **persists**: a follow-up continues the work, it does not restart it.

The new capability is **mid-work messaging**, in two modes:
- **QUEUE** (default): the message waits, then is folded in as an ordinary user turn at the next loop boundary.
- **STEER**: the message cancels the in-flight **model call** immediately, the loop unwinds to the boundary, and the steer text is folded in as the next user turn — so the user can correct course after reading the agent's live reasoning.

Three new leaf packages — `internal/session` (state + router + drivers), `internal/inbox` (the user-message seam), `internal/emit` (live reasoning out) — compose existing packages without touching the frozen `backend.CodingBackend` contract. Everything stays inside the seven invariants.

The single load-bearing trust line, enforced in code: **a principal's steer/queue message becomes a real (un-`guard.Wrap`'d) `user` turn; everything the loop reads from a tool / file / peer / bus stays `guard.Wrap`'d as data.** Authorization at the channel boundary is the *only* thing that promotes a chat message to principal-trust.

---

## 2. Mapping to existing packages

| New work | Anchored in |
|---|---|
| `internal/session` (Session, Router, Driver, Sink) | composes `agent`, `backend`, `super`, `project`, `summarize`, `memory`, `eventlog`, `model`, `policy`, `channel` |
| `internal/inbox` (`*Box`, `Push`/`Drain`/`Steer`) | mirrors `super/reader.go`'s mutex-guarded queue + goroutine lifecycle |
| `internal/emit` (Emitter, Event) | gated like `Advisor`/`Peer` on `backend.Native` (native.go:46-58,116) |
| Native loop inbox+emit seam | `backend/native.go:134` loop; QUEUE drains where it folds the next user turn |
| Supervisor loop inbox+emit seam | `super/super.go:152` loop; drains beside `drainFindings` (super.go:166) |
| Drivers | `agent.Orchestrator.executeSingle` (orchestrator.go:144), `super.Supervisor.Run` (super.go:124), `project.Loop.Run` (project.go:190) |
| Router classifier | reuses the `route.Review`/`summarize.Summarize` `Complete`→`firstText`→brace-extract pattern (route.go:83-103) and `agent.ShouldSupervise` (orchestrator.go:73,110) |
| Serve reuse | `server.Server` (server.go) gains a per-thread Session map + a concurrent intake goroutine; `channel.Authorized.Permit` (authorized.go:35) gates every message |
| Persistence | `agent.Checkpoint` single-`UpsertTask` write into `store.Task.Detail` (durability.go:180-185) |

No new module dependency (I6): every new package is stdlib-only plus internal imports. `Task`/`Result`/`CodingBackend` are untouched (I1).

---

## 3. The Session + Auto-Router

### 3.1 `internal/session` — the state container

```go
type Session struct {
    ID     string              // "chat-local" or the serve threadID
    Sender string              // pinned from the FIRST authorized request
    Repo   string

    History []model.Message    // canonical turns — the SAME shape native/super build
    State   WorkState          // bounded carry-over (never raw transcripts)

    Inbox   *inbox.Box         // user→agent seam (drained by the running loop)
    Router  Router
    Drivers Drivers
    Out     emit.Emitter       // reasoning/intent sink (terminal stdout / channel)

    Log    *eventlog.Log
    Mem    *memory.Memory      // optional, nil-safe
    Budget *budget.Ledger      // CONVERSATION-scoped (see §6)
    Meter  func(task string) model.Provider // metered provider factory, keyed by s.ID

    mu    sync.Mutex           // guards Phase + History
    Phase Phase
}

type WorkState struct {
    Summary     summarize.ContextSummary // reuses project.State's discipline
    Active      Route                    // which driver currently/last owned the work
    Branch      string                   // integration tip when project/super mid-flight
    LastOutcome string                   // data-only tail of the last terminal result
}
```

`History` is the literal `[]model.Message` the native loop builds (native.go:129) and the supervisor builds (super.go:149). **This is "continue, not restart":** a follow-up appends a `user` turn to `History` and either feeds the in-flight drive's `Inbox`, or — if idle and the router says continue — re-enters the same driver seeded with `History`+`State`. A genuinely new goal is the explicit branch (§3.5).

### 3.2 Phase machine

```
Idle ─msg─▶ Routing ─route─▶ Working ─(loop finishes)─▶ Terminal ─fold─▶ Idle
                                 │  message while Working:
                                 │    QUEUE → Inbox, drained at next boundary (stays Working)
                                 │    STEER → Inbox+steer, cancels model call now (stays Working)
                                 └─ AwaitingGate (driver blocked on Approver) ─answer─▶ Working
```

Only the `Working → Inbox` edges are new. The `Idle→Working→Terminal` spine is exactly today's orchestrator/loop flow, wrapped to be re-enterable.

### 3.3 `Turn` — the single entry point

```go
func (s *Session) Turn(ctx context.Context, text string) error
```

Decided under `s.mu`:

1. **`Phase == Working`** — append the user turn to `History`, log `session_followup{mode,phase}`, `s.Inbox.Push(userMsg, classifyInterrupt(text))`. The running loop drains it. `Turn` returns immediately; the drive continues.
2. **`Phase == Idle`** — `route := s.Router.Route(ctx, text, s.State)`; log `session_route`; launch the chosen driver in a goroutine bound to the drive context, `Inbox` wired in; `Phase = Working`. On completion the driver folds its terminal result into `s.State` (via `summarize`) and sets `Phase = Idle`.

`classifyInterrupt` (queue-vs-steer) is a **local default-QUEUE rule, no LLM round-trip** — STEER only when the message is prefix-marked (`!` / `/steer`) (a Telegram inline Steer button is a planned enhancement, not yet built). Immediacy is the whole point of steering. `Router.Route` (which machine, for a NEW drive) and `classifyInterrupt` (queue-vs-steer, for an IN-FLIGHT drive) never run on the same message.

### 3.4 The Router

```go
type Route int
const ( RouteContinue Route = iota; RouteNative; RouteSupervise; RouteProject; RouteChat )

type Router interface { Route(ctx context.Context, text string, st WorkState) (Route, error) }

type SupervisorFirstRouter struct {
    Classifier      model.Provider          // the METERED provider (same conversation ledger)
    ShouldSupervise func(goal string) bool   // REUSED from agent wiring (orchestrator.go)
}
```

**One cheap executor-tier classifier call** (JSON-out, ~256 tokens), parsed with the same defensive `firstText`+brace-extraction `route.go:89-96`/`summarize.go:82-85` use, returns `{route, reason}`. The native-vs-feature-vs-project sizing is **the model classifier's call** — a parseable proposal (sized by the work, not the wording, via a capability+cost manifest: `chat` < `native` < `supervise` < `project`) is **honored as-is**. The `agent.ShouldSupervise` heuristic is now ONLY the **fallback** for unparseable / no-classifier output (see the mitigation below) — it never overrules a proposal the model could parse. `RouteChat` answers "what are you working on?" without any loop; `RouteContinue` (the persistence requirement) is detected here when the message references `State.Summary.Goal`.

**Mitigations adopted (adv #9):**
- The classifier MUST use the **metered** provider (`s.Meter(s.ID)`), never a raw provider — its spend counts against the conversation ceiling (§6).
- On unparseable output, **fall back to the pure-function `ShouldSupervise` over the goal text** (no model call), not silently to `RouteNative`. Log `session_route{route, reason_len}`.
- The principal's own message is trusted input — do **not** fence it as data for the classifier; only tool/file/web content the classifier transitively sees needs fencing (I7).
- Routing is a **dispatcher, never a rail**: native / supervisor / project all terminate in the same verifier (I2) and the same conversation budget ceiling. A mis-route can only cost money (bounded by §6), never defeat a rail.

### 3.5 Drivers — Route → existing machinery, no new agentic logic

```go
type Driver interface { Drive(ctx context.Context, in DriveInput) (DriveResult, error) }
type DriveInput struct {
    Goal    string
    History []model.Message   // CONTINUE, not restart
    State   WorkState
    Inbox   *inbox.Box
    Out     emit.Emitter
}
type Drivers struct { Native, Supervise, Project Driver }
```

| Route | Driver | Reuses |
|---|---|---|
| `RouteContinue` | driver named by `State.Active` | re-enter with appended `History` |
| `RouteNative` | `nativeDriver` | `orchestrator.go:executeSingle` → `native.go` |
| `RouteSupervise` | `superviseDriver` | `super.go:Run` |
| `RouteProject` | `projectDriver` | `project.go:Run` |
| `RouteChat` | none — one metered `Complete` over `History`, no loop, no worktree | |

- **nativeDriver**: builds `backend.Task{ID: s.ID + "-" + seq, Dir: worktree}` and runs `executeSingle` (fresh worktree, `Native.Run`, final verify — I2 unchanged). The per-drive task ID is fine for the worktree/eventlog but **MUST NOT be the budget key** (§6). Continuation: it passes `History` as the loop's seed (see §4 `Seed`).
- **superviseDriver**: `super.Supervisor.Run(ctx, goal)`. The user inbox is a *second* concurrent source folded at the round boundary alongside `drainFindings` (super.go:166).
- **projectDriver**: `project.Loop.Run(ctx)`. `WorkState.Summary` seeds the loop's initial `summarize.ContextSummary` (project.go:196), closing the whole-project continue path.

A new goal (unrelated to the active one) is the explicit non-continue branch: `summarize` the finished work into `State.LastOutcome`, archive prior `History` (kept in the event log, I5), start a fresh `WorkState`.

---

## 4. The interruptible loop (queue + steer)

The concurrency core: an **additive, nil-gated** `Inbox` seam on the native AND supervisor loops where `nil` = byte-identical (gated exactly like `Advisor`/`Peer`).

### 4.1 The seam type (`internal/inbox`, stdlib-only)

A new leaf package so neither `backend` nor `super` gains an import into channel/session machinery (mirrors why `Peer` is an interface in native.go:71). Define the **concrete** type in `inbox`, a **local interface** in each consumer:

```go
// internal/inbox
type Mode int; const ( Queue Mode = iota; Steer )

type Box struct {
    mu     sync.Mutex
    queued []model.Message
    steerC chan struct{}     // buffered cap-1, edge-notify
    log    *eventlog.Log
    label  string
}
func New(log *eventlog.Log, label string) *Box { return &Box{steerC: make(chan struct{},1), log:log, label:label} }

func (b *Box) Push(m model.Message, mode Mode) {
    b.mu.Lock(); b.queued = append(b.queued, m); b.mu.Unlock()
    b.log.Append(eventlog.Event{Task: b.label, Kind: "user_message",
        Detail: map[string]any{"mode": modeStr(mode), "len": textLen(m)}}) // metadata only (I5/I7)
    if mode == Steer { select { case b.steerC <- struct{}{}: default: } }   // never blocks the producer
}
func (b *Box) Drain() []model.Message {
    b.mu.Lock(); defer b.mu.Unlock()
    if len(b.queued)==0 { return nil }
    out := b.queued; b.queued = nil; return out
}
func (b *Box) Steer() <-chan struct{} { return b.steerC }
```

In `backend`: `type Inbox interface { Drain() []model.Message; Steer() <-chan struct{} }`, a nil-able `Inbox` field plus a nil-able `Seed []model.Message` field on `Native` (both gated, additive — I1 untouched). `super` defines the same local interface. `*inbox.Box` satisfies both; `inbox` never enters `backend`'s import graph.

**Race-freedom:** `queued` is the only shared mutable state, fully `mu`-guarded on both `Push` (producer) and `Drain` (consumer). `steerC` is a cap-1 edge-notify. The loop goroutine is the single owner of `msgs`/`History` (the same single-owner discipline as `runState`, super.go:227). No data race.

### 4.2 Steer cancels the MODEL CALL only — never an in-flight tool (adv #2, Facet D wins)

This is the decisive synthesis call. **The per-iteration cancellable context wraps ONLY `Model.Complete` (and cheap read-only bus Asks). `Box.Exec` / `Tools.Dispatch` / `Peer.Dispatch` / `Verifier.Check` receive the TASK ctx, never the iter-ctx.** A steer that arrives mid-tool is buffered and applied at the *next* loop boundary; it does **not** kill a running container.

Why (grounded): `sandbox.go:129` uses `exec.CommandContext` over `/work` bind-mounted **read-write** (sandbox.go:117). A SIGKILL mid-`git commit` / `go build -o` / shell `>` redirect would leave partial **host** state (`--rm` only cleans the container, not `/work`). And `writeNoFollow` (fs.go:68-81) is `O_TRUNC` + `Write` — **two syscalls, not atomic** (Facet B's "atomic single-syscall" claim is false). Confining steer to the model call (pure compute, zero disk effect) makes **"no half-applied tool state" true by construction** and eliminates Facet B's entire tool-phase-steer / transcript-integrity subsection — there is never a dangling `tool_use` because tools always run to completion.

**Prerequisite fix (adopt, correct independent of steer):** make `writeNoFollow` atomic — write to a temp file in the same dir, `os.Rename` (atomic on POSIX) — so even an OS-level kill of the *harness* leaves no torn file. A separate explicit `/abort` verb (not steer) is the only thing that may cancel a running sandbox command; it is documented as potentially leaving the **disposable** worktree dirty (acceptable: the tree is thrown away and never reaches gated `main`).

### 4.3 The loop seam (native; supervisor identical)

```go
for i := 0; i < steps; i++ {
    // QUEUE drains FIRST, as user turns (where super.drainFindings folds, super.go:166)
    if n.Inbox != nil { for _, m := range n.Inbox.Drain() { msgs = append(msgs, m) } }

    // nil Inbox ⇒ EXACTLY today's path: iterCtx is ctx, no WithCancel, no watcher (adv #11)
    iterCtx := ctx
    var cancel context.CancelCauseFunc
    var watcher chan struct{}
    if n.Inbox != nil {
        iterCtx, cancel = context.WithCancelCause(ctx)
        watcher = make(chan struct{})
        go func() {                                  // lifecycle copied from super/reader.go
            select {
            case <-n.Inbox.Steer(): cancel(errSteer)  // sentinel cause distinguishes steer
            case <-iterCtx.Done():                     // iter ended on its own → watcher exits, no leak
            }
            close(watcher)
        }()
    }

    resp, err := n.Model.Complete(iterCtx, systemPrompt, msgs, toolDefs, 4096)

    if cancel != nil { cancel(nil); <-watcher }      // deterministic teardown EVERY iter → no leak

    if err != nil {
        switch classifyCancel(ctx, iterCtx) {        // SHARED helper, identical in both loops
        case cancelShutdown:                          // taskCtx died (SIGTERM/deadline) — DOMINATES
            return Result{Backend: n.Name(), Summary: "interrupted: " + ctx.Err().Error()}, nil
        case cancelSteer:                             // a steer — NOT an error; drain+continue
            n.Log.Append(eventlog.Event{Task: t.ID, Kind: "steer_interrupt",
                Detail: map[string]any{"step": i, "phase": "model"}})
            continue                                  // next iter's Drain() folds the steer text
        default:                                      // genuine transport fault
            return Result{Backend: n.Name()}, fmt.Errorf("model step %d: %w", i, err)
        }
    }
    // ... existing dispatch (assistant turn, Box.Exec under TASK ctx, finish→Verifier.Check) ...
}
```

```go
// shared, so native.go and super.go cannot drift (adv #3, #4)
func classifyCancel(taskCtx, iterCtx context.Context) cancelKind {
    if taskCtx.Err() != nil { return cancelShutdown }                 // (1) shutdown STRICTLY dominates
    if errors.Is(context.Cause(iterCtx), errSteer) { return cancelSteer } // (2)
    return cancelFault                                                // (3)
}
```

**Discriminator (adv #3, #4): `context.WithCancelCause` + `context.Cause` (Go 1.21+ stdlib, I6-clean), evaluated in fixed precedence — taskCtx.Err() FIRST.** If a SIGTERM and a steer race, shutdown wins (the bounded-loop / clean-shutdown rail is never overrun). No shared mutable `steered` bool exists anywhere — the cause travels inside the context, so `go test -race` is green by construction (a hard acceptance criterion). The steer-cause check sits **before** native.go's existing `fmt.Errorf("model step…")` path so a steer is never mistaken for a fatal fault.

### 4.4 Supervisor cascade caveat

The supervisor's iter-ctx wraps **only** `s.Model.Complete` and short read-tool calls. `doSpawn` / `doCode` / `doIntegrate` / `doAwait` get the **original task ctx**, so steering the planner never kills an in-flight subagent. Aborting a worker stays the bus's existing **supervisor-only** `KindCancel` path (bus message.go:43-44,61-63) — already authority-checked. This cleanly separates "steer the planner" from "cancel a worker."

**Deterministic fold order at the round boundary (adv #10):** drain both queues, then emit the user QUEUE message(s) as **un-`Wrap`'d trusted user text FIRST** (labeled "principal instruction") and the subagent findings as **`guard.Wrap`'d DATA SECOND** (reusing `drainFindings`'s rendering verbatim) — two distinct labeled blocks, never concatenated. This preserves the trust line even when a user message and a finding arrive in the same gap.

### 4.5 nil = byte-identical (adv #11)

When `Inbox == nil`: `iterCtx := ctx` directly (no `WithCancelCause`, no allocation, no watcher goroutine, no `Drain`), guarded by a single `if n.Inbox != nil` exactly like `if n.Peer != nil` (native.go:116). Acceptance: a golden-transcript test with nil Inbox, plus an alloc/goroutine-count assertion on the nil path.

---

## 5. Reasoning surfacing + two-mode UX

### 5.1 `internal/emit` — the output seam (stdlib-only leaf)

`backend` (a frozen leaf) must not import `channel`, so a brand-new stdlib leaf both can import:

```go
type Kind string
const ( KindReasoning Kind="reasoning"; KindIntent Kind="intent"; KindResult Kind="result"; KindStatus Kind="status" )
type Event struct { Kind Kind; Step int; Text, Detail string }
type Emitter interface{ Emit(ctx context.Context, e Event) }
type Func func(context.Context, Event)
func (f Func) Emit(c context.Context, e Event){ f(c,e) }
type Multi []Emitter   // fan-out: fast local sink sync, channel sink async
```

A nil `Emitter` ⇒ the loop is byte-identical (gated like `Advisor`/`Peer`). **Per-step intent for v1; token streaming is a documented follow-on** — `model.Provider.Complete` is non-streaming (model.go:54-59) and both adapters do one `http.Do`; SSE would touch the provider seam every loop reads as stable. The model already emits `text` blocks alongside `tool_use` (appended verbatim native.go:143) — surfacing them is free.

### 5.2 Emission points

**Native (native.go, new `Emitter` field):** after `Complete`, before dispatch, emit `KindReasoning` for each `text` block (the steer surface); inside the block switch, **before** the side effect, emit `KindIntent` (`run` → `"about to run: "+clip(cmd,80)` immediately before `Box.Exec` native.go:180; `finish` → `"declaring done — verifier will judge"`); after, emit `KindResult` (`exit N`, `verify passed/failed`). **Supervisor:** same placement in `dispatch` (`doSpawn`→"spawning <role>", `doCode`→"writing code", `doIntegrate`→"integrating N branches").

**Mitigations (adv #6, #8):**
- **The channel Emitter sink is non-blocking from the loop's perspective.** `Emit` is synchronous only for the terminal/stdout and eventlog-mirror sinks (fast, local). The **channel** sink pushes into a buffered, coalescing, drop-oldest queue drained by a **separate per-thread sender goroutine**, so the loop never blocks on Telegram/Slack HTTP (rate limits, `429` retry-after). The send path and the receive/intake path share **no lock** — the steer that unblocks must always be deliverable. Coalesce reasoning+intent+result of one step into ≤1 chat message.
- Surface **harness-authored intent lines** (`about to run: <clipped cmd>`), preferring the structured `KindIntent` over dumping `resp.Content` text wholesale, so laundered tool output can't ride into the user's view verbatim.
- Run every surfaced string through the eventlog `redact` path before it leaves the process, and mirror each emit as a **metadata-only** `eventlog.Event{Kind:"surface", Detail:{surface_kind,step}}` — never the body. In serve, `Update` already targets one `threadID`; intent never broadcasts.

### 5.3 Terminal REPL (`internal/chat`, the primary front door)

A `bufio.Scanner` reader goroutine on `os.Stdin` runs **while the agent works** — line-based, no raw mode for v1 (reuse the stdlib `stty` `echoOff` pattern in `onboard/wizard.go`; no `golang.org/x/term`, I6). Convention:

```
<text>+Enter      → QUEUE  (default; delivered at next boundary)
!<text> / /steer  → STEER  (immediate; cancels the in-flight model call)
/status, /quit    → session control
```

On accept, immediately emit `KindStatus`: `queued: "<text>" (delivered after this step)` or `steering — interrupting current step…`, so the user always knows which mode was understood. Agent lines are prefixed (`· reasoning`, `→ intent`, `✓ result`) to read cleanly interleaved with typed input. The reader exits on EOF (Ctrl-D) or session-ctx cancel — no leak. An Esc-hotkey single-key steer is a raw-mode follow-on.

### 5.4 Serve channels (Telegram / Slack)

Normal message = **QUEUE**; an `!`/`/steer` **prefix** = **STEER** (the shipped trigger). The channel Emitter sink is a thin adapter over `Channel.Update`, coalesced. _(A Telegram inline "🛑 Steer" button — reusing the existing inline-keyboard + authorized-callback plumbing to arm steer on a thread's next message — is a planned enhancement, not yet built; the prefix trigger is the live path.)_

**Per-message authorization in a dedicated intake goroutine (adv #5 — load-bearing):** today `Authorized.Receive` (authorized.go:43) is a blocking one-at-a-time loop consumed by `server.Serve`'s outer loop (server.go:35), which is busy inside `Run` for the whole current task and not calling `Receive` again — so it **cannot** deliver a mid-task message. The new design therefore:
1. Runs a **separate per-thread intake goroutine** that reads the channel and calls `Authorized.Permit(req.Sender)` on **every** message (queue AND steer) before `Inbox.Push`. An unauthorized message is dropped + logged `unauthorized_steer`/`unauthorized_command` and never reaches the loop. The intake goroutine owns a `Permit`-based filter — it does **not** reuse `Authorized.Receive` (that would steal requests from the outer loop).
2. Pins `Session.Sender` from the **first** authorized request; `Turn` refuses any message whose sender ≠ `Session.Sender` (a thread can be reached by multiple senders).
3. Verifies the concrete `channel.Channel` impls are safe for one goroutine calling `Receive` while another calls `Update` on the same thread (Telegram long-poll vs send-message); if not, serialize sends through a transport mutex — **distinct** from any lock the intake path holds.

---

## 6. Safety / invariant envelope

- **I1 frozen contract:** `Inbox`/`Seed`/`Emitter` are new optional struct fields on `Native`/`Supervisor`, gated like `Advisor`/`Peer`; nil = byte-identical. `Run(ctx, Task)(Result, error)` is never touched.
- **I2 verifier sole authority:** every driver's done-ness stays the verifier's (`executeSingle` re-verifies; `super.Run`/`project.Run` gate `Done` on the verifier). Steer is just more user text — it **cannot** set `finished=true` or shortcut verify; the model must still call `finish`. No "user says done → ship" path exists. `RouteChat` does no work, so nothing to verify.
- **I3 / I4 no ambient authority / sandboxed:** steer is **text into the model's context only**; it carries no executable payload. The loop's only executor stays `Box.Exec` in the hardened container (`--network none`, `--cap-drop=ALL`, RO rootfs). The classifier carries no secrets. Serve-mode steer is admitted **only after** per-message `Permit`.
- **Gate unchanged:** a steered "push to prod" still reaches `policy.Gate` → `Approver` (chat `GuardedApprove`, re-authorized). Steer does **not** pre-authorize a gate; the principal still gets one explicit prompt. `AwaitingGate` routes through the existing approver.
- **Budget — conversation-scoped (adv #1, BLOCKER):** the budget Ledger keys by an opaque task string and the **per-task** ceiling resets per key (`Charge`, budget.go:89-94); only `gceiling` is conversation-wide. A per-drive task ID (fine for worktree/eventlog) **must not** be the budget key. So: **the Session owns ONE `meter.Provider.Task = s.ID` reused across every drive**, AND the wiring calls `Ledger.SetGlobalCeiling` at session construction as the conversation wall. The router's classifier uses that same metered provider. Acceptance test: N back-to-back continue-drives hit `budget.ErrCeiling` at the conversation ceiling, not N×ceiling.
- **Steer storm bounded (adv: R6):** steer never resets the dollar/token budget or the ctx deadline. It MAY grant a bounded `+k` (k≈10) step credit under an absolute `MaxSteps` ceiling and a per-conversation `MaxSteers`; past that it is accepted as a message granting no steps. Rapid steers **coalesce** (drain-all batches into one delivered user turn, one model call) → log `steer_coalesced{count}`. The dollar ceiling and `WithDeadline` wall are the storm-proof backstops; the step counter `i` is never reset.
- **I5 append-only:** all new kinds metadata-only + redacted; bodies never logged.
- **I7 untrusted-as-data:** the steer is the *principal's* trusted instruction → un-`Wrap`'d user turn; everything from a tool/file/peer/bus stays `guard.Wrap`'d. **Fencing is immutable once applied** — a steer is a NEW user turn and MUST NOT cause the harness to un-fence or "merge" any previously-`Wrap`'d data in `History`. Authorization at the channel is the ONLY promotion to principal-trust.
- **Persistence (adv #7, mandatory for serve):** a SIGTERM mid-conversation must not silently become "restart." Checkpoint the Session crash-atomically via the existing single-`UpsertTask` write into `store.Task.Detail`: (a) the bounded `summarize.ContextSummary` work-state (never raw transcripts), (b) the current `Route`/`Active` driver, (c) any **undrained** queued inbox messages. `Checkpoint.Resume` reconstitutes the Session and **re-delivers undrained queued messages** before continuing, rebuilding the worktree from the last **verified** tip (`RunState.TipSHA`), never a torn tree. For `nilcore chat` durability is optional (the user is present); for serve it is required. If full `History` is too large, persist the bounded summary as the seed and document the lossy-but-continuous resume.

### New event-log kinds (metadata only, redacted)
`session_open{id,sender,repo}` · `session_turn{phase,len_text}` · `session_route{route,reason_len}` · `session_followup{mode,phase}` · `session_continue{driver}` · `session_drive_start`/`session_drive_done{driver,route,verified,reason}` · `session_fold{decisions,remaining_len}` · `session_close` · `user_message{mode,len}` · `steer_interrupt{step,phase}` · `queue_drain{count}` · `steer_coalesced{count}` · `task_cancel{cause}` · `unauthorized_steer{sender}` · `surface{surface_kind,step}`. These layer **above** the loop kinds (`model_call`, `tool_exec`, `verify`, `super_*`, `project_*`) so the trail is replayable end-to-end. `Log.Append` is already mutex-safe + nil-safe (eventlog.go:86); the log's `Err()` halt-gate still applies.

---

## 7. CLI surface

- **`nilcore chat [-dir ./repo]`** — the primary front door: constructs one `Session` with a terminal `Sink`, the `internal/chat` stdin reader goroutine, a metered provider keyed by `s.ID`, and `SetGlobalCeiling` from `-budget`. No allowlist (the terminal user is the principal by construction; the Session records `principal:"local"`).
- **`nilcore serve -channel telegram`** reuses the SAME `Session`: `server.Server` gains a per-thread `map[threadID]*Session` and the concurrent per-thread intake goroutine (§5.4). Telegram/Slack thus get queue+steer. The empty-allowlist refusal (main.go:514-518) stays.
- **Default `bare nilcore` → `chat`** (today it prints usage; the conversational front door becomes the natural default). `nilcore -goal …` keeps the existing flag-prefixed dispatch (main.go:82-84) unchanged.
- **`run` / `build` / `serve` remain** for scripting/CI — `runMain` (one bounded native task) and `buildMain` (supervisor/project) are byte-identical (nil Inbox/Emitter).

Wiring sites: `runMain`/`serveMain`/`envFactory`/`buildStack` in `cmd/nilcore/{main.go,build.go}` — where the Session is constructed, `ShouldSupervise` supplied, and `meterProvider(prov, ledger, s.ID)` keyed by the conversation.

---

## 8. Worked end-to-end example

1. User runs `nilcore chat` and types **"build me a URL-shortener service with tests and a Dockerfile."** `Turn` (Idle): the metered classifier returns `{route:"project"}`; reconciled with `ShouldSupervise` → `RouteProject`. Logs `session_route{project}`; launches `projectDriver` with `Inbox`+`Emitter`; `Phase=Working`.
2. The project loop drives plan→slice; the supervisor's first round emits `· reasoning: I'll scaffold the HTTP handler first` then `→ about to run: go test ./...`. The user sees intent live in the terminal.
3. User types **"also add a rate limiter"** (plain Enter → QUEUE). `classifyInterrupt`→Queue; `Inbox.Push`; ack `queued: "also add a rate limiter" (delivered after this step)`. Logs `user_message{mode:queue}`. The drive continues; at the next round boundary `Drain()` folds it as a user turn → `queue_drain{count:1}`.
4. The next round emits `→ about to run: go build -o /tmp/bin ./cmd/shortener`. The user realizes the package path is wrong and types **`!the binary lives in ./service not ./cmd/shortener`** (STEER). Ack `steering — interrupting current step…`; `Inbox.Push(…, Steer)` pokes `steerC`. The watcher calls `cancel(errSteer)`; the in-flight `Model.Complete` returns; `classifyCancel` → `cancelSteer`; logs `steer_interrupt{step,phase:model}`; the loop continues. **The already-running `go build` (if one were mid-flight) is untouched** — steer only cancelled the model call; no half-applied state. Next boundary `Drain()` folds the steer as an un-`Wrap`'d user turn; the model corrects course.
5. The loop converges; the verifier passes; the single gated `PromoteToBase` prompts the user (Gate unchanged). `session_drive_done{driver:project,verified:true}`; `session_fold`; `Phase=Idle`.
6. User types **"now write a README"** — classifier returns `RouteContinue` (references the active goal); `nativeDriver` re-enters with the existing `History`+`State`. The conversation continued; it did not restart. A SIGTERM at any point checkpoints the bounded summary + route + undrained queue; restart re-delivers and continues from the verified tip.

---

### Critical Files for Implementation
- `/Users/mt/Programming/Schtack/NilCore/internal/backend/native.go` — add nil-gated `Inbox`/`Seed`/`Emitter` fields; per-iter `WithCancelCause` watcher + `classifyCancel`; emit reasoning/intent; QUEUE drain at the boundary.
- `/Users/mt/Programming/Schtack/NilCore/internal/super/super.go` — same seam beside `drainFindings` (super.go:166); iter-ctx wraps only `Model.Complete`, task ctx for spawn/code/integrate; deterministic user-first/findings-second fold.
- `/Users/mt/Programming/Schtack/NilCore/internal/super/reader.go` — the goroutine lifecycle (start-before-intake, `defer stop()`-that-waits) to copy for the inbox watcher.
- `/Users/mt/Programming/Schtack/NilCore/internal/tools/fs.go` — make `writeNoFollow` atomic (temp + `os.Rename`).
- `/Users/mt/Programming/Schtack/NilCore/internal/server/server.go` + `/Users/mt/Programming/Schtack/NilCore/cmd/nilcore/main.go` — per-thread `Session` map, concurrent per-message-`Permit` intake goroutine, `nilcore chat` entrypoint, and `meter.Provider` keyed by the conversation ID with `SetGlobalCeiling`.


---

## Implementation plan — phased task DAG

## Conversational Front Door — Implementation Task DAG

Convention matches `docs/TASKS.md`: one task = one branch (`task/<ID>`) = one PR, `Owns` sets disjoint across in-flight branches, lowest eligible ID first. Phase letters here are **C0–C4** ("Conversational") so these IDs never collide with the existing P0–P6 queue. `internal/backend/native.go`, `internal/super/super.go`, `internal/channel/channel.go`, `cmd/nilcore/main.go`, `docs/ARCHITECTURE.md`, `docs/TASKS.md`, `go.mod`, `Makefile` remain **contract files** — any task that edits one is serialized and noted.

The risky concurrency core (the interruptible loop + its seam type) is C0/C1 and must land green — with `go test -race` as a hard acceptance gate — before any session/CLI work (C2+) builds on it. Everything stays default-off: a nil `Inbox`/`Emitter` leaves `run`/`build`/`serve` byte-identical.

| ID | Phase | Goal | Depends-on | Owns | Acceptance |
|---|---|---|---|---|---|
| C0-T01 | C0 | **Atomic `writeNoFollow` (prerequisite fix, correct independent of steer).** Make the structured `write`/`edit` write atomic so an OS-level kill of the harness never leaves a torn file: write to a temp file in the same dir under `O_CREATE\|O_WRONLY\|O_EXCL\|O_NOFOLLOW`, then `os.Rename` (atomic on POSIX). Preserve the existing final-component symlink defense and worktree confinement. | — | `internal/tools/fs.go` (+ `internal/tools/tools_test.go` cases) | `make verify` green; a test that a write interrupted before `Rename` leaves the original file intact (no truncated content); symlink-escape test still passes; `O_NOFOLLOW` semantics preserved on the temp open; `go test -race ./internal/tools/` green. |
| C0-T02 | C0 | **`internal/emit` — live reasoning/intent sink (new leaf, stdlib-only).** Define `type Event struct { Kind, Text string; Step int }` and `type Emitter interface { Emit(Event) }`, plus a stdout `WriterEmitter` (terminal) and a `nil`-safe no-op. Emit kinds: `intent` (per-step model intent line), `tool` (what tool is about to run), `verify`, `steer_ack`. No imports beyond stdlib; the loop holds a nil-able `Emitter` so `nil` = no allocation, no emit. | — | `internal/emit/` | `make verify`; unit test that `WriterEmitter` renders each kind on one line; nil Emitter is a safe no-op; package imports stdlib only (verified by `go list -deps`). |
| C1-T01 | C1 | **`internal/inbox` — the user-message seam (new leaf, stdlib-only).** Implement the design's `Box`: `mu`-guarded `queued []model.Message`, cap-1 buffered `steerC chan struct{}`, `New(log,label)`, `Push(m, Mode)`, `Drain() []model.Message`, `Steer() <-chan struct{}`, with `Mode` = `Queue\|Steer`. `Push` logs `user_message` with metadata only (mode + text length, never the body — I5/I7) and does a non-blocking `select` send on `steerC` for Steer. Imports: `sync`, plus `internal/model` + `internal/eventlog` only. | C0-T02 | `internal/inbox/` | `make verify`; `go test -race` green under concurrent `Push`/`Drain`; `Push(Steer)` never blocks even when `steerC` already signaled (cap-1 edge-notify); `Drain` returns and clears atomically; logged event carries no message body. |
| C1-T02 | C1 | **Shared cancel-classifier + sentinel (new tiny leaf).** `internal/loopctl` with `var errSteer = errors.New(...)`, `type cancelKind` (`cancelShutdown\|cancelSteer\|cancelFault`), and `func ClassifyCancel(taskCtx, iterCtx context.Context) cancelKind` evaluating in fixed precedence: `taskCtx.Err()!=nil` ⇒ shutdown FIRST; else `errors.Is(context.Cause(iterCtx), ErrSteer)` ⇒ steer; else fault. Export `ErrSteer`. Stdlib only. This is the single source both loops import so native and super cannot drift. | — | `internal/loopctl/` | `make verify`; table test covering all three precedence cases incl. the shutdown-vs-steer race (shutdown wins); no shared mutable state; stdlib-only deps. |
| C1-T03 | C1 | **Interruptible native loop seam (CONTRACT — serialized).** Add two nil-able additive fields to `backend.Native`: `Inbox` (local interface `{ Drain() []model.Message; Steer() <-chan struct{} }`, satisfied by `*inbox.Box`; declared in `backend`, not imported, mirroring `Peer`) and `Seed []model.Message`. At the top of each loop iteration: `Drain()` queued msgs and append as user turns; if `Inbox==nil` keep `iterCtx := ctx` (no `WithCancelCause`, no watcher — byte-identical); else `iterCtx,cancel := context.WithCancelCause(ctx)` + a watcher goroutine (lifecycle copied from `super/reader.go`) that `cancel(loopctl.ErrSteer)` on `Steer()`. Wrap ONLY `Model.Complete` in `iterCtx`; `Box.Exec`/`Tools.Dispatch`/`Peer.Dispatch`/`Verifier.Check` keep the TASK ctx. After Complete: `cancel(nil); <-watcher` every iter (deterministic teardown). On err, switch on `loopctl.ClassifyCancel`: shutdown ⇒ return clean interrupted Result; steer ⇒ log `steer_interrupt{step,phase:"model"}` and `continue` (next Drain folds the steer); fault ⇒ existing `model step %d` error path. Seed `msgs` from `Seed` when non-nil. | C1-T01, C1-T02 | `internal/backend/native.go` | `make verify`; **`go test -race ./internal/backend/` green (hard gate)**; with `Inbox==nil` the loop is byte-identical (golden-transcript test vs current behavior, no extra goroutine — assert via a fake provider call-count + a goroutine-leak check); a steer mid-`Complete` cancels that call, is reclassified as steer (not fault), and the queued text appears as the next user turn; a `tool` phase is never cancelled by steer (in-flight `Box.Exec` runs to completion); SIGTERM/deadline dominates a simultaneous steer; no watcher goroutine leak across iterations. **Updates `docs/ARCHITECTURE.md` §I1 note in the same serialized PR** to register the additive `Inbox`/`Seed` gate alongside `Advisor`/`Peer`. |
| C1-T04 | C1 | **Interruptible supervisor loop seam (CONTRACT — serialized).** Mirror C1-T03 in `super.Supervisor.Run`: nil-able `Inbox` (same local interface) + `Out emit.Emitter`. Drain user QUEUE messages at the round boundary **beside** `drainFindings`, with the deterministic fold order: user message(s) as **un-`Wrap`'d trusted principal text FIRST** (labeled "principal instruction"), subagent findings as `guard.Wrap`'d DATA SECOND — two distinct labeled blocks, never concatenated. iter-ctx wraps ONLY `s.Model.Complete` (+ short read-tool calls); `doSpawn`/`doCode`/`doIntegrate`/`doAwait` keep the task ctx (steering the planner never kills a worker — worker cancel stays the bus `KindCancel` path). Use the SAME `loopctl.ClassifyCancel`. nil Inbox = byte-identical. | C1-T01, C1-T02, C0-T02 | `internal/super/super.go` | `make verify`; `go test -race ./internal/super/` green; nil-Inbox byte-identical (golden test); a QUEUE message and a finding arriving in the same gap produce two correctly-labeled, correctly-trust-fenced blocks; a steer cancels only the planner's `Complete`, never an in-flight `doSpawn`; shutdown dominates steer. **Updates `docs/ARCHITECTURE.md` in the same serialized PR.** |
| C2-T01 | C2 | **`internal/session` core: `Session`, `WorkState`, `Phase`, `Turn` (state container only — no router/drivers yet).** Implement `Session{ID,Sender,Repo,History []model.Message,State WorkState,Inbox *inbox.Box,Out emit.Emitter,Log,Mem,Budget,mu,Phase}` and `WorkState{Summary,Active,Branch,LastOutcome}`. `Turn(ctx,text)` decided under `mu`: if `Phase==Working` append user turn to `History`, log `session_followup{mode,phase}`, `Inbox.Push(userMsg, classifyInterrupt(text))`, return; if `Phase==Idle`, set `Phase=Routing` and hand off to a `run(route)` callback (injected — Router/Drivers land in C2-T02/03). `classifyInterrupt` = local default-QUEUE rule (Steer only on `!`/`/steer` prefix), no LLM call. Phase machine `Idle→Routing→Working→Terminal→Idle` with `AwaitingGate`. | C1-T01 | `internal/session/session.go`, `internal/session/state.go` | `make verify`; `go test -race`; `Turn` while Working never blocks and pushes to Inbox with correct mode; `classifyInterrupt` table test (prefix ⇒ Steer, else Queue); Phase transitions guarded by `mu`; History grows monotonically (continue-not-restart); no goroutine leak when a drive goroutine completes and flips Phase back to Idle. |
| C2-T02 | C2 | **`internal/session` Router (`SupervisorFirstRouter`).** `Route` enum (`RouteContinue\|RouteNative\|RouteSupervise\|RouteProject\|RouteChat`) + `Router` interface + `SupervisorFirstRouter{Classifier model.Provider, ShouldSupervise func(string)bool}`. One cheap metered classifier call (JSON-out ~256 tok) parsed with the defensive `firstText`+brace-extract pattern from `route.go`/`summarize.go`; on unparseable output fall back to the pure-function `ShouldSupervise` (no model call) — never silently `RouteNative`. `RouteContinue` detected when the message references `State.Summary.Goal`. Log `session_route{route,reason_len}`. Classifier MUST be the metered provider; principal text is trusted (not fenced), only tool/file content it transitively sees is fenced. | C2-T01 | `internal/session/router.go` | `make verify`; tests with a fake provider returning known JSON for each route; unparseable output falls back to `ShouldSupervise` (asserted, not to NativE by default); a continue-referencing message yields `RouteContinue`; `RouteChat` triggers no loop; logged event carries no message body. |
| C2-T03 | C2 | **`internal/session` Drivers — Route → existing machinery (no new agentic logic).** `Driver` interface + `DriveInput{Goal,History,State,Inbox,Out}`/`DriveResult` + `Drivers{Native,Supervise,Project}`. `nativeDriver` builds `backend.Task{ID:s.ID+"-"+seq,Dir:worktree}`, runs the orchestrator's single-task path (fresh worktree, `Native.Run` with `Inbox`+`Seed:=History`, final verify), and folds the terminal result into `WorkState` via `summarize`; the per-drive task ID is the worktree/eventlog key but **MUST NOT be the budget key** (conversation ID is). `superviseDriver` wraps `super.Supervisor.Run` with the user Inbox as a second concurrent source. `projectDriver` wraps `project.Loop.Run`, seeding `State.Summary` into the loop's initial `ContextSummary`. `RouteChat` = one metered `Complete` over `History`, no loop/worktree. A new (unrelated) goal summarizes finished work into `State.LastOutcome`, archives prior `History` (kept in the log), starts a fresh `WorkState`. | C2-T01, C2-T02, C1-T03, C1-T04 | `internal/session/drivers.go` | `make verify`; `go test -race`; per-driver test with fakes asserting: native passes `History` as `Seed` and `Inbox` through, verifier remains final authority, budget key is the conversation ID (not the per-drive task ID); continue re-enters the driver named by `State.Active` with appended History; new-goal path archives + resets WorkState; RouteChat runs zero loops/worktrees. |
| C2-T04 | C2 | **Per-step intent emission wiring (live reasoning to steer on).** Wire `emit.Emitter` through the native + supervisor loops so each step emits an `intent` line (first text block of the assistant turn, or a short tool-about-to-run note) and a `steer_ack` when a steer is folded. Default `nil` Emitter = no output (byte-identical). Keep emission metadata-light; never emit secrets. (Token-streaming via provider streaming is explicitly OUT of scope here — a noted future enhancement; non-streaming per-step intent is sufficient to steer on.) | C1-T03, C1-T04, C0-T02 | `internal/backend/native.go`, `internal/super/super.go` (additive emit calls only) | `make verify`; nil-Emitter byte-identical (golden test unchanged); a wired Emitter receives one `intent` per step and a `steer_ack` after a steer fold; emitted lines contain no secret/credential patterns. **Serialized (touches two contract files); coordinate with C1-T03/T04 — ideally land emit calls in the SAME serialized PRs as C1-T03/T04 to avoid re-editing the contract files. If split out, this task is the serialized owner.** |
| C3-T01 | C3 | **Interactive `nilcore chat` REPL — the primary front door.** A line-based stdin REPL (stdlib `bufio`) that builds ONE `Session` (ID `chat-local`, Sender pinned to the local principal, Repo from `-dir`), wires a `WriterEmitter` to stdout for live reasoning, and feeds each input line to `Session.Turn`. While a drive is Working, a new line is queued/steered (default queue; `!`/`/steer` prefix steers); Ctrl-C is graceful shutdown (cancels the task ctx — shutdown dominates any pending steer). Reuses the existing boot/provider/worktree/verifier/budget wiring from `runMain`/`buildMain`. | C2-T03, C2-T04 | `cmd/nilcore/chat.go`, `cmd/nilcore/main.go` (add `case "chat"` + default-to-chat) | `make verify`; a scripted-stdin test driving a fake Session asserting: each line becomes a Turn; a line typed mid-drive is queued (delivered at the next boundary) and a `!`-prefixed line steers (cancels the in-flight model call); Ctrl-C shuts down cleanly with no goroutine leak; bare `nilcore` (no args) launches chat. **Serialized (touches `cmd/nilcore/main.go`).** |
| C3-T02 | C3 | **Serve mode gains the same Session (queue+steer over Telegram/Slack).** Refit `server.Server` with a per-thread `map[threadID]*Session` + a concurrent intake goroutine: `Receive` runs in its own goroutine and routes each authorized inbound message to that thread's `Session.Turn` (creating the Session on first message, pinning `Sender`). Every message is `Permit`-checked via `channel.Authorized` BEFORE it reaches `Turn` (steer/queue from an unauthorized sender is rejected+logged, never promotes to principal trust). `Update` carries `emit` lines back as progress; a Telegram "Steer" button maps to Steer mode. Clean shutdown drains and checkpoints. | C2-T03, C2-T04 | `internal/server/server.go` | `make verify`; `go test -race`; fake-channel test asserting: two threads get independent Sessions; a second authorized message mid-drive is queued (or steered on the Steer affordance); an unauthorized mid-drive message is rejected+logged and never reaches `Turn`; clean shutdown with no leaked intake goroutine. |
| C4-T01 | C4 | **Session persistence (continue across restart).** Persist bounded `WorkState` + conversation pointer via the existing `agent.Checkpoint` single-`UpsertTask` write into `store.Task.Detail`; on restart a Session re-hydrates `WorkState` (never raw transcripts — History tail is reconstructable from the append-only log if needed). No new dependency. | C2-T03 | `internal/session/persist.go` | `make verify`; test that a Session's `WorkState` round-trips through the store and a follow-up after re-hydrate continues (driver named by `State.Active`) rather than restarting; no transcript stored, only summary-shaped state. |
| C4-T02 | C4 | **Docs: register the conversational front door in ARCHITECTURE + TASKS (CONTRACT — serialized, last).** Document the `session`/`inbox`/`emit`/`loopctl` packages as extension points, the steer-cancels-model-call-only rule, the trust line (principal message = un-Wrap'd user turn; everything else stays guard.Wrap'd), and the queue/steer event kinds (`user_message`, `steer_interrupt`, `session_route`, `session_followup`, `steer_ack`). Append the C-phase tasks to the master DAG. | C1-T03, C1-T04, C2-T03, C3-T01, C3-T02 | `docs/ARCHITECTURE.md`, `docs/TASKS.md` | `make verify`; docs accurately describe the shipped seams and event kinds; no invariant statement weakened; CHANGELOG entry added. |

---

## Phasing rationale

Five phases, dependency-ordered so the risky concurrency core lands and is proven before anything builds on it.

C0 (prerequisites, fully parallel): C0-T01 makes `writeNoFollow` atomic (temp-file + `os.Rename`) so even an OS kill of the harness never tears a file — correct independent of steer and a precondition for the "no half-applied tool state" guarantee; C0-T02 adds the stdlib-only `internal/emit` sink for live reasoning. Both are leaf, no dependencies.

C1 (the concurrency core — must land green with `go test -race` as a HARD gate before C2+): C1-T01 `internal/inbox` (the mutex-guarded queue + cap-1 steer channel, the user-message seam); C1-T02 `internal/loopctl` (the shared `WithCancelCause`/`Cause` classifier + `ErrSteer` sentinel, so native and super can't drift); then the two serialized contract edits — C1-T03 native loop seam and C1-T04 supervisor loop seam — each gated nil-off (byte-identical when `Inbox==nil`), each wrapping ONLY `Model.Complete` in the cancellable iter-ctx so a steer kills a pure-compute model call and never an in-flight sandbox exec (which would tear the RW-bind-mounted `/work`). Go 1.25 confirms `context.WithCancelCause`/`context.Cause` are available, keeping I6 (stdlib-only core) intact.

C2 (the Session that drives the machinery): C2-T01 the `Session`/`WorkState`/`Phase`/`Turn` state container; C2-T02 the metered `SupervisorFirstRouter` (reusing the `route.go`/`summarize.go` defensive-parse pattern and `agent.ShouldSupervise` as the no-model fallback); C2-T03 the Drivers that map each Route onto the EXISTING native/supervisor/project machinery with no new agentic logic; C2-T04 wires per-step intent emission into both loops (these emit calls ideally ride in the C1-T03/T04 serialized PRs to avoid re-touching the contract files).

C3 (the two front doors over one Session): C3-T01 the interactive `nilcore chat` REPL (primary front door, bare `nilcore` defaults to it); C3-T02 refits serve mode with a per-thread Session map + concurrent intake so Telegram/Slack get queue+steer, every message `Permit`-checked before it can promote to principal trust.

C4 (persistence + docs): C4-T01 round-trips bounded `WorkState` through the existing `agent.Checkpoint`/store for continue-across-restart; C4-T02 is the final serialized docs update registering the new seams, the steer trust line, and the new event kinds.

Sizing: every task is one branch with a disjoint `Owns` set. The four contract-file edits (C1-T03, C1-T04, C3-T01, C4-T02, plus C2-T04 if split out) are explicitly serialized per CLAUDE.md §5; everything else parallelizes within its phase.

---

## Biggest risks

1. STEER-CANCEL SCOPE IS THE WHOLE BALLGAME. The design's decisive call — iter-ctx wraps ONLY Model.Complete, never Box.Exec/Tools.Dispatch/Verifier.Check — is what makes 'no half-applied tool state' true by construction. Confirmed in code: sandbox.go:129 uses exec.CommandContext over a RW bind-mounted /work (sandbox.go:117), so cancelling a tool ctx SIGKILLs the container mid-write (--rm cleans the container, not the host worktree). If an implementer accidentally threads iterCtx into the tool phase (easy to do, since ctx is passed everywhere), a steer can corrupt the worktree. Mitigation: C1-T03/T04 acceptance MUST include a test that an in-flight Box.Exec is NOT cancelled by a steer, plus a code-review check that only Model.Complete receives iterCtx.

2. BYTE-IDENTICAL nil-OFF REGRESSION ON THE TWO CONTRACT LOOPS. native.go and super.go are frozen-contract-adjacent; the whole project's parallel-agent safety rests on the single-agent/single-supervisor path staying unchanged. The seam must take iterCtx:=ctx with NO WithCancelCause, NO watcher goroutine, NO Drain when Inbox==nil (design adv #11). Risk: a subtle allocation or an always-spawned watcher leaks goroutines or perturbs the golden transcript. Mitigation: golden-transcript + goroutine-count tests with Inbox==nil are hard acceptance gates on C1-T03/T04, and go test -race must be green.

3. SHUTDOWN-vs-STEER RACE AND CANCEL-CAUSE DISCRIMINATION. A SIGTERM/deadline and a steer can fire on overlapping ctxs; if classification is wrong, a shutdown gets mistaken for a steer (loop continues past a hard termination rail) or a steer gets mistaken for a fatal model fault (run aborts). The fix — context.WithCancelCause + Cause with taskCtx.Err() checked FIRST — lives in one shared loopctl.ClassifyCancel (C1-T02) so native and super can't drift. Risk is any caller re-implementing the precedence locally. Mitigation: both loops import the shared classifier; the race case (shutdown wins) is an explicit table test.

4. BUDGET / TERMINATION KEYING ACROSS A PERSISTENT CONVERSATION. Each drive builds a fresh per-drive task ID for its worktree/eventlog, but the budget ledger and termination ceilings must be keyed by the CONVERSATION (s.ID), or a long chat silently resets its spend ceiling every follow-up and defeats the budget rail. The metered router classifier must also charge the conversation ledger. Mitigation: C2-T03 acceptance asserts the budget key is s.ID, not the per-drive task ID; C2-T02 asserts the classifier uses the metered provider.

5. TRUST-LINE PROMOTION IN SERVE MODE. Queue/steer must NOT bypass the channel allowlist: a steer is a principal instruction (un-guard.Wrap'd user turn), so an unauthorized sender steering mid-drive would inject controlling instructions (violating I7 + the P2-T07 authorization boundary). Every inbound message must pass channel.Authorized.Permit BEFORE reaching Session.Turn. Mitigation: C3-T02 acceptance includes an unauthorized-mid-drive-message-rejected-and-logged test; the concurrent intake goroutine checks Permit before Push.

6. CONCURRENT INTAKE GOROUTINE LIFECYCLE (serve) AND WATCHER GOROUTINE (loop). Two new long-lived/per-iter goroutines (the serve intake reader and the per-iteration steer watcher) are the classic leak/deadlock surface. The watcher must be torn down deterministically every iteration (cancel(nil); <-watcher) and the intake reader must exit on shutdown without writing to a closed Session. The proven prior art is super/reader.go's stopc/done pattern — reuse it verbatim. Mitigation: go test -race + explicit goroutine-leak assertions on C1-T03/T04 and C3-T02.


---

## Modes, context, and web (shipped, post-design)

The original design above auto-infers everything. This is the behavior of `ModeAuto`,
the default. A later workstream added three things the **user** controls directly,
inside the same one chat loop, without touching the frozen `backend.CodingBackend`
contract (I1). The full per-task account is in the CHANGELOG; the seams:

### User-set modes (`/discuss` · `/plan` · `/execute` · `/auto`)

`session.Mode` (in `state.go`) is a sticky, user-set behavioral policy carried on
`WorkState`, set ONLY by a front-door control verb (never from `Turn`/inbox/tool text,
I7) and overriding the auto-router when pinned. It is a **safety posture**, so it
round-trips through the persistence snapshot — a `/plan` conversation resumes in plan
mode, never silently full-capability. A mid-drive switch applies to the next turn
(capability is fixed at drive launch).

- `/discuss`, `/plan` ⇒ **read-only**. Enforcement is **structural**, not a prompt:
  - a write-free registry (`tools.ReadOnlyWithCodeintel` — read/search/codeintel, no write/edit/git), **and**
  - the new additive `backend.Native.DisableShell`, which suppresses the always-on `run` tool entirely (and refuses any `run` the model emits anyway), **and**
  - a `verify.Pass` pass-through verifier (a research turn ships nothing, so there is nothing to gate — I2 governs only work that ships).
  So a read-only drive has **no registered path to mutate the tree** regardless of what the model attempts. `backend.Native` is byte-identical when `DisableShell=false`.
- `/execute` ⇒ full capability, sized native-vs-supervise by the same heuristic the router uses, gated by the real verifier (I2).
- `/auto` (default) ⇒ the auto-router, byte-identical to before modes existed.

The read-only capability primitives (`tools.ReadOnly`, `tools.ReadOnlyWithCodeintel`,
`policy.ReadOnlyCommandPolicy`) are shared with the multi-agent roles in `internal/roster`.

### Context attach (`/add <path|url>`)

`ReadTool`/`SearchTool` carry an optional `ReadRoots` field: additional **read-only**
roots the read/search tools may consult beyond the worktree (they run host-side, so no
sandbox mount is needed). I4 discipline:

- a **relative** path still resolves only against the worktree (byte-identical default; `../escape` rejected);
- extra roots are addressed by **absolute** path, allowed only if they resolve symlink-safe inside an added root (`/etc/passwd` is rejected);
- `WriteTool`/`EditTool` are untouched — they resolve only against the worktree, so **extra roots are never writable** (single-writable-root invariant preserved).

`/add <path>` validates + symlink-resolves the path at the cmd layer and registers it
on the Session (principal-only, I7; threaded into each drive at launch). `/add <url>`
asks the agent to fetch the URL via `web_fetch` (below).

### Web access (`-allow-egress` + `web_fetch` + `web_search`)

Default stays **default-deny**. `-allow-egress host,host` stands up the allowlist proxy
(`policy.EgressProxy.Start` — the listener/goroutine/shutdown lifecycle around the
existing `ServeHTTP`) bound to the conversation ctx, and routes a **container** sandbox
through it via `AllowEgressVia` (using the runtime host alias; `sandbox.Container.ExtraHosts`
adds `--add-host` for docker-Linux). The `web_fetch` tool (read a URL) and `web_search`
tool (Brave Search; needs `BRAVE_API_KEY` + `api.search.brave.com` allowlisted) are
advertised only when egress is on AND the box is egress-capable; both run inside the
sandbox and `guard.Wrap` their bodies as untrusted data (I7). `web_search` injects its
key as a per-run env var referenced as `$NILCORE_SEARCH_KEY` in the command, so the key
never reaches the command string, the model, or the log (I3). The namespace backend has
no proxy egress path (empty netns), so web access requires the container backend
(fail-closed) — see `docs/OPERATIONS.md`.

### Control verbs, the gauge, and `/clear` (both front doors)

`session.ParseControl` is the single control-verb parser the REPL and the serve intake
both call on principal top-level input only (post-`Authorized.Permit`; never on `Turn`/
inbox/tool text — I7), so `/discuss /plan /execute /auto /add /clear /mode /status
/context /cancel` work identically over the keyboard and over Telegram/Slack. The REPL
prompt shows a per-mode glyph (auto ◇, discuss ◆, plan ▣, execute ▶) and a clockwise
context-usage ring (◔◑◕●, degrading to `context NN%` off a TTY). `meter.CtxWindow` +
`meter.Provider.OnUsage` feed `Session.ContextUsage`; near 80% of the window the prior
conversation is auto-summarized into a compact seed (`session_compact`), and `/clear`
resets History on demand (keeping the pinned mode and attached roots).

### Project steering file (`NILCORE.md` / `AGENTS.md`)

A repo may carry an authoritative steering file (`NILCORE.md`, falling back to
`AGENTS.md`) that the operator owns. `internal/steering` loads it and feeds it to the
agent as **trusted** project instructions — the deliberate, scoped I7 exception. It is
NOT untrusted tool/file content: the operator authored it, so it is admitted as
controlling guidance, wired into the same chat front door (and `run`/`build`). But it
sits **below** the safety core: steering can shape *how* the agent works, never *widen
capability* — it cannot bypass the verifier (I2), the human gate, the sandbox, or any
invariant. Absent the file, the default is byte-identical.
