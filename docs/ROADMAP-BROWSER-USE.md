# Roadmap — End-to-end browser agency (Phase 14)

**North star.** NilCore today can *peek* at a web page: the `browser_view` tool (`internal/tools/browser.go`) launches a headless Chromium **once**, drives an optional fixed flow (`navigate/click/type/key/wait`), captures **one** observation (title + innerText + a screenshot), and exits. That is a behavioral-verification *seam* (D1/R3), not a browser **agent**. This phase turns it into a full **observe → plan → act → verify** browser loop the model drives over many turns against a **persistent, session-scoped** browser — with **accessibility-tree perception** (numbered element refs, not raw coordinates), version-stamped refs that fail closed on DOM drift, a rich action set (scroll, tabs, history, select, upload, download, wait-for, extract), structural prompt-injection containment (Rule-of-Two + plan-then-verify + untrusted-as-data), and a typed, verifier-gated artifact as the only mergeable output.

The thesis is unchanged and *extended*: the model is the engine, the harness stays small, **the verifier is the sole authority on "done" (I2)**, model-emitted execution stays **sandboxed (I4)** behind the **default-deny egress allowlist**, page content is **untrusted data, never instructions (I7)**, and the whole loop is **bounded and append-only-logged (I5)**. Browser-use is the *first* and *correct* GUI modality for a zero-dependency Go agent — accessibility snapshots are **20–50× cheaper in tokens** than screenshots, give **deterministic element identity** (no coordinate-rescale arithmetic, no vision dependency), and are reachable over the **existing pure-Go CDP client** (`internal/cdp`) with **no new module (I6)**. Desktop computer-use — the pixel/coordinate path — is the *separately-gated* sibling in [`docs/ROADMAP-COMPUTER-USE.md`](ROADMAP-COMPUTER-USE.md); this doc is the do-now modality.

> **Staging doc, not the law.** Like `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` and `docs/SWARM.md`, this presumes the seven invariants and the frozen `backend.CodingBackend` contract (`CLAUDE.md` → `docs/ARCHITECTURE.md`) and never restates them — it points. Every task is **additive, opt-in, env/flag-gated, stdlib-first** (`CGO_ENABLED=0`, no new module): **the default `nilcore` binary stays byte-identical when browser agency is off.** Promotion of these specs into the canonical `docs/TASKS.md` is itself a **serialized contract task** (`P14-T15`, §9).

---

## 1. What exists today (the seam we extend, sourced)

| Piece | File | What it does | Gap for "end-to-end browser use" |
|---|---|---|---|
| Pure-Go CDP client | `internal/cdp/cdp.go`, `commands.go` | RFC6455 WebSocket + JSON-RPC CDP; `Navigate/Eval/Title/Text/Screenshot/ElementCenter/Click/ClickSelector/Type/TypeKey/TypeIntoSelector` | No accessibility tree, no scroll, no tabs/targets, no history, no `<select>`, no upload/download, no network-idle wait |
| `browser_view` tool | `internal/tools/browser.go` | one call = one Chrome launch + one optional flow + one observation; screenshot rides back as `model.ImageBlock` via `RunWithImage`/ImageRunner | **Stateless across tool calls** — no persistent session the model can drive turn-by-turn |
| Driver | `cmd/tools/nilcore-browser/{main,interactive}.go` | batch (`--dump-dom`/`--screenshot`) + flow (`--actions`) modes; launches, runs steps, captures, **exits** | No daemon/attach mode; one-shot only |
| Shared contract | `internal/browserwire` | `ShellSingleQuote` (I4 quoting), `Observation{Title,Text,Console,ScreenshotB64}` | Observation carries no a11y snapshot, refs, URL, or tab list |
| UI verify-pack | `internal/artifact/packs/ui` | behavioral claims reproduce in-box | No browse-extract / multi-step trajectory assertions |
| Composite verifier | `cmd/nilcore/verifier.go` (`NILCORE_BROWSER_VERIFY`) | folds a browser check **into** `verify.Check` (I2) | A verifier seam, not an agent surface |
| Egress profiles | `internal/egressprofile` | named allowlist presets, `EgressFor` clamps **narrow-only** | No per-task browse-domain preset |
| Secrets / gate / audit | `internal/secrets`, `internal/policy` (`Gate`,`GateAction`,`Classify`), `internal/eventlog` | secrets host-side (I3); irreversible actions gated; append-only log (I5) | Not yet wired to a browser action-classifier |

