# Roadmap — Native macOS desktop control (GATED tier `CU-MAC-T##`)

**Driving the *real* macOS desktop — AXUIElement + CGEvent + ScreenCaptureKit — quarantined behind a higher gate than even the contained-Linux desktop, because it touches the user's own machine.**

This is the **darwin sibling** of the shipped contained-Linux desktop computer use (`docs/ROADMAP-COMPUTER-USE.md`). The crucial difference, stated up front: the Linux tier drives a **disposable virtual desktop inside a sandbox** (I4 clean); native-macOS drives **a real Mac desktop** — either a disposable **guest VM** (the recommended, I4-compliant path) or, behind a louder gate, the **user's own host** (where I4 is *broken by construction*). That asymmetry is the whole reason this is a separate, more-gated doc.

> **The reframe (from `ROADMAP-COMPUTER-USE.md` §0a still holds):** computer use is a *new perception+actuation backend behind the same loop*. So native-macOS is **one new fat driver** — `cmd/tools/nilcore-desktop-darwin` — behind the **unchanged** `desktopwire` contract. `internal/{desktopwire,desktopsession,desktopagent,som,desktop}` are reused **byte-for-byte, zero edits** — they speak only `Observation`/`Act` JSON and `id→Box` math, all OS-agnostic. The darwin work is: a driver, a signed native helper, a transport variant, and a darwin gate. **The Go core links none of it.**

> **Staging doc, gated.** Like its Linux sibling and the `EXT-NN` tier, this presumes the seven invariants and the frozen `backend.CodingBackend` contract and never restates them. Every task is additive; the Go core and the Go driver stay **`CGO_ENABLED=0`, zero-module (I6)** — *all* native code is quarantined in a separately-compiled signed helper outside the Go build graph. Promotion into `docs/TASKS.md` is a serialized contract task (`CU-MAC-T11`).

---

## 0. The gate — native-macOS-specific, layered on the §0 desktop gate

Everything in `ROADMAP-COMPUTER-USE.md` §0 applies (a recorded human thesis decision, etc.), **plus** these darwin-specific conditions, because driving a real Mac is a categorically larger grant:

1. **Two-flag opt-in.** `NILCORE_COMPUTER_USE` (inert-when-off, as today) **plus** a darwin flag `NILCORE_DESKTOP_DARWIN`. **Host-control mode needs a third, separate, louder flag `NILCORE_DESKTOP_HOST=1`** — so a VM-shaped run can *never* silently become host control.
2. **OS-level consent is the real gate (TCC).** The signed helper must hold **Accessibility** *and* **Screen Recording** grants — user-consented in System Settings, unforgeable, revocable. This *is* the no-ambient-authority gate at the OS layer; it cannot be scripted or entitled around.
3. **VM is the default and the only path that claims I4-compliance.** Host-control mode is the gated, non-default tier and must force **unconditional console approval** (`capguard` GateRequired regardless of axes), a **per-app-bundle allowlist** of what the model may drive, a global **Esc kill-switch**, and exclusion of the controlling terminal from screenshots.
4. **Isolation-gate handshake.** In VM mode the driver refuses to start unless the transport confirms it is talking to a **guest** (a guest-identity handshake), so a misconfiguration can't fall through to host control.
5. **Accept the scaling ceiling.** Apple's EULA + kernel cap = **max 2 concurrent macOS guests per physical Mac**, no nested virtualization, **Apple-Silicon only**. Native-macOS does **not** fan out like Linux containers and cannot run on a cloud Linux box — it needs bare-metal Apple hardware. The gate owner must accept this.

If any cannot be met, the tier stays unbuilt.

---

## 0a. Architecture — a darwin driver + a signed helper + a transport variant

### What is reused (everything but the driver)

`desktopwire` (contract), `desktopsession` (the file-queue Session: stale-ref guard + `{{secret}}` substitution), `desktopagent` (the thin `computer`/`NativeComputerTool`), `internal/som` (the SoM overlay), and `internal/desktop` (`Detect` + the rung `Ladder`) are **OS-agnostic and reused unchanged**. *If you find yourself editing `desktopwire`, stop — the contract is shared and that is a serialized change.*

### The new fat driver — `cmd/tools/nilcore-desktop-darwin`

It implements the **exact same `serve.go` file-queue protocol** as the Linux driver (`req-N.json`/`resp-N.json`/`ready`, the `OpObserve/Click/Type/Key/Scroll/Wait/Close` set, the 3-rung ladder, the rescale-in-one-place). Its three live seams replace the Linux ones one-for-one:

