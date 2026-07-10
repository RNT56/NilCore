# Roadmap — Desktop computer use (GATED tier `CU-T##`)

**Full desktop/OS GUI control — screenshot in, mouse/keyboard out — quarantined behind an explicit decision gate, exactly like the `EXT-NN` items in `docs/ROADMAP-EXTERNAL-INFRA.md`.**

NilCore's `docs/ARCHITECTURE.md` states the position plainly: *"Full **desktop** computer use remains a deliberate non-goal."* It is a non-goal not because it is impossible — the design below shows it is entirely buildable on the existing spine — but because it **expands NilCore's identity and threat surface** in a way that must be a *recorded human decision*, not a casual feature. A full virtual desktop is a far larger attack/escape surface than a headless browser; the safe isolation tier for it is a **microVM (Firecracker/KVM)**, which is itself the gated `EXT-08`; and the perception path (raw screenshot coordinates) imports a vision/coordinate-rescale failure surface that the synthesis (`docs/ROADMAP-BROWSER-USE.md` §2) shows is **strictly worse than browser-use's accessibility-tree path** for the coding-and-research work NilCore exists to do.

So this tier is **fully blueprinted but not eligible work.** Build [`ROADMAP-BROWSER-USE.md`](ROADMAP-BROWSER-USE.md) first — it is the correct GUI modality. Reach for desktop computer use **only** when a task genuinely needs to drive a native app or a canvas/non-DOM surface a browser cannot, **and** a human has cleared the §0 gate below.

> **Why a separate, gated doc.** `docs/PRINCIPLES.md` names the governing anti-principle — *"bolting on features that dilute the core"* — and `docs/ARCHITECTURE.md` fixes the shipping identity (one small self-hosted Go binary). Desktop computer use contradicts the "non-goal" line on purpose. Keeping it here — visible, fully specified, but **gated** — is how NilCore stays honest: the capability is acknowledged and a complete path exists, without the core silently drifting into a general-purpose "computer operator."

---

## 0a. Agreed design — the two-path model (converged with the operator)

After Phase 14 shipped, the realization that reframes this whole tier: **~80% of the computer-use machinery already exists.** Computer use is not a new agent — it is a **new perception+actuation backend behind the SAME native loop** the browse agent uses. Already-built and directly reused (with `file:line`): `capguard.Evaluate` (the Rule-of-Two injection containment — `internal/capguard/capguard.go:114`), the egress allowlist proxy, the multimodal `model.ImageBlock` seam (`internal/model/model.go:56`) over `tools.ImageRunner`/`Registry.DispatchRich` (`internal/tools/tool.go:41,119`), the bounded observe→act loop + budgets + stagnation + plan-then-verify + `guard.Wrap` fencing, the artifact/verifier spine, the **sidecar-image-as-optional-capability** pattern (`images/sandbox/` → `nilcore-browser`), the **file-queue daemon transport** (`internal/browsersession`), and the **trust-ledger strength-routing** (`internal/trust` + `agent.Selector` @ `internal/agent/orchestrator.go:51`). So "full-blown computer use" reduces to a **contained virtual desktop + a `nilcore-desktop` driver + a desktop perception ladder + a thin `computer_use` tool** — and the thin-tool/fat-driver split keeps it from bloating the core.

### Two paths, one governed body

The capability ships as **two front-ends over one shared, governed body.** The body — the `nilcore-desktop` driver, the sandbox, `capguard`, the verifier, the bounded loop, the audit log — is identical for both. The paths differ in **exactly two things: how the model *describes* an action, and how we *perceive* the screen.** Everything that makes it *governed* is shared, so two paths cost ~two thin front-ends, not two stacks.