**The shape of the work:** keep every one of those guarantees, and add **statefulness, structured perception, a bounded agent loop, and structural injection containment** on top — reusing the spine (`internal/artifact`/`evverify`/`requeue`/`report`) so a browse run that extracts data produces the *same* verifier-gated `artifact.Artifact{Claims[]}` the rest of NilCore already trusts.

---

## 2. How the field deploys this end-to-end (grounded, 2025–2026)

The canonical loop is identical across Anthropic Computer Use, OpenAI CUA/Operator, Google Gemini 2.5 Computer Use, and the OSS leaders (browser-use, Stagehand/Browserbase, Skyvern, Steel). **The model never executes anything — it emits action *intents*; a host harness executes them against a sandboxed browser and feeds back the next observation; an external verifier (not the model's self-report) decides "done."** This is *already* NilCore's architecture; we are aligning the browser surface to it.

```
                    ┌─────────────────────────────────────────────────────────┐
   user goal ─────▶ │  TRUSTED PLANNER  (advisor tier; blind to page content)  │
   (trusted)        │  pre-commits a branching plan — control-flow integrity   │
                    └───────────────┬─────────────────────────────────────────┘
                                    │ act intent {observe | click ref | type ref | navigate | scroll | extract | finish}
                                    ▼
        ┌───────────────────────────────────────────────────────────────────────┐
        │  HOST HARNESS (NilCore)  — application-as-executor, bounded + logged    │
        │   • resolve ref → CDP backend_node_id (version-stamped; fail closed)    │
        │   • Rule-of-Two capability check + policy.Gate on irreversible actions  │
        │   • substitute {{secret:…}} from SecretStore (model never sees it)      │
        └───────────────┬───────────────────────────────────────────────────────┘
                        │ CDP command (Input.dispatch* / Page.* / Accessibility.*)
                        ▼
        ┌───────────────────────────────────────────────────────────────────────┐
        │  SANDBOX (I4)  — persistent headless Chrome, default-deny egress +      │
        │  per-task domain allowlist; the browser NEVER runs on the host         │
        └───────────────┬───────────────────────────────────────────────────────┘
                        │ wait for network-idle / DOM-stability, then snapshot
                        ▼
        ┌───────────────────────────────────────────────────────────────────────┐
        │  OBSERVATION  → guard.Wrap-fenced as UNTRUSTED DATA (I7)                │
        │  a11y set-of-marks (numbered refs + roles + labels) + url + tabs        │
        │  + screenshot fallback (canvas/WebGL only)                              │
        └───────────────┬───────────────────────────────────────────────────────┘
                        │ (perception resolves only runtime VALUES, not control flow)
                        └────────────────────▶ back to model … until finish or step cap
                                               then  verify.Check  governs "done" (I2)
```

**The decisive design choices the leaders converged on (and we adopt):**

1. **Accessibility-tree / "set-of-marks" perception is the PRIMARY channel; a screenshot is the FALLBACK.** Serialize interactive elements to a numbered ref index (≈200–400 tokens vs thousands for a screenshot — a **20–50× cost difference**); the model references `ref=12`, the harness resolves it to a CDP `backend_node_id`. This removes pixel math, DPI/resolution drift, and the vision dependency. Screenshots are kept only for canvas/WebGL/custom widgets the a11y tree omits, and are sent at XGA/WXGA. *(browser-use, Stagehand v3, Playwright-MCP, Skyvern; Set-of-Mark arXiv:2310.11441; a11y ≈ 4.8–4.9× lighter than raw HTML.)*
2. **Version-stamped element refs that fail closed on DOM drift.** A re-render can reuse a ref for a *different* element (the classic Cancel→Delete swap). Every ref carries a snapshot version; the host-side tool rejects a ref whose snapshot is stale rather than acting on the wrong node. *(browser-use stale-ref handling; Playwright Locator vs ElementHandle.)*
3. **The field has graduated OFF Playwright onto raw CDP** for latency, OOPIF/target tracking, and event-driven watchdogs (browser-use, Stagehand v3 ≈44% faster). NilCore already *is* raw CDP and pure-Go — a structural advantage; we extend it, not replace it.
4. **Deterministic waits, never sleeps.** Wait for network-idle / DOM-stability / an expected element *before* re-observing. Realistic fault injection (WAREX, arXiv:2510.03285) collapses naïve agents from **42% → 2%** (network) / **42% → 30%** (server) success — demo numbers hide fragility; production rigor is mandatory.
5. **Screenshot/observe-and-verify after each step; never retry the same action verbatim.** Models assume actions succeeded; force a re-observation and, on failure, a *fundamentally different* approach (a11y-click → keyboard nav → coordinate fallback).
6. **Bounded loop + compacted history + single-snapshot retention.** `max_steps`/`max_failures`, loop/stagnation detection, summarized rolling memory, prompt-cache the stable system prompt + tool defs — keeps token cost flat over long tasks (≈8.9k tokens/$0.0048/6.8s per step is the published order of magnitude; intelligent trimming ≈57% cost reduction).
7. **Security is ARCHITECTURAL, not model-level.** Provider classifiers block ~88–95% of injections but **no agent is immune** (Willison; ~1% adaptive attack success at frontier). The durable fixes — which map 1:1 onto NilCore's invariants — are §6.

> **Sources** (selected, deduped): Anthropic Computer Use tool & best-practices (platform.claude.com/docs), Claude in Chrome safety (support.claude.com), Willison *lethal trifecta* (simonwillison.net/2025/Jun/16) and *computer use* (2024/Oct/22); OpenAI CUA/Operator (openai.com/index/computer-using-agent, operator-system-card) + Responses computer-use guide; Google Gemini 2.5 Computer Use (ai.google.dev/gemini-api/docs/computer-use, blog.google DeepMind); browser-use *Playwright→CDP* (browser-use.com/posts/playwright-to-cdp) + interactive-element detection; Stagehand v3 (browserbase.com/blog/stagehand-v3); Skyvern (skyvern.com/blog); Steel (github.com/steel-dev/steel-browser); Set-of-Mark (arXiv:2310.11441); OmniParser (github.com/microsoft/OmniParser); UI-TARS (arXiv:2501.12326); ScreenSpot-Pro (arXiv:2504.07981); WAREX (arXiv:2510.03285); *An Illusion of Progress?* / Online-Mind2Web (arXiv:2504.01382); CaMeL (arXiv:2503.18813); NOVA *CaMeLs Can Use Computers Too* (arXiv:2601.09923); Meta *Agents Rule of Two* (ai.meta.com, 2025-10-31); *Building Browser Agents: Architecture, Security* (arXiv:2511.19477); pass@k vs pass^k (philschmid.de).

---

## 3. The seven pillars

| Pillar | What it adds | Why it's the value center |
|---|---|---|
| **1 — Persistent browser session** | `internal/browsersession`: a task-scoped, in-sandbox Chrome the model drives across many tool calls; CDP `Conn` stays alive; navigation/cookies/tabs persist for the task. | Turns a one-shot peek into an *agent*. The single biggest gap. |
| **2 — Accessibility set-of-marks perception** | a11y snapshot → numbered, version-stamped element refs + roles + labels + URL + tab list; screenshot demoted to fallback. | 20–50× cheaper, deterministic element identity, no coordinate math. The decisive 2025–26 best practice. |
| **3 — Rich, reliable action set** | `observe / click(ref) / type(ref,text) / key / scroll / select / navigate / back / forward / open_tab / switch_tab / upload / wait_for / extract / finish` with deterministic waits + never-retry recovery. | Covers real flows (login, multi-page, forms, downloads) with WAREX-grade fault tolerance. |
| **4 — Bounded observe→plan→act→verify loop** | `internal/browseragent`: the controller — step/failure budgets, loop/stagnation detection, history compaction, single-snapshot retention, observe-and-verify each step. | The production/demo divide. Keeps cost flat and the loop honest. |
| **5 — Structural injection containment** | Rule-of-Two capability check (`internal/capguard`) + plan-then-verify control-flow integrity (`internal/browseragent/plan`) + untrusted-as-data fencing + per-task egress allowlist + `{{secret:…}}` proxy. | Makes "untrusted-input-is-data" and "no-ambient-authority" *structural*, not advisory — the 2026 consensus, already encoded in NilCore's invariants. |
| **6 — Typed, verifier-gated output** | a browse run that extracts data emits an `artifact.Artifact{Claims[]}` (provenance per claim) judged by an extended `ui` verify-pack; only verifier-green ships (I2). Granular requeue per claim. | Removes "trust the agent's summary." Reuses the Phase-11 spine wholesale. |
| **7 — Surface, eval & audit** | `nilcore browse` subcommand + `browser_*` tools (`NILCORE_BROWSER_AGENT`); a browse-trajectory `report` projection; a fault-injection eval harness (WAREX-style). | A usable, measurable, replayable product surface — the human face of I2/I5. |

**NON-GOALS** (do not plan these here): desktop/OS GUI control via raw screenshot coordinates (that is the *gated* sibling, [`ROADMAP-COMPUTER-USE.md`](ROADMAP-COMPUTER-USE.md)); a vision-grounding/parser model dependency (OmniParser/UI-TARS-class) in the core; CAPTCHA solving or anti-bot evasion (refused — security-integrity category); a host-side browser (every browser run is in-sandbox, I4); auto-acknowledging any safety/confirmation gate in code (it nullifies the human gate — the single most common deployment mistake).

---

## 4. Shared architecture

### 4.1 Pillar 1 — `internal/browsersession` (leaf, stdlib only)

A task-scoped session manager that holds a **live** browser the model drives turn-by-turn.

```go
package browsersession

// Session is a persistent, in-sandbox browser the agent drives across many acts.
// It owns one cdp.Conn (long-lived), the current snapshot (with a version), and the
// page/tab set. It NEVER runs a browser on the host — Launch execs the in-sandbox
// nilcore-browser daemon (box.Exec) and attaches over a loopback control channel (I4).
type Session struct { /* box sandbox.Sandbox; conn *cdp.Conn; snap *Snapshot; ... */ }

type Ref struct {
    ID      int    // stable within a snapshot version: "ref=12"
    Role    string // a11y role (button, link, textbox, …) — UNTRUSTED label
    Name    string // accessible name — UNTRUSTED (page-controlled, I7)
    Version uint64 // snapshot version this ref belongs to
}

type Snapshot struct {
    Version uint64
    URL     string   // current page URL — UNTRUSTED
    Title   string   // UNTRUSTED
    Refs    []Ref    // the numbered set-of-marks (interactive elements only)
    Tabs    []Tab
    Shot    string   // base64 PNG, populated only on demand / canvas fallback
}

func Launch(ctx context.Context, box sandbox.Sandbox, opt Options) (*Session, error)
func (s *Session) Observe(ctx context.Context) (*Snapshot, error)            // re-snapshot after waits
func (s *Session) Act(ctx context.Context, a Action) (*Snapshot, error)      // execute one act, re-observe
func (s *Session) Resolve(r Ref) error                                       // version check: stale ⇒ error (fail closed)
func (s *Session) Close() error
```

- **Statefulness without host risk.** `Launch` execs the in-sandbox `nilcore-browser --serve` daemon (P14-T05) which runs headless Chrome with `--remote-debugging-port` pinned to **loopback** (already the pattern in `interactive.go:interactiveChromiumArgs`); `Session` attaches via the pure-Go `cdp.Dial`. Chrome + the daemon live entirely in the sandbox (I4); the host holds only the `cdp.Conn` byte stream. Each `Act` is one or more CDP commands followed by a deterministic wait and a re-`Observe`.
- **Version-stamped refs (Pillar 2 reliability).** `Observe` bumps `Snapshot.Version`; `Resolve` rejects any `Ref` whose `Version` ≠ current — so a re-render that reused a node id for a different element **fails closed** instead of mis-clicking (Cancel→Delete defense).
- **`{{secret:NAME}}` substitution (Pillar 5).** A `type` act whose text is `{{secret:login_password}}` is resolved **host-side** from `secrets.SecretStore` at execution time, injected into the CDP `Input.insertText`, and **never** placed in the model context, the snapshot, or the log (I3; mirrors browser-use `sensitive_data`). The model only ever sees the placeholder.
- **Invariant compliance.** I1 untouched (rides out-of-band of `backend.Task`/`Result`). I3 (secrets host-side, placeholders to model). I4 (browser in sandbox; loopback debug port). I6 (stdlib + existing `internal/cdp`; no module). I7 (every `Name`/`URL`/`Title`/text is untrusted, `guard.Wrap`-fenced at the tool boundary).
- **Additive/opt-in/byte-identical-when-off:** a leaf nothing imports until P14-T09/T13 wire it.

### 4.2 Pillar 2 — accessibility set-of-marks (extends `internal/cdp` + `internal/browserwire`)

- **`internal/cdp` (P14-T01)** gains: `AccessibilityTree(ctx)` (`Accessibility.getFullAXTree`, filtered to interactive + named nodes), `Scroll`, `Targets`/`AttachToTarget` (tabs/OOPIF), `WaitForLoad`/`NetworkIdle` (via `Page.lifecycleEvent`/`Network` events with a quiescence window), `Select` (`<option>` set), `History` (`Page.navigateToHistoryEntry`), `BackendNodeBox` (resolve a ref to a clickable box). Each new method decodes the **UNTRUSTED** CDP result into a typed Go value, exactly like the existing file.
- **`internal/browserwire` (P14-T02)** grows the contract: `Observation` v2 adds `URL`, `Refs []RefWire{ID,Role,Name}`, `Tabs`, `SnapshotVersion`, keeping `Title/Text/Console/ScreenshotB64` so the existing one-shot `browser_view` and the `ui` pack parse v2 unchanged (additive fields). One tested copy of the wire contract, shared host- and sandbox-side (the existing discipline).
- **Why a11y over pixels (sourced):** generalist VLMs score **<2% on ScreenSpot-Pro** raw-pixel grounding; tiny targets (avg 0.07% of screen) defeat downscaled single-shot vision; a11y refs sidestep all of it and are 20–50× cheaper. Screenshot is captured **only** when the model requests it or the a11y tree is empty (canvas/WebGL) — and then at XGA/WXGA via stdlib downscale, with instruction text placed **before** the image (the documented click-accuracy ordering).

### 4.3 Pillar 4 — `internal/browseragent` (the bounded loop)

```go
type Budget struct { MaxSteps, MaxFailures int; Deadline time.Duration } // bounded autonomy
type Controller struct { /* sess *browsersession.Session; model …; log eventlog; plan *plan.Planner */ }

func (c *Controller) Run(ctx context.Context, goal string) (Result, error)
```

- One turn = `Observe` (after a deterministic wait) → fenced snapshot to the model → model emits **one** act → `Resolve`+`Act` → repeat. **Single-snapshot retention** (only the latest snapshot stays in context; prior turns compact to a one-line summary) keeps token cost flat.
- **Loop/stagnation detection:** identical (act, post-snapshot) pairs N times, or no DOM/URL change after an act, trips a *stagnation* break — the controller injects "that did nothing; try a fundamentally different approach" and counts a failure; **never retries the same act verbatim**.
- **Budgets are hard:** `MaxSteps`/`MaxFailures`/`Deadline` bound the loop; exceeding any returns control (to the gate / supervisor) with full diagnostics (last snapshot + console + act history) — the WAREX/runaway-cost defense. The cap is **logged** (silent truncation reads as success).
- **Invariant compliance.** I2 (the controller never declares done — it hands the result to `verify.Check`). I5 (every observe/act/gate appends to the event log; replayable). I7 (snapshots/console are data; the *only* trusted instruction stream is the user goal + harness scaffolding).

### 4.4 Pillar 6 — typed output (reuses the Phase-11 spine)

An `extract` act writes claims into `.nilcore/artifacts/<id>.json` (`artifact.Artifact`, via the existing sandboxed write path + `internal/worktreefs`); the extended **`ui` verify-pack (P14-T10)** registers verifier-ids that **re-derive** each claim in-box (re-navigate + re-read the asserted value, assert a behavioral predicate) and overwrite the model's self-claimed status (I2). A failed claim is one **granular requeue** unit (`internal/requeue`) — re-drive that field, not the world. Browse output therefore ships only because **every claim passed a runnable check**, identical to every other NilCore artifact.

---

## 5. Pillar designs at a glance (packages · seams · gating)

| Pillar | New/extended packages | Plugs in at | Env/flag gate |
|---|---|---|---|
| 1 Session | `internal/browsersession` (new); `cmd/tools/nilcore-browser` (extend: `--serve`/attach) | `box.Exec` launch; `cdp.Dial` attach | — (leaf) |
| 2 Perception | `internal/cdp` (extend); `internal/browserwire` (extend) | `cdp.Conn` methods; `Observation` v2 | — |
| 3 Actions | `internal/browsersession` (Act dispatch); `cmd/tools/nilcore-browser` | CDP `Input.*`/`Page.*` | — |
| 4 Loop | `internal/browseragent` (new) | model loop; `internal/eventlog` | — |
| 5 Containment | `internal/capguard` (new, Rule-of-Two); `internal/browseragent/plan` (new); `internal/egressprofile` (extend, `browse` preset) | session setup; `policy.Gate`; egress proxy | — |
| 6 Output | `internal/artifact/packs/ui` (extend); `internal/requeue` (reuse) | `evverify.Registry`; `verify.Composite` | `NILCORE_VERIFY_PACKS=ui` (exists) |
| 7 Surface | `cmd/nilcore` (`browse` arm); `internal/tools` (`browser_*` tools); `internal/report` (trajectory); `eval/browse` | one dispatch arm; native loop registry | `NILCORE_BROWSER_AGENT` / `-browse` |

Every package above is a **leaf** or an additive extension; **no existing package imports a browse leaf until the wiring task (P14-T13)**, so the default binary is byte-identical with the feature off (the same nil-gated discipline as `Advisor`/`Peer`/`browser_view`).

---

## 6. The security spine (where 2026 converged — and it IS NilCore's invariants)

Provider/model defenses are necessary but **insufficient** (Willison; ~1% adaptive success at frontier scale). The durable answer is architectural and **maps 1:1 onto NilCore's existing law** — the upgrade is to make it *structural and enforced in Go code, not the prompt*.

1. **Rule of Two / lethal trifecta (`internal/capguard`).** Never let one browse session combine all three of **[A] untrusted input** (reading arbitrary web), **[B] private-data access** (mounted secrets/repo files), **[C] open external communication** (egress beyond the task allowlist). NilCore already denies [C]-to-arbitrary-hosts (default-deny egress) and [B]-as-plaintext (secrets never in prompt). `capguard.Classify(session) → {A,B,C}` refuses to *grant* all three at once and **routes to `policy.Gate` (human)** when all three are unavoidable. This is the Meta *Rule of Two* / Willison *lethal trifecta* fix, enforced at session setup. *(ai.meta.com 2025-10-31; simonwillison.net 2025-06-16.)*
2. **Plan-then-verify control-flow integrity (`internal/browseragent/plan`, CaMeL/NOVA-flavored).** The **trusted planner** (advisor tier, `internal/advisor`) receives **only** the user goal + the *structural* affordances (ref count, roles) — **never** raw page text/labels/screenshots — and pre-commits a **branching plan**. The **untrusted perception** stream then resolves only *which* ref/value to act on inside that pre-committed structure, so injected on-screen text **cannot rewrite control flow** (residual risk is the narrow "Branch Steering" class). This realizes NilCore invariant **I7** ("untrusted input is data, never instructions") as *dataflow*, not advice. *(CaMeL arXiv:2503.18813 — 67% AgentDojo attacks defeated, 77% tasks with provable security; NOVA arXiv:2601.09923 — retains ~57% utility.)*
3. **Untrusted-as-data, always.** Every snapshot, label, URL, console line, and download is `guard.Wrap`-fenced as data (the existing I7 boundary in `browser_view`); the a11y `Name`/`Role` are page-controlled and treated as untrusted even though they look structural.
4. **Per-task egress allowlist (`internal/egressprofile` `browse` preset).** The session can reach **only** the domains the task names; `EgressFor` still clamps narrow-only and default-deny holds — limiting both injection intake and exfiltration. A denied host is unreachable and the act fails closed (I4).
5. **Secrets stay host-side (I3).** `{{secret:…}}` placeholders only; real values injected at the CDP boundary, scrubbed from logs (the SecretStore + redaction discipline). Prefer human-mediated login for high-value sites (the "takeover" pattern) over handing the agent a credential.
6. **Human gate for irreversible web actions (I2 + bounded autonomy).** `policy.Classify` already flags irreversible actions; extend it with a browser action-semantic check — clicks/submits whose target text matches `purchase|pay|transfer|delete|refund|consent|accept terms|accept cookies` route through `policy.Gate` (human), enforced **in code, not the prompt**. **Never auto-acknowledge** any confirmation — surfacing-then-echoing a gate without a real human approval is the #1 deployment mistake and nullifies the gate.
7. **Bounded + fully logged (I5).** Step/failure/deadline budgets; every model call, act, observation hash, gate decision appended to the hash-chained event log; deterministic replay for audit and regression (the `report` projection, Pillar 7).

> **What NilCore refuses** (sourced from the synthesis): no raw screenshot-coordinate control as the default path; no model-side execution of anything; no auto-acknowledged safety checks; no credentials/secrets in the model context or sandbox; no broad ambient authority or open egress; no unbounded loops; no assembling the lethal trifecta in one agent to chase a feature; no trusting any single benchmark number — `make verify` is the authority.

---

## 7. The task DAG (`P14-T##`)

One task = one branch = one PR (`CLAUDE.md` §5). **Owns** is package-directory granular and **disjoint** across any two concurrently-open tasks. `make verify` green is part of every task's Definition of Done. Every task ships **additive + opt-in + byte-identical-when-off** with the test that proves it.

| ID | Wave | Title | Depends on | Owns | Notes |
|---|---|---|---|---|---|
| P14-T01 | 0 | CDP extensions: a11y tree, scroll, targets/tabs, network-idle/load waits, `<select>`, history, ref→box | — | `internal/cdp` | pure-Go; UNTRUSTED decode |
| P14-T02 | 0 | `Observation` v2 (URL, refs, tabs, version) — additive fields | — | `internal/browserwire` | one shared wire copy |
| P14-T03 | 0 | `capguard` Rule-of-Two classifier leaf | — | `internal/capguard` | refuses A∧B∧C; routes to gate |
| P14-T04 | 1 | `browsersession` (persistent session, version-stamped refs, `{{secret}}` substitution) | P14-T01, P14-T02 | `internal/browsersession` | I3/I4/I7 keystone |
| P14-T05 | 1 | `nilcore-browser --serve`/attach daemon + a11y snapshot + rich actions | P14-T01, P14-T02 | `cmd/tools/nilcore-browser` | loopback debug port; fail-closed |
| P14-T06 | 1 | egress `browse` preset + per-task domain allowlist | — | `internal/egressprofile` | narrow-only clamp preserved |
| P14-T07 | 2 | `browseragent` loop (budgets, stagnation/loop detect, history compaction, observe-and-verify) | P14-T04 | `internal/browseragent` | never declares "done" (I2) |
| P14-T08 | 2 | plan-then-verify control-flow integrity (planner blind to untrusted) | P14-T07 | `internal/browseragent/plan` | CaMeL/NOVA dataflow |
| P14-T09 | 3 | stateful `browser_*` tools (observe/act/extract) + secret substitution + guard fencing | P14-T04, P14-T05 | `internal/tools` (browser*.go) | shares `internal/tools` — serialize vs other tools tasks |
| P14-T10 | 3 | `ui` verify-pack: browse-extract claims + multi-step behavioral assertions reproduce in-box | P14-T09 | `internal/artifact/packs/ui` | overwrites self-claimed status (I2) |
| P14-T11 | 3 | browse-trajectory `report` projection (+ replay) | P14-T02 | `internal/report` | shares `report` — serialize |
| P14-T12 | 3 | browse eval scenarios + WAREX-style fault-injection harness | — | `eval/browse` | pass@1 + pass^k; report the cap |
| P14-T13 | 4 | `nilcore browse` subcommand + `buildBrowse` + `NILCORE_BROWSER_AGENT` gate + session lifecycle + Rule-of-Two + irreversible-action gate | P14-T07, P14-T08, P14-T09, P14-T06, P14-T03 | `cmd/nilcore` (browse arm) | shares `cmd/nilcore` — serialize |
| P14-T14 | 5 | Staging-doc consolidation | — | `docs/ROADMAP-BROWSER-USE.md` | this file |
| P14-T15 | 5 | Promotion into the canonical DAG | P14-T01…T14 | `docs/{TASKS,ARCHITECTURE}.md`, `CLAUDE.md` (repo-map line), `CHANGELOG.md` | **contract · serialized** |

**Acceptance criteria are uniform** (each task spec, when promoted, carries these): builds + `make verify` green; the feature is **off by default** and the default binary is byte-identical when the gate env is unset; new code paths have hermetic unit tests (live Chrome is **CI-only**, exactly like the existing browser e2e — `runInteractive` is exercised only by the browser-e2e job, never unit tests); every observation is `guard.Wrap`-fenced; no new Go module (`go.mod` unchanged, I6; `CGO_ENABLED=0`); no secret reaches the model or the log; the loop is bounded and every step is logged.

---

## 8. Multi-worker parallel execution plan

This is the `CLAUDE.md` §5 protocol applied to Phase 14 — **the same worktree-per-task pattern NilCore dogfoods.**

**Wave schedule (Owns-disjoint ⇒ safe to run concurrently):**

```
Wave 0  ──▶  P14-T01 (cdp)   ∥  P14-T02 (browserwire)  ∥  P14-T03 (capguard)        3 agents
Wave 1  ──▶  P14-T04 (browsersession)  ∥  P14-T05 (nilcore-browser)  ∥  P14-T06 (egressprofile)   3 agents
Wave 2  ──▶  P14-T07 (browseragent)  ──▶  P14-T08 (browseragent/plan)               (T08 after T07: same package tree)
Wave 3  ──▶  P14-T09 (tools)   ∥  P14-T11 (report)   ∥  P14-T12 (eval/browse)        then P14-T10 (ui pack) after T09
Wave 4  ──▶  P14-T13 (cmd/nilcore wiring)                                            solo on cmd/nilcore
Wave 5  ──▶  P14-T14 (staging doc)  ──▶  P14-T15 (promotion, contract)               serialized
```

**Rules that keep it collision-free:**
- **Disjoint Owns per wave.** No two concurrently-open `task/P14-*` branches share a package directory. The three Wave-0 leaves (`internal/cdp`, `internal/browserwire`, `internal/capguard`) are independent; Wave-1's three each own a different dir.
- **Shared-dir tasks are serialized, not parallel.** `internal/tools` (T09), `internal/report` (T11), and `cmd/nilcore` (T13) are each touched by exactly one P14 task at a time — and T13 must not overlap any *other* in-flight `cmd/nilcore` task elsewhere in the repo (check `git branch -a` first, per §5 rule 3).
- **Contract files are serialized — never edited in parallel:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `CLAUDE.md`, `go.mod`, `Makefile`. Only **P14-T15** touches them, last, as a dedicated contract task. **No P14 task adds a module** (I6), so `go.mod` is never in a P14 diff before T15.
- **Branch = claim.** `git worktree add ../nilcore-P14-T0X -b task/P14-T0X origin/main`; work scoped strictly to `Owns`; `make verify`; PR against `main`; squash-merge is the gate. Dependent waves start only once their `Depends on` set is **merged to main**.
- **Critical path:** T01 → T04 → T07 → T08 → T13 → T15 (six serial hops); everything else parallelizes around it. With ~3 workers the wall-clock is roughly the critical path plus wiring/promotion.

---

## 9. Pitfalls to avoid (each with its mitigation, baked into the tasks)

| Pitfall (sourced) | Mitigation (where it lives) |
|---|---|
| **Stale element refs / same-ref semantic swap** (Cancel→Delete) | Version-stamped refs; `Session.Resolve` fails closed on version mismatch (P14-T04) |
| **Acting on stale state** (DOM/layout shift, async) | Deterministic network-idle/DOM-stability wait *before* every re-observe (P14-T01/T04); no `sleep` |
| **Model assumes success** | Observe-and-verify each step; loop/stagnation detection; never retry verbatim (P14-T07) |
| **Runaway loops / cost blowups** | Hard `MaxSteps`/`MaxFailures`/`Deadline`; the cap is logged, not silent (P14-T07) |
| **Context bloat over long sessions** | Single-snapshot retention + compacted rolling summary; a11y over screenshots (P14-T02/T07) |
| **Token cost of screenshots** | a11y primary (20–50×↓); screenshot only on demand/canvas, at XGA/WXGA (P14-T02) |
| **Prompt injection via page content** | Plan-then-verify (planner blind to untrusted) + untrusted-as-data fencing + Rule-of-Two (P14-T03/T08) |
| **Lethal trifecta assembled by accident** | `capguard` refuses A∧B∧C; gate when unavoidable (P14-T03/T13) |
| **Secrets leaking via prompt/log** | `{{secret:…}}` host-side substitution; SecretStore + redaction; placeholders to model (P14-T04) |
| **Irreversible action without a human** | Action-semantic classifier → `policy.Gate`; **never auto-acknowledge** (P14-T13) |
| **Canvas/WebGL/non-DOM apps** (Sheets, Figma) | Screenshot fallback path; documented as a known limit, not a silent failure (P14-T02/T09) |
| **a11y-tree gaps** (unlabeled widgets) | Fallback to coordinate-from-screenshot for that element; fail closed if neither resolves (P14-T05) |
| **New-tab/navigation desync** | Target/tab tracking (`Targets`/`AttachToTarget`); switch_tab act; snapshot includes tab list (P14-T01) |
| **Demo-grade fragility (42%→2% under faults)** | WAREX-style fault-injection in the eval harness; pass^k (reliability), not just pass@1 (P14-T12) |
| **Trusting benchmark numbers** (WebVoyager debunked, Mind2Web stale) | `make verify` + the `ui` pack are the authority; evals are signal, never the gate (P14-T10/T12) |
| **Playwright/abstraction latency** | Already raw CDP, pure-Go — a structural win we keep (P14-T01) |

---

## 10. Definition of Done (phase-level)

Phase 14 is done when: a human can run `NILCORE_BROWSER_AGENT=1 nilcore browse -goal "…" -egress-profile browse` and the agent drives a persistent, sandboxed browser through a multi-step flow (login, navigate, fill, submit, extract) over a11y refs; every observation is fenced data; secrets never reach the model; the Rule-of-Two check and the irreversible-action gate fire as specified; the run emits a verifier-gated `artifact.Artifact` that ships **only** because the `ui` pack re-derived every claim in-box (I2); the whole trajectory is replayable from the event log (I5); `make verify` is green; the default binary is byte-identical with the feature off (I6); and **P14-T15** has folded the canon update (TASKS/ARCHITECTURE/CHANGELOG) in as a serialized contract change. Desktop computer-use remains the separately-gated sibling — [`ROADMAP-COMPUTER-USE.md`](ROADMAP-COMPUTER-USE.md).