| Seam | Linux driver | Darwin driver |
|---|---|---|
| `capture()` | `scrot` | `/usr/sbin/screencapture -x -t png -R x,y,w,h` (Apple-maintained; survives the macOS-15 `CGWindowListCreateImage` obsoletion; historically no Screen-Recording prompt on the CLI) |
| `runInput()` | `xdotool` | the signed **`nilcore-mac-helper`** (`click x y` / `type` / `key` / `scroll` → CGEvent). `cliclick` is the zero-helper MVP substitute |
| `dumpA11y()` | `nilcore-a11y-dump` (AT-SPI) | `nilcore-mac-helper dump-tree <pid>` → walks **AXUIElement**, prints the **same `a11yNode` JSON** `{role,name,value,box,actions}` so **`parseA11y` is reused verbatim** |

**Actuation maps cleanly:** a Rung-1 ref-click becomes `nilcore-mac-helper press <axpath>` = `AXUIElementPerformAction(kAXPress)` — DPI-independent, no cursor move, no focus steal — falling back to a CGEvent coordinate click at the frame centre when the element exposes no `AXPress`. (This is the macOS analog of the Linux `Action.DoAction`-first rule.)

### The CGO strategy — quarantine all native code behind a process boundary

There is **no pure-Go path** to CGEvent (input), ScreenCaptureKit (fast capture), or AXUIElement (the a11y tree) — `robotgo` confirms macOS has no CGO-free backend; `purego`/`darwinkit`/`kinax-go` are immature, add a **module** (breaks I6), and hit hardened-runtime library-validation. **So: the Go core *and* the Go driver stay `CGO_ENABLED=0` stdlib-only; the only native artifact is `nilcore-mac-helper`, built with its own Xcode/clang toolchain, *outside* the Go build graph, never in `go.mod`, signed independently, and shelled-to over `os/exec`** — identical to how `scrot`/`xdotool`/`nilcore-a11y-dump` and the hand-rolled CDP client keep the Linux driver module-free. Putting CGO in the Go driver would force `CGO_ENABLED=1` on `go build ./...` and contaminate the whole-repo release posture — **refused.** Two tiers:

- **MVP (zero custom native):** driver shells to `screencapture` + `cliclick` (+ `osascript` for deterministic app steps). Proves the permission/onboarding flow and the whole vertical slice before any native code exists. *Limitation:* no AX-tree reading → the ladder runs at **Rung 2/3 only** (weaker than the Linux driver).
- **Production (recommended):** one small (~200–400 LOC) Swift/ObjC `nilcore-mac-helper`, **Developer-ID-signed + notarized + stapled, hardened runtime**, owning AXUIElement (`dump-tree`/`press`/`set-value`), CGEvent (input), and capture, over a versioned JSON stdio protocol. **It is the entity that holds the TCC grants** — so the ephemeral per-task Go binary is never the TCC subject (critical: TCC keys on code-signature identity, and a rebuilt unsigned Go binary loses its grant every build). This restores **Rung 1**.

### Isolation — the disposable macOS guest VM is the I4-compliant default

Prefer a **dedicated, disposable macOS guest VM** (Apple **Virtualization.framework**, driven by a baked `Tart`/`Lume` CLI or a tiny signed `vmctl`) — the desktop analog of NilCore's disposable worktree/container, and the **only** design consistent with I3/I4. **Clone a golden image per session, destroy on exit.** The golden image bakes the Accessibility + Screen-Recording TCC grants **once**, so clones inherit them and the agent never prompts and never touches the host's TCC database. The in-guest `nilcore-mac-helper` is the "computer server"; the host Go core reaches it over **vsock/ssh** (not a `/work` bind-mount) and links zero macOS frameworks. Default the guest to **network-deny + the existing `egressprofile` allowlist**.