| | **Path B — Custom** (DEFAULT, the one we own) | **Path A — Native** (`NILCORE_COMPUTER_NATIVE`, opt-in) |
|---|---|---|
| **Tool format** | Our own generic `computer_use` schema `{action, ref?, coordinate?, text}` — reuses `model.Tool` (`internal/model/model.go:61`) **as-is** | Anthropic's `computer_20251124` built-in beta tool, fully matched |
| **Contract change** | **NONE** — rides the shipped `ImageBlock`/`ImageRunner` seam | **The lone one** (`CU-T12`): `model.BuiltinTool` + the `anthropic-beta: computer-use-2025-11-24` header, only when a builtin tool is present |
| **Perception** | The **Set-of-Marks ladder** (below): refs → marked screenshot → coordinates | Pixels/coordinates (Anthropic's design) |
| **Vendor** | Any vision model — provider-agnostic | Anthropic-locked (that session) |
| **Role** | The vendor-independent **workhorse we compound forever** | The borrow-able **frontier ceiling** for the hardest pure-pixel cases |

Path A is **off the default critical path** — nothing the default build does depends on it; it lights up only behind its env flag, and it is the only task that touches the frozen `internal/model`/`internal/provider` contract.

### Path B's perception — the 3-rung Set-of-Marks ladder

Path B unifies every perception mode into ONE model-facing contract — **"pick numbered element [N]"** — so the model's output shape never changes as the ground source degrades. All three rungs live in the **fat `nilcore-desktop` driver**, never the thin core tool. Best → worst:

- **Rung 1 — AT-SPI refs (preferred: cheapest, exact, no image).** The driver reads the Linux accessibility tree (AT-SPI2 over D-Bus): walk from the registry root, **filter to interactive widgets that are `SHOWING && SENSITIVE`** (mandatory — raw trees are ~5× too verbose), read each kept node's role + accessible name + `GetExtents(SCREEN)` box, and number them `[1..N]` — emitting a compact `Ref{ID,Role,Name,Box}` list mirroring `browserwire.Ref` (`internal/browserwire/browserwire.go:35`). Actuation on "click ref N" prefers **`Action.DoAction`** (DPI/resolution-independent — zero coordinate math), falling back to an `xdotool` click at the box centre only when a node exposes no action. Extents are **re-queried at action time** (boxes go stale during scroll/animation). *This is the desktop twin of the browser set-of-marks we already shipped in `internal/cdp/snapshot.go`.*
- **Rung 2 — SoM-annotated screenshot (fallback when the tree is empty/sparse/lying).** Numbered boxes come from two sources, in priority: **(2a)** AT-SPI extents drawn onto the screenshot (SoM cheapness *plus* exact a11y boxes, with pixels so the model can read an ambiguous icon), and/or **(2b)** lightweight **CGO-free classical-CV** proposals (grayscale → gradient/Canny edges → dilation → connected-component labelling → enclosing rects — pure arithmetic over `image.Image`, no ML, no module). The driver captures (shell to `scrot` in the sandbox), resizes to the model's pixel budget with stdlib `image/draw`, overlays numbered boxes + badges (digit glyphs via an **embedded 5×7 bitmap table = zero module**), and emits the marked PNG as a `model.ImageBlock` over the **existing** `DispatchRich` seam — byte-identical to the browser screenshot path. The `id→box` table is held driver-side and **logged (I5)** so the click is replayable. The model still outputs `[N]`; the driver maps `id→box→centre`.
- **Rung 3 — raw coordinate pointing (last resort).** Only when no boxes can be produced (canvas/WebGL/games/immediate-mode UIs with zero a11y *and* CV too noisy to mark). The model emits `{coordinate:[x,y]}` in the **resized image space**; the driver rescales resized→true-virtual-screen pixels in **one place** (the #1 mis-click bug, owned once) before the `xdotool` click. **Rung 3 is exactly where Anthropic's RL-grounding (Path A) is strongest** — so when `NILCORE_COMPUTER_NATIVE` is set, Rung 3 may **hand the single grounding sub-call to Path A's native tool** (return one `{x,y}`) while Path B keeps the loop, safety, and verifier. Flag off ⇒ Rung 3 stays self-owned raw-coordinate.

### The ladder trigger — per-step, window-cached

The rung is chosen **per-step (per observation), against the currently-focused window**, not per-session — because one task routinely mixes app classes (a rich GTK dialog, an empty Electron pane, a canvas preview). A per-session latch would either over-trust a sparse tree for the whole run (silent mis-targeting) or pay the screenshot cost where AT-SPI would have sufficed. Drop triggers: **1→2** when the filtered actionable-mark count is empty/implausibly sparse for the window's area, *or* when a ref-click verifiably fails to change state across two steps (the existing **stagnation detector**, reused); **2→3** when neither AT-SPI boxes nor CV proposals yield a plausible mark over the region. A short **per-window cache** (invalidated on focus change / resize / stagnation) keeps a stable dialog from being re-probed every action. Handoff to Path A is **per-step, Rung-3-only, opt-in-only**, and never a per-session surrender — Path B always retains the governed body.

### The improvement engine — how Path B compounds without retraining

We can't touch the model weights (Anthropic's), so every lever is in the harness (ours). Four, each wired to a real shipped seam:

- **(a) SoM / visual-detection depth** — the Rung-2 classical-CV box-source (`internal/desktop/detect.go`, pure-Go, zero module) improves independently (better edges/dilation/CC-labelling, occlusion pruning, mark caps). Fewer raw-coordinate cases each version.
- **(b) AT-SPI coverage depth** — the Rung-1 extractor widens the interactive-role set and adds toolkit enablement (`QT_ACCESSIBILITY`, Electron `--force-renderer-accessibility`). More apps stay on the cheap exact rung.
- **(c) Model-routing over the Phase-13 trust ledger — the strongest lever, no retraining, no vendor lock.** The desktop backend registers as a new name in `Orchestrator.Backends`; the existing `trust.Selector` (`internal/trust/selector.go`) / `agent.TrustOracle` path re-ranks it from fresh verifier-judged `race_outcome` signals **with zero wiring change**. The pixel-grounding sub-task routes to whichever configured backend has earned the strongest record — so as the ecosystem ships better grounding models, NilCore routes to them **automatically and vendor-independently**, and `trust.Replay` (`internal/trust/replay.go:37`) keeps that strength auditable from the append-only log (I5).
- **(d) The eval flywheel** — a new `eval/desktop` sibling **reuses the `eval/browse` harness unchanged** (`FaultPlan`/`Grade`/`Reliability.PassAt1`/`PassPowK` @ `eval/browse/browse.go:38,117,153,162`): an OS-task scenario catalog + desktop faults (DPI change, mid-task resize, a11y-tree-goes-empty, sparse-tree-lies). **Every change to (a)/(b)/(c) is gated on pass@1 + pass^k** — improvement is *measured*, realizing principle #9 (earn-from-evidence), not hoped.

### Packaging & the I6 line (the load-bearing detail)

**Env-flag `NILCORE_COMPUTER_USE`, always compiled but inert-when-off** (parity with `NILCORE_BROWSER`): a thin, stdlib-only, unadvertised core tool + the fat `nilcore-desktop` driver in the **optional** `images/sandbox-desktop` image. The **core gains no Go module** — SoM overlay is stdlib `image/draw` + an embedded digit bitmap; classical-CV is pure arithmetic over `image.Image`. **AT-SPI is the one place a module is tempting (`godbus`): the driver should instead hand-roll a minimal D-Bus client over the unix socket — exactly as NilCore already hand-rolled the CDP WebSocket+JSON-RPC client for the browser driver** — so the *whole repo* `go.mod` stays unchanged (I6 fully intact), not just the core binary. (Falling back to a `godbus`-bearing *separate* driver module is the documented Plan B if the narrow D-Bus slice proves too costly to hand-roll.) `at-spi2`/`xdotool`/`scrot` are **image-baked binaries**, never `go.mod`. `CGO_ENABLED=0` throughout.

### Honest limits (carried into §6)

- **AT-SPI coverage is a structural hole, not an edge case.** Electron/Chromium expose empty/partial trees by default (VS Code ignores `--force-renderer-accessibility`; newer Chromium needs `AXManualAccessibility`), Java/Swing is fragile, and canvas/WebGL/games have **zero** tree. These are exactly the surfaces that *justify* desktop CU over browser-use — and exactly where Rung 1 evaporates, pushing traffic onto the weaker Rungs 2/3. The capability is strongest on native GTK/Qt apps (which browser-use can't reach but are also the friendliest case).
- **Classical-CV boxes are genuinely weak** (over/under-segment, miss flat icons, no semantics). SoM fixes *which box*, not *what it is* — without OCR/captions (deliberately refused to hold I6) the model reads ambiguous icon labels from raw pixels and can mis-identify a correctly-boxed element.
- **Linux-X11-in-a-container only.** The design is Xvfb/`xdotool`/`scrot` — Wayland breaks (`xdotool` is X11-only). This is the *contained Linux desktop*; it **runs on a macOS/Windows host** via the container runtime's Linux VM (the headless Xvfb never touches the host screen). Driving the **real** macOS desktop is a separate, higher-risk tier — a darwin driver behind the same contract — fully planned in [`ROADMAP-COMPUTER-USE-DARWIN.md`](ROADMAP-COMPUTER-USE-DARWIN.md) (`CU-MAC-T##`), where I4 holds only inside a disposable guest VM and host-control mode is the most-gated tier in the roadmap. The live Linux e2e is **container-required** (runs anywhere with Docker/Podman, including a Mac), not literally CI-only.
- **Perception sophistication ≠ reliability.** OSWorld evidence shows mixed screenshot+filtered-a11y gives only *modest* gains and agents stay far below human. The **verifier (I2)**, not tree richness, remains the only authority on done.

---

## 0b. Implementation status — BUILT (the §0 gate was cleared by the operator)

This tier is no longer a pure blueprint: the operator cleared the §0 thesis gate (CU-T00) by explicit decision, and the design below is **implemented and `make verify`-green**. What shipped:

- **Path B (default) — COMPLETE end-to-end.** `internal/desktopwire`, `internal/desktopsession` (file-queue transport, stale-ref guard, `{{secret}}` host-side), `internal/desktopagent` (the thin `computer` tool + plan prompt), `internal/som` (the SoM overlay, zero module), `internal/desktop` (classical-CV detector + the per-step rung ladder), the `cmd/tools/nilcore-desktop` driver (the 3-rung ladder, shells to image-baked `nilcore-a11y-dump`/`scrot`/`xdotool`, brings up Xvfb), `cmd/nilcore desktop` (env-gated, Rule-of-Two), `images/sandbox-desktop`, and `eval/desktop` (reuses the `eval/browse` reliability harness).
- **Path A (native Anthropic) — BUILT, opt-in.** The lone frozen-contract change (`internal/model/builtin.go` + the Anthropic provider beta header) is byte-identical for normal tools (tested); `tools.BuiltinProvider` lets a tool advertise the typed builtin def; `desktopagent.NativeComputerTool` translates Anthropic's native actions to the SAME driver, which runs `--native` (raw fixed-size screenshots, 1:1 coordinates).
- **Implementation refinements over this plan (on-thesis):** the AT-SPI source is an **image-baked `pyatspi` dump tool the driver shells to** (the chromium pattern) rather than a hand-rolled D-Bus client — keeping the *whole repo* `go.mod` unchanged; and the `computer` tool lives in its own **`internal/desktopagent`** package (not `internal/tools/computer.go`) to avoid a tools→session import, exactly as browse does.
- **CI-only / Linux-X11:** the live Xvfb/AT-SPI/xdotool path runs only in a desktop-e2e CI job (no display on a macOS dev host); all pure logic (a11y parse, xdotool builders, rung-1/2/3 + native assembly, SoM overlay, CV detect, ladder, secret/stale-ref guards, file-queue round-trip, the Path-A contract) is unit-tested hermetically.
- **CU-T11 (model selection) — BUILT, the operator's single-model form.** *Not* multi-model routing: computer use (and browser use, for parity) runs on **one model set for the feature** — `-model` flag → `NILCORE_COMPUTER_MODEL`/`NILCORE_BROWSE_MODEL` env → a strong GUI default (**Opus 4.8**, `claude-opus-4-8`; Fable 5 the alternative), via `guiModelSpec`/`resolveNativeSpec` in `cmd/nilcore`. This deliberately does **not** consult the general executor config (GUI grounding wants a capable model) and adds no orchestrator change. The earlier trust-ledger *per-step* grounding-routing idea stays out of scope by decision — a single configured model is the design.
- **Live e2e is local + model-free.** `make desktop-e2e` (`test/desktop-e2e.sh`) builds the desktop image and exercises the real Xvfb/scrot/xdotool/AT-SPI stack **inside the container** — no API key, no host display — so it runs identically on macOS or Linux via Docker/Podman (it is container-required, **not** CI-only). The hermetic Go tests additionally cover the full driver `runServe` loop over the file-queue with all live seams faked.

---

## 0. The gate — what must be true before ANY `CU-T##` task is written

A `CU-NN` item is **not** an eligible task under `CLAUDE.md` §5 work-selection. Before a single line is written, **all** of the following must hold and be recorded (in the PR that promotes the item into `docs/TASKS.md`, itself a serialized contract change). This mirrors `docs/ROADMAP-EXTERNAL-INFRA.md` §0:

1. **A recorded thesis decision.** A human owner has explicitly decided NilCore's identity may expand from "a coding/research harness" to "an agent that can drive an arbitrary graphical desktop." This decision **is** the gate; it is *not* delegable to the agent (it is precisely the class of irreversible, outward-facing action the whole design reserves for a human).
2. **Invariants extended, not bypassed.** The item must show concretely that it **extends** `I1`–`I7`. In particular: the virtual desktop runs **inside the sandbox (I4)** — a contained Xvfb display + apps, never the host screen; **no secrets reach the model or the sandbox (I3)**; **the verifier still governs "done" (I2)**; **every action is logged (I5)**; **screen content is untrusted data, never instructions (I7)**.
3. **The strongest feasible isolation is used.** Because a full desktop runs far more code than a headless browser, the recommended substrate is a **microVM (`EXT-08`)**. Shipping desktop computer use on a shared-kernel container is acceptable only as a *labeled, lower-assurance* tier with an even tighter egress allowlist — and the gate owner must accept that trade in writing.
4. **Default-off, opt-in, inert when off.** Per the agreed env-flag packaging (§0a), the thin `computer_use` tool is compiled into `nilcore` but is **inert and unadvertised** unless `NILCORE_COMPUTER_USE` is set — so the *behavior* of the default binary is byte-identical with the flag off, exactly like `NILCORE_BROWSER`. The desktop image (`images/sandbox-desktop/`) and the `nilcore-desktop` driver are separate artifacts nothing pulls by default; nothing here becomes a hard requirement.
5. **Dependency budget justified (`I6`).** No new Go module in the core, and the always-compiled core tool stays a **thin, stdlib-only pass-through**. All heavy work lives in the in-image `nilcore-desktop` driver, which may shell to **operator-trusted, image-baked** binaries (`xdotool`/`scrot`/a window manager/`at-spi2`) — those live in the image, not in `go.mod`; the Go core stays stdlib-only and `CGO_ENABLED=0`. Screenshot resize-to-pixel-budget and coordinate-rescale are pure-stdlib `image`/`image/draw`, performed in the driver. Any deviation is justified in both the PR and the CHANGELOG.

If any of these cannot be met, the item stays on this roadmap, **unbuilt**.

> **Relationship to `EXT`.** Desktop computer use shares the gate philosophy of `EXT-01..08` and **depends on `EXT-08`** (the microVM isolation tier) for its strong-assurance form. It is registered here as its own tier rather than an `EXT` item because, unlike the EXT set, it requires **no standing external infrastructure** — it runs entirely on one host, in-sandbox. Its gate is about **identity and threat-surface**, not cloud authority.

---

## 1. The capability — what it adds, precisely

The canonical 2025–2026 desktop-computer-use loop (Anthropic Computer Use, OpenAI CUA/Operator, Google Gemini 2.5 Computer Use) is the **same bounded, application-driven loop** as browser-use — the model emits action intents, the harness executes them and returns the next observation — differing only in **perception** and **actuation** (X11 mouse/keyboard against a virtual display instead of CDP against a DOM). *(NilCore's adaptation, §0a: perception is **AT-SPI set-of-marks first**, screenshot fallback — not pixels-first like the reference loop below; the diagram shows the screenshot path, which is the fallback.)*

```
 user goal ─▶ TRUSTED PLANNER ─▶ model emits ONE computer action {screenshot|left_click[x,y]|type|key|scroll|zoom|…}
                                          │
                                          ▼
              HOST HARNESS (NilCore, application-as-executor, bounded+logged)
                • Rule-of-Two + policy gate on consequential actions   • NEVER auto-acknowledge a gate
                • rescale model coords → true virtual-screen pixels      • {{secret}} stays host-side (I3)
                                          │  one action over a control channel
                                          ▼
              SANDBOX (I4) — a CONTAINED virtual desktop: Xvfb X11 display +
              lightweight WM + apps; default-deny egress; NEVER the host screen
                                          │  scrot screenshot, resized to the model's pixel budget
                                          ▼
              OBSERVATION → screenshot as model.ImageBlock, guard.Wrap-fenced (I7)
                                          └────▶ back to model … until finish or step cap
                                                 then verify.Check governs "done" (I2)
```

This is exactly Anthropic's reference architecture (the `anthropic-quickstarts/computer-use-demo`: Ubuntu + **Xvfb** virtual X11 + **Mutter** WM + **Tint2** panel + x11vnc/noVNC, all inside Docker) and E2B Desktop (Xfce on a Firecracker microVM, ~150ms cold start). The model controls a **disposable virtual screen**, never the operator's machine — which is what preserves I3/I4. *(Sources: platform.claude.com computer-use-tool; github.com/anthropics/anthropic-quickstarts computer-use-demo; github.com/e2b-dev/desktop; e2b.dev/blog Manus; northflank.com Firecracker-vs-gVisor.)*

**What it unlocks that browser-use cannot:** driving native desktop apps (an installed IDE/CLI GUI, LibreOffice), canvas/non-DOM web surfaces (Figma, Google Sheets, Canva) where the accessibility tree is empty, and OS-level flows. **What it costs:** screenshots are 20–50× more tokens than a11y snapshots; generalist VLMs ground raw pixels poorly (**<2% on ScreenSpot-Pro**, tiny targets avg 0.07% of screen); coordinate-rescale bugs are the #1 mis-click cause; and the virtual desktop is a large surface needing microVM-grade isolation. That cost/benefit is exactly why this is gated and browser-use is do-now.

---

## 2. How it honors the invariants (the reconciliation)

| Invariant | How desktop CU **extends** it (not bypasses) |
|---|---|
| **I1** backend contract frozen | The desktop session rides **out-of-band** of `backend.Task`/`Result` (like artifacts/browse). The unit of work is still `Run(ctx,Task)`. **The default (generic-tool) path needs ZERO contract change** — it reuses the shipped multimodal `ImageBlock`/`ImageRunner` seam (P9-T01) with our own tool schema. The ONLY contract-touching change is the **optional** native Anthropic `computer` beta tool (`internal/model`/`internal/provider/anthropic`), a **dedicated, serialized** task (`CU-T12`) gated behind `NILCORE_COMPUTER_NATIVE` — never on the default path, never a side effect. |
| **I2** verifier is sole authority | The loop never declares done; `verify.Check` does. A desktop behavioral verifier (re-derive the asserted end-state in-box) folds into `verify.Composite` like the browser check. |
| **I3** no ambient authority | Secrets stay in `SecretStore`; `{{secret:…}}` placeholders only; **takeover** pattern for logins. No host credentials in the virtual desktop. |
| **I4** model-emitted execution sandboxed | The **entire desktop** (Xvfb + WM + apps + the `nilcore-desktop` driver) runs in the sandbox — container or, for strong assurance, the `EXT-08` microVM. The model drives a **virtual** screen; the host display is never touched. Default-deny egress + scoped allowlist. |
| **I5** append-only audit | Every screenshot hash, action, gate decision appends to the hash-chained log; deterministic replay. (Screenshots can hold sensitive pixels — retention policy + the ZDR-eligibility note apply.) |
| **I6** zero-dependency core | No new Go module. The driver shells to **image-baked** `xdotool`/`scrot`/WM (operator-trusted, not `go.mod`). Screenshot resize to XGA is pure-stdlib (`image`/`image/draw`). |
| **I7** untrusted input boundary | Every screenshot/window-title/clipboard read is `guard.Wrap`-fenced data. Plan-then-verify (planner blind to screen content) carries over from browse — injected on-screen text cannot rewrite control flow. |

**Net:** desktop computer use is *implementable without weakening a single invariant* — the gate is about whether NilCore should *be* this, not whether it *can* do it safely.

---

## 3. The task DAG (`CU-T##`) — BLOCKED behind §0

Every task below is **blocked** until the §0 gate is recorded. The structure mirrors `docs/SWARM.md` / `docs/EXT-EXECUTION-PLANS.md` depth so that, *if* a human clears the gate, the work executes completely without re-deriving the design. Owns are package-directory granular and disjoint per wave.

The DAG reflects the §0a converged two-path design. **The default critical path (`CU-T01→T04→T05→T07→T08→T09`) carries ZERO frozen-contract change** — Path B reuses `model.Tool`/`ImageRunner` as-is. The lone contract task is **`CU-T12` (Path A)**, off the critical path behind `NILCORE_COMPUTER_NATIVE`. The browse leaves we shipped (`capguard`, the `ImageBlock` seam, `browsersession`'s file-queue, the loop discipline in `browseragent`, the `eval/browse` harness, the Phase-13 trust ledger) are **reused, not re-derived** — file:line in §0a. Owns are package-granular and disjoint across *concurrently-open* tasks; the dependency chain serializes same-dir tasks (e.g. `internal/desktop` is built up across T05→T07→T08 in order, never in parallel).

| ID | Wave | Title | Depends on | Owns | Notes |
|---|---|---|---|---|---|
| **CU-T00** | gate | **Record the §0 thesis-gate decision** (human owner) | — | — (PR record) | **the gate; not delegable to the agent** |
| CU-T01 | 1 | Desktop sidecar image: Xvfb + WM + `at-spi2`/`dbus-x11` + `xdotool` + `scrot` + GTK/Qt apps; a11y env (`QT_ACCESSIBILITY=1`, `GTK_MODULES=…atk-bridge`, `gsettings toolkit-accessibility`) | CU-T00 | `images/sandbox-desktop/` | image-baked binaries, **not `go.mod`** (I6); nothing pulls it by default |
| CU-T02 | 1 | Desktop wire contract — `Observation{Refs,ScreenshotB64,…}` / `Ref{ID,Role,Name,Box}` / closed `Act` set (sibling of `browserwire`) | CU-T00 | `internal/desktopwire` | pure-stdlib leaf, UNTRUSTED-tagged (I7); no contract change |
| **CU-T03** | 2 | **Thin inert `computer_use` core tool** — generic `{action,ref?,coordinate?,text}` schema reusing `model.Tool` **as-is**; implements `ImageRunner` so screenshots ride `model.ImageBlock` via `DispatchRich`; inert unless `NILCORE_COMPUTER_USE` set | CU-T02 | `internal/tools/computer.go` | **default critical · zero contract change**; shares `internal/tools` — serialize |
| **CU-T04** | 2 | `nilcore-desktop` driver skeleton + file-queue session (sibling of `browsersession`); `{{secret}}` host-side (I3); capture via `scrot` in-sandbox (I4); **hand-rolled minimal D-Bus** (no `godbus` in core — I6, like the CDP client) | CU-T02 | `cmd/tools/nilcore-desktop`, `internal/desktopsession` | **default critical · the fat half** |
| **CU-T05** | 3 | **Rung 1 — AT-SPI set-of-marks** (D-Bus walk → filter `SHOWING&&SENSITIVE` → `GetExtents(SCREEN)` → numbered refs); `Action.DoAction`-first actuation (DPI-independent), `xdotool` fallback; re-query extents at action time | CU-T04 | `internal/desktop/atspi.go` | **default critical · best rung**; driver-side, core untouched |
| CU-T06 | 3 | **Stdlib SoM overlay** — `Mark{ID,Box,Role,Label}`, `Overlay(img,marks)→(marked,id→box)` via `image/draw`/`png` + embedded 5×7 digit bitmap (**zero module**, I6); `id→box` logged (I5) | CU-T02 | `internal/som` | pure-stdlib leaf, host-side deterministic; shared by Rung 2 |
| **CU-T07** | 4 | **Rung 2 — SoM screenshot fallback** — box sources: (a) AT-SPI extents via `internal/som`, (b) classical-CV proposals (grayscale→edges→dilation→CC-label, **pure Go, no ML/module**); resize-to-budget + marked `ImageBlock`; mark-cap + occlusion prune | CU-T05, CU-T06 | `internal/desktop/detect.go` | **default critical · fallback rung** |
| **CU-T08** | 4 | **Rung 3 + the ladder decision** — per-step rung select against focused window + short per-window cache (invalidate on focus/resize/stagnation); Rung-3 raw-coordinate with resized→true-pixel rescale in **one place**; reuses the loop stagnation detector | CU-T07 | `internal/desktop/ladder.go` | **default critical · last resort + trigger logic**; no contract change |
| **CU-T09** | 4 | `nilcore desktop` subcommand + `NILCORE_COMPUTER_USE` wiring + session lifecycle + **`capguard.Evaluate` + `policy.GateStructured`** for the desktop session (mirror `cmd/nilcore/browse.go`); **never auto-acks a gate** | CU-T04, CU-T08 | `cmd/nilcore/desktop.go` | **default critical**; shares `cmd/nilcore` — serialize |
| CU-T10 | 5 | **eval/desktop flywheel** — reuses the `eval/browse` harness unchanged; OS-task scenario catalog + desktop faults (DPI change, mid-task resize, a11y-empty, sparse-tree-lies); pass@1 + pass^k gate every grounding change | CU-T08 | `eval/desktop` | improvement engine (d); `eval/browse` untouched |
| CU-T11 | 5 | **Register `"desktop"` backend into trust-ledger routing** — add to `Orchestrator.Backends`; existing `trust.Selector`/`agent.TrustOracle` re-rank it from fresh `race_outcome` (**zero wiring change**); routes the grounding sub-task to the strongest wired backend | CU-T09 | `cmd/nilcore/desktop_route.go` | improvement engine (c); verifier still governs (I2) |
| CU-T12 | 5 · **opt** | **Path A — native Anthropic `computer` beta tool** (the lone contract task): `model.BuiltinTool` + provider emits `anthropic-beta: computer-use-2025-11-24` + matched `computer_20251124`/model triple, **only when a builtin tool is present**; generic path never affected | CU-T08 | `internal/model/builtin.go`, `internal/provider/anthropic.go` (native branch) | **contract · solo · off the default path**; `NILCORE_COMPUTER_NATIVE` only |
| CU-T13 | 6 | Staging-doc consolidation | CU-T00 | `docs/ROADMAP-COMPUTER-USE.md` | this file |
| CU-T14 | 6 | Promotion into the canonical DAG + `EXT-08` microVM cross-reference | CU-T01…T13 | `docs/{TASKS,ARCHITECTURE}.md`, `CLAUDE.md`, `CHANGELOG.md` | **contract · serialized** |

**Strong-assurance variant:** CU-T01's container image is the *baseline*; the strong form runs the same desktop inside the `EXT-08` Firecracker microVM — gate `EXT-08` first for sensitive/unattended runs. The default critical path ships the capability **without** CU-T12 (native), CU-T11 (routing), or EXT-08 (microVM); all three are additive. Path B is usable the moment CU-T09 lands.

---

## 4. Multi-worker parallel execution plan (when, and only when, the gate clears)

Same `CLAUDE.md` §5 worktree-per-task protocol as Phase 14. **CU-T00 is a human decision and blocks everything.**

```
GATE   ──▶  CU-T00  (human records the §0 decision — no code)
Wave 1 ──▶  CU-T01 (images/sandbox-desktop)  ∥  CU-T02 (internal/desktopwire)        2 agents
Wave 2 ──▶  CU-T03 (internal/tools: thin computer_use)  ∥  CU-T04 (driver + desktopsession)   2 agents
Wave 3 ──▶  CU-T05 (internal/desktop: AT-SPI Rung 1)  ∥  CU-T06 (internal/som: SoM overlay)    2 agents
Wave 4 ──▶  CU-T07 (Rung 2 detect) ──▶ CU-T08 (Rung 3 + ladder)   ∥   CU-T09 (cmd/nilcore/desktop.go)
Wave 5 ──▶  CU-T10 (eval/desktop)  ∥  CU-T11 (desktop_route)  ∥  CU-T12 (Path A native — CONTRACT/solo, OFF default)
Wave 6 ──▶  CU-T13 (staging doc)  ──▶  CU-T14 (promotion, CONTRACT)
```

- **The DEFAULT path carries NO frozen-contract change.** Path B's `computer_use` tool reuses `model.Tool`/`ImageRunner` as-is, so the critical path never touches `internal/model`/`internal/provider`. That is the payoff of the §0a generic-tool-default decision — and it's why most of the DAG can fan out without serializing on the contract.
- **CU-T12 (Path A) is the lone contract task** (`internal/model` built-in tool + `internal/provider/anthropic` beta branch) — serialized, never parallel with another task reading the model contract, built only if a user opts into `NILCORE_COMPUTER_NATIVE`. It can start any time after CU-T08.
- **`internal/desktop` is built up sequentially** (T05 atspi → T07 detect → T08 ladder) by the dependency chain, so the three never run concurrently despite sharing the dir. `cmd/nilcore` (T09, T11) likewise serializes.
- **No CU task adds a Go module (I6).** AT-SPI is a hand-rolled D-Bus slice in the driver; the only module-tempting fallback (`godbus` in a *separate* driver module) is Plan B, recorded in §0a. Contract files (`docs/TASKS.md`, `docs/ARCHITECTURE.md`, `CLAUDE.md`, `go.mod`, `Makefile`) are touched only by **CU-T14**, last.
- **Critical path (default capability):** CU-T00 → CU-T02 → CU-T03/T04 → CU-T05 → CU-T07 → CU-T08 → CU-T09. Path B is usable when CU-T09 lands; CU-T10/T11 (eval + routing) and CU-T12 (native) and EXT-08 (microVM) are all additive.

---

## 5. Best practices baked in (sourced)

- **Climb the Set-of-Marks ladder; only fall to raw coordinates as a last resort** (§0a). Rung 1 (AT-SPI refs + `Action.DoAction`) is DPI/resolution-independent and needs zero pixel math; Rung 2 (marked screenshot) keeps the model on "pick `[N]`"; Rung 3 (raw `{x,y}`) is the floor. The unifying "pick a numbered element" contract is the single biggest reliability lever (CU-T05/T07/T08).
- **Decide the rung per-step against the focused window**, cached per-window and invalidated on focus/resize/stagnation — a single task mixes a rich GTK dialog, an empty Electron pane, and a canvas, so a per-session latch either mis-targets or overpays (CU-T08).
- **Filter the a11y tree to `SHOWING && SENSITIVE` interactive roles** before sending — raw trees are ~5× too verbose and blow the token budget (CU-T05).
- **Re-query AT-SPI extents (and re-snapshot) at action time** — boxes go stale during scroll/animation; never act on a stale snapshot.
- **Resize + rescale in ONE place (the driver), and only on Rung 3 / Path A** — declared display dims MUST equal the image sent; per-model pixel ceilings (Opus 4.x ≤2576 long edge); the API 400s on oversize, never auto-resizes. Coordinate mismatch is the #1 mis-click bug (CU-T08/T12).
- **Observe-and-verify after each step; single-snapshot retention; bound the loop** — reuse the `browseragent` budgets/stagnation discipline (CU-T08).
- **Measure every grounding change** through `eval/desktop` (pass@1 + pass^k under desktop faults) before it ships — improvement is earned, not asserted (CU-T10).
- **(Path A only)** honest display dims, the matched `computer_2025…`/beta-header/model triple, and `is_error` on out-of-bounds (CU-T12).

## 6. Pitfalls to avoid (sourced)

- **Auto-acknowledging a safety/confirmation check in code** — the single most common deployment mistake; it nullifies the human gate. NilCore **never** auto-acknowledges (CU-T09).
- **AT-SPI coverage is a structural hole, not an edge case** — Electron/Chromium (VS Code ignores `--force-renderer-accessibility`), Java/Swing, and canvas/WebGL/games expose poor or zero trees, exactly the surfaces that justify desktop CU. Expect frequent Rung-2/3 fallback; don't assume Rung 1 (CU-T05/T07).
- **Over-trusting a sparse/lying tree** — a non-empty tree can still lack the one widget the step needs → silent mis-target. The stagnation-based 1→2 drop trigger mitigates but costs two wasted steps (CU-T08).
- **Classical-CV boxes are weak** (over/under-segment, miss flat icons, no semantics) — SoM fixes *which box*, not *what it is*; without OCR (refused for I6) the model still reads ambiguous icons from pixels (CU-T07).
- **Wayland / macOS unsupported** — the design is Linux-X11 (`xdotool`/`scrot`); verifiable only in-container, CI-only on a macOS dev host (CU-T01/T05/T09).
- **Path A's injection classifier can inject confirmation turns with no API opt-out** — an unattended Path-A handoff must tolerate them; the per-model pixel ceiling beyond Opus 4.x isn't enumerated, so don't hardcode (CU-T12).
- **Perception sophistication ≠ reliability** — OSWorld shows a11y+screenshot gives only *modest* gains, agents stay far below human; the **verifier (I2)**, not tree richness, is the only authority on done (CU-T10).
- **Treating a container as VM-equivalent** — a desktop escape from a shared kernel is a host compromise; use the `EXT-08` microVM for unattended/sensitive (§3).
- **Secrets in the prompt/screenshot stream** — `{{secret}}` host-side, takeover for logins; secrets never enter the sandbox, driver, screenshot, or prompt (CU-T04/T09).
- **Xvfb/display crashing inside the desktop** — supervise/restart the display stack; the driver fails closed if the display is down (CU-T04).

---

## 7. Bottom line

Desktop computer use is **buildable on NilCore's existing spine without weakening any invariant** — the virtual desktop lives in the sandbox (I4), secrets stay host-side (I3), the verifier still governs (I2), every action is logged (I5), screen content is data (I7), and the **default path adds no Go module and no contract change** (I1/I6). It ships as **two front-ends over one governed body**: **Path B**, the vendor-independent workhorse we own and compound forever — a 3-rung **Set-of-Marks ladder** (AT-SPI refs → marked screenshot → raw coordinate) that keeps the model on "pick `[N]`", improving without retraining via SoM/AT-SPI depth, trust-ledger model-routing, and a measured eval flywheel; and **Path A**, the borrow-able Anthropic frontier ceiling for the hardest pure-pixel cases, fully matched but opt-in and the lone contract task.

It is **gated** purely because it is an **identity-and-threat-surface decision** that belongs to a human, and because **browser-use (CDP + accessibility tree) is the strictly better first modality** for what NilCore does. Build [`ROADMAP-BROWSER-USE.md`](ROADMAP-BROWSER-USE.md) (shipped); keep this blueprint ready; open `CU-T00` only when a real native-app/canvas need and a recorded §0 decision both exist. The default capability is usable the moment `CU-T09` lands — Path A, routing, and the microVM are all additive on top.