**Host-control mode** (driving the user's real desktop) is *offered but strictly gated and non-default* — it has **no disposable boundary**: a model-driven CGEvent stream can touch anything the user can (keychain, Messages, banking). It exists only for tasks that genuinely need the user's real signed-in apps, behind `NILCORE_DESKTOP_HOST=1` + forced approval + per-app allowlist.

### The macOS Set-of-Marks ladder (a direct remap — `som`/`desktop` reused)

The model-facing contract never changes — **"pick numbered element `[N]`"**. Only the rung data-sources swap:

- **Rung 1 — AXUIElement refs.** The helper roots at `AXUIElementCreateApplication(pid)`, walks `kAXChildrenAttribute`, and **batch-reads** (`AXUIElementCopyMultipleAttributeValues` — mandatory for perf, one IPC/node) `kAXRole/kAXSubrole/kAXTitle|kAXDescription/kAXValue/kAXEnabled/kAXPosition+kAXSize`, unpacking into a `{x,y,w,h}` frame in **global screen coords**; filters to actionable, enabled, on-screen, non-zero-frame leaves; emits the **same `a11yNode` JSON** the Linux dump emits. Re-query extents at action time (boxes go stale).
- **Rung 2 — SoM-annotated screenshot.** `buildMarks` reused verbatim: AXUIElement frames (with role/name) first, then `internal/desktop.Detect` classical-CV proposals (pure-Go, zero module) fill the rest; `som.Overlay` draws numbered boxes + embedded-bitmap digit badges. **Darwin delta:** AXUIElement frames are in **points**, the screenshot is in **pixels** — `scaleBoxToResized` must convert **points→pixels (×backingScaleFactor)** *before* scaling into the resized image, or every box is half-placed on Retina.
- **Rung 3 — raw coordinate** (canvas/Metal/games, Electron with empty AX). Model emits `{coordinate:[x,y]}` in resized space; the driver rescales **resized→pixels→points** (one place; now a **two-stage** map: `/resizeScale` then `/backingScaleFactor` + display origin). This is where Path A (`NILCORE_COMPUTER_NATIVE`, already shipped) is strongest — hand the single grounding sub-call to Anthropic's native tool when set.

**Coverage reality (the honest part):** AppKit-native + Catalyst apps give **rich Rung-1 trees** (the happy path browser-use can't reach); **Electron** (Slack/VS Code/Teams) gives an empty `AXWebArea` (`AXManualAccessibility` often returns `kAXErrorAttributeUnsupported`) → auto-degrade to Rung 2/3 via the existing stagnation/sparse-tree trigger; **games/canvas** have zero tree → Rung 3 only. Probe tree richness at session start to pre-classify per app.

---

## 1. Per-invariant reconciliation (honest about what breaks)

| Inv | Verdict |
|---|---|
| **I1** backend contract | **HOLDS.** Same `desktopwire` JSON + same `desktopagent` tool over `backend.Native`. No `Task`/`Result`/`CodingBackend` change. Path A's `BuiltinTool` (already shipped, CU-T12) is reused as-is. |
| **I2** verifier governs done | **HOLDS.** `-check` runs `behavioralVerifierWithLog`; the model's finish never decides. *Caveat:* a host-mode behavioral check would run on the host — keep `-check` **VM-only** (or run it inside the guest). |
| **I3** no ambient authority | **HOLDS in VM, STRESSED in host.** `{{secret}}` stays host-side, reused. But host control gives the agent the user's whole logged-in session = de-facto ambient authority the SecretStore never granted. *Mitigation:* VM-isolate (golden image holds only task-scoped logins); host mode hard-gated + per-app allowlist + secure-input exclusion (macOS secure-input fields silently swallow synthetic keystrokes — a *side benefit*). |
| **I4** model-emitted execution sandboxed | **THE LOAD-BEARING TENSION. HOLDS in VM** (the disposable guest *is* the sandbox; the host core links nothing native). **BROKEN in host mode** by construction — CGEvent on the real desktop is unsandboxed actuation against the user's machine; the structured-tool exception does **not** cover arbitrary GUI effect. *Mitigation:* VM is the default and the only I4-compliant path; host mode is an explicitly-recorded human decision, never silent. |
| **I5** append-only audit | **HOLDS, strengthened.** `desktopEventSink` reused; darwin **adds** coordinates in *both* pixel+point space, the `id→box` table, the helper RPC + screenshot hash, and TCC/grant-probe results. |
| **I6** zero-module / CGO-free | **STRESSED but HELD.** No pure-Go macOS path exists, so a native artifact is unavoidable — but **all** native lives in the separately-compiled, not-in-`go.mod`, signed helper (or OS-baked `screencapture`/`cliclick`), shelled-to. `go.mod` unchanged; `CGO_ENABLED=0 go build ./...` stays green. **Do NOT import `purego`/`darwinkit`/`robotgo`/`Code-Hex/vz`** — each breaks I6. |
| **I7** untrusted input is data | **HOLDS, reused.** AX titles/values/screenshots from arbitrary apps are SCREEN-controlled UNTRUSTED, `guard.Wrap`-fenced; sanitize before the prompt exactly as Linux. |

**Net:** native-macOS is implementable **without weakening any invariant *in VM mode*.** Host mode deliberately relaxes I3/I4 and is therefore the louder-gated, non-default tier. That is the entire reason it ranks as a higher-risk tier than the contained-Linux desktop.

---

## 2. The task DAG (`CU-MAC-T##`) — BLOCKED behind §0

The MVP path (`T01→T02→T03→T04`) ships a Rung-2/3 capability with **zero custom native code**; `T05/T06` add the signed helper (restoring Rung 1); `T08/T09` add the VM and the host gate. Owns are package-granular and disjoint per wave.

> **Status (MVP SHIPPED — `make verify` green, validated on a real Mac).** The §0 gate was cleared by the operator. **`T01–T04` are BUILT** (`cmd/tools/nilcore-desktop-darwin`: serve loop + file-queue, `coords.go` two-stage map, `screencapture` capture, `cliclick` input — all hermetically unit-tested), and the **host transport half of `T09` is BUILT** (`internal/desktopsession/hosttransport.go` + `LaunchHost`, exercised by a real-subprocess `TestMain` round-trip) and **wired** (`nilcore desktop --mac-host`, doubly gated by `NILCORE_DESKTOP_HOST=1` + forced approval). `make desktop-mac` builds the driver; `make desktop-mac-smoke` drives a live observe and confirmed the full vertical (build → serve → file-queue → observe → capture seam → honest fail-closed when Screen Recording TCC is ungranted). **Still open:** `T05–T08`, `T10`, `T11` (the signed helper restoring Rung 1, the VM as the I4-compliant default, the per-app allowlist/kill-switch, darwin eval, canonical promotion).

| ID | Wave | Title | Depends | Owns | Note |
|---|---|---|---|---|---|
| **CU-MAC-T00** | gate | Record the §0 + darwin-gate decision (human) | — | — (PR) | ✅ **DONE** — operator cleared the gate; host-control MVP first, VM as the compliant default to follow |
| CU-MAC-T01 | 1 | Darwin driver skeleton (serve loop, file-queue, flags) cloning the Linux driver's pure pieces | T00 | `cmd/tools/nilcore-desktop-darwin/{main,serve}.go` | ✅ **BUILT** — protocol reused; live seams are vars; hermetic assembly tests; `CGO_ENABLED=0` |
| CU-MAC-T02 | 1 | Coordinate model — points↔pixels↔resized, display-keyed (backingScaleFactor + origin), in ONE place | T00 | `…/coords.go` | ✅ **BUILT** — pure, unit-tested at 1×/2×/multi-monitor; the #1 mis-click bug owned once |
| CU-MAC-T03 | 2 | `screencapture` capture seam + Retina-aware downscale for native mode | T01,T02 | `…/capture.go` | ✅ **BUILT** — `screencapture` seam + backingScale detect (env→osascript→2.0) + stdlib resize; pure-tested |
| CU-MAC-T04 | 2 | **MVP input via `cliclick`** (zero custom native) — Rung 2/3 click/type/key/scroll | T01,T02 | `…/input.go` | ✅ **BUILT** — pure cliclick builders unit-tested (click/type/key-chord/scroll); fails closed without cliclick |
| CU-MAC-T05 | 3 | **`nilcore-mac-helper`** — signed Swift/ObjC helper (AXUIElement dump/press/set-value + CGEvent + capture) over JSON stdio | T03,T04 | `helpers/nilcore-mac-helper/` (**outside the Go build graph**) | ~200–400 LOC; not in `go.mod`; Developer-ID signed + notarized + stapled; emits the same `a11yNode` JSON; **holds the TCC grants** |
| CU-MAC-T06 | 4 | Wire the helper: `dumpA11y`→`dump-tree`, `runInput`→helper, AXPress-preferred actuation | T05 | `…/a11y.go`, `…/helper.go` | reuse `parseA11y` verbatim; restores the full 3-rung ladder |
| CU-MAC-T07 | 4 | Permission/onboarding probe — live TCC check + posted-but-no-effect detection | T05 | `…/permissions.go` | `probe` cmd (live state, not the cached `AXIsProcessTrusted`); fail closed with a "grant …, then restart" message |
| CU-MAC-T08 | 5 | VM provisioning + guest transport — golden image (Tart/Lume/Virtualization.framework) with grants baked, `vmTransport` behind `desktopsession.transport` | T06,T07 | `internal/desktopsession/vmtransport.go`, `images/desktop-darwin-vm/` | clone-per-session/destroy-on-exit; reach in-guest driver over vsock/ssh; guest-identity handshake; **the I4-compliant default** |
| CU-MAC-T09 | 5 | Host-control transport + the darwin/host gate (`NILCORE_DESKTOP_HOST`, forced approval, per-app allowlist, kill-switch) | T06,T07 | `internal/desktopsession/hosttransport.go`, `cmd/nilcore/desktop.go` | 🟡 **PARTIAL** — host transport + `LaunchHost` + `--mac-host` gate BUILT (`NILCORE_DESKTOP_HOST=1` + forced approval); per-app allowlist + kill-switch still open |
| CU-MAC-T10 | 6 | `eval/desktop-darwin` — reuse the `eval/browse` harness with darwin faults (backingScale change, resize, empty-AX Electron, sparse-tree-lies) | T08 | `eval/desktop-darwin/` | gate every ladder/helper improvement on pass@1 + pass^k; CI = a Mac runner + a guest |
| CU-MAC-T11 | 6 | Promotion into the canonical DAG + this doc consolidation | T01…T10 | `docs/{TASKS,ARCHITECTURE}.md`, `CLAUDE.md`, `CHANGELOG.md` | **contract · serialized** |

**Multi-worker plan:** Wave 1 `T01 ∥ T02` (disjoint files); Wave 2 `T03 ∥ T04`; Wave 3 `T05` (the Swift helper — a different toolchain, ownable by a specialist); Wave 4 `T06 ∥ T07`; Wave 5 `T08 ∥ T09` (disjoint transports); Wave 6 `T10 → T11`. **No Go module is ever added** (the helper is outside the Go graph); contract files touched only by `T11`.

---

## 3. Concrete TODOs (beyond the DAG)

- **TCC onboarding:** a `docs/PREREQUISITES.md` darwin section — the user must **manually** grant Accessibility + Screen Recording to the signed helper (admin auth, **cannot be scripted**; `tccutil` only *resets*). Document the restart-after-Screen-Recording requirement and the deep-link URLs (`x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility` / `_ScreenCapture`).
- **Code signing + notarization:** a **stable Developer-ID Application identity** for the helper (TCC keys on cdhash/bundle-id — a rebuilt unsigned helper silently loses its grant → injection no-ops). Pipeline: `codesign --options runtime --entitlements …` → `xcrun notarytool submit --wait` → `xcrun stapler staple`; keep the designated requirement constant across releases.
- **Helper protocol spec:** pin a **versioned JSON line protocol** (`dump-tree`/`press`/`set-value`/`click`/`type`/`key`/`scroll`/`capture`/`probe`) + a guest-identity handshake; treat it exactly like the sandboxed-CLI pattern (same `os/exec` + event-log discipline).
- **VM provisioning:** build the golden guest (Apple Silicon, macOS 12+), bake grants + helper + seed apps once; script clone/destroy; guest network-deny + egress allowlist; document the **2-guests-per-Mac** cap, no-nesting, no-iCloud-in-guest.
- **Coordinate testing:** hermetic tests for the points↔pixels↔resized map at 1×/2×/multi-monitor (incl. negative-origin secondary displays); log both spaces.
- **Capability probe:** at session start, measure tree richness (node count, fraction with `AXRole`) to pre-classify AppKit-rich vs Electron-empty vs game-zero; auto-degrade on stagnation.
- **Kill-switch + exclusions (host mode):** global Esc takeover, exclude the controlling terminal from screenshots, per-bundle-ID allowlist, **tag every synthetic CGEvent** via the source-userData field so human-vs-agent input is distinguishable.
- **CI:** the live AX/CGEvent path needs a **dedicated macOS runner with granted TCC + a provisioned guest** — even harder to automate than the Linux desktop e2e; keep the pure pieces hermetic, gate the live slice on the special runner.

---

## 4. Best practices (sourced)

- **Quarantine all native code in the separately-compiled signed helper; the Go core *and* driver stay `CGO_ENABLED=0`** — identical to the `scrot`/`xdotool`/`nilcore-a11y-dump` / hand-rolled-CDP discipline. If tempted by `purego`/`darwinkit`/`robotgo`, stop (breaks I6).
- **Reuse `desktopwire`/`desktopsession`/`desktopagent`/`som`/`desktop` UNCHANGED** — darwin is a new driver + helper + transport, nothing in those packages should be edited.
- **Prefer AXUIElement actions (`AXPress`/`AXSetValue`) over synthetic coordinate clicks** whenever an AX element exists — deterministic, survives layout change, no cursor move/focus steal; keep CGEvent coordinates strictly the Rung-3 fallback.
- **Centralize points↔pixels in ONE display-keyed function**, cache backingScaleFactor + origin, re-read on display-config change, log both spaces — the #1 mis-click bug, worse than Linux (Retina + multi-monitor + resize compound).
- **Let the long-lived signed helper own the TCC grants, never the ephemeral Go binary** — pin a stable Developer ID so a worktree rebuild doesn't re-prompt every time.
- **Use `screencapture` for capture in the MVP** (Apple-maintained; sidesteps the macOS-15 `CGWindowList` obsoletion; no Screen-Recording prompt); graduate to a long-lived ScreenCaptureKit helper only when the frame loop needs it.
- **Default to the disposable VM (I4-compliant); make host control a separate, louder, gated, non-default tier** with forced approval — never let a VM-shaped run silently become host control.
- **Probe-and-degrade per app**; gate every ladder/helper/CV improvement on the reused `eval/browse` pass@1 + pass^k flywheel (principle #9).
- **Reliable injection needs verification:** CGEventPost is fire-and-forget/async and can coalesce/drop; secure-input fields swallow synthetic keys — use tunable inter-event delays and a "did it take effect" check.

---

## 5. Risks / blockers (honest)

- **I4 is genuinely BROKEN in host-control mode** — CGEvent on the user's real desktop is unsandboxed actuation with the user's full ambient authority. The *only* mitigation is making the VM the default and host mode an explicit, hard-gated, recorded decision; the residual risk is real and is the core reason this is a higher-risk tier.
- **TCC is brittle + human-in-the-loop by design** — first-run requires manual System-Settings toggles that can't be scripted; grants drop when the signing identity changes; freshly-granted Screen Recording needs a restart; the per-process `AXIsProcessTrusted` cache and responsible-process inheritance trap make "posted-but-nothing-happened" no-ops a frequent silent-failure mode.
- **VM isolation is heavyweight + CAPPED** — Apple-Silicon-only, ~80 GB golden image + ~30 GB/clone, 30 s+ cold boot, **max 2 concurrent guests/Mac, no nesting** → cannot fan out like Linux containers, cannot run on a cloud Linux box; needs bare-metal Apple hardware (a real scaling/cost blocker).
- **AX coverage is a structural hole on exactly the surfaces that justify desktop CU** — Electron/games/canvas expose empty/zero trees → traffic lands on the weaker Rung 2/3 where classical-CV over/under-segments and misses flat icons (SoM fixes *which* box, not *what* it is; OCR refused to hold I6).
- **No pure-Go path** for input/capture/AX → a native artifact is unavoidable; the zero-module posture survives *only* because the native code is quarantined in a signed helper. Any import of `purego`/`darwinkit`/`robotgo`/`Code-Hex/vz` breaks I6 and must be refused.
- **The live slice is even harder to CI than Linux** — it needs a Mac runner *with granted TCC and a provisioned guest*; the pure pieces test hermetically but the end-to-end is manual/special-runner only.

---

## 6. Bottom line

Native-macOS control is **buildable on the existing spine** — a new `nilcore-desktop-darwin` driver behind the unchanged contract, all native quarantined in a signed `nilcore-mac-helper` so the Go core stays CGO-free (I6), reusing the SoM ladder + session + tool + verifier wholesale. In a **disposable guest VM it is I4-compliant** and the clean recommended path. In **host-control mode it deliberately relaxes I3/I4** — which is why it is the most-gated tier in the whole roadmap, requires a separate louder opt-in + forced approval + per-app allowlist, and is bounded to ~2 guests per Apple-Silicon Mac. Build the contained-Linux desktop first; reach for this only when a task genuinely needs real macOS apps **and** a human has recorded both the §0 thesis decision and the VM-vs-host choice.

> **Sources** (selected): Apple — Quartz Event Services / CGEvent, ScreenCaptureKit, High-Resolution (points vs pixels), AXUIElement, Hardened Runtime / notarization; `BlueM/cliclick`, `go-vgo/robotgo` (no CGO-free macOS backend), `progrium/darwinkit`, `MacPaw/macapptree`, `trycua/cua`, Multi's macOS remote-control engine, the Electron `AXManualAccessibility` issues, and the cliclick macOS-15 `CGWindowListCreateImage` obsoletion. Full citation set in the research brief.
