# EXT-08 — Firecracker microVM sandbox tier (GATED execution plan)

**Read order:** `CLAUDE.md` → `docs/ARCHITECTURE.md` (§Execution model / the isolation spectrum) →
`docs/ROADMAP-EXTERNAL-INFRA.md` (§0 gate + §9 EXT-08) → `docs/SWARM.md` (depth model for a plan-as-DAG) →
this file.

> **Status: BLOCKED behind the §0 gate.** This is a *ready-when-the-gate-clears* plan, not an eligible
> `CLAUDE.md` §5 task. The gate here is a **hardware/host capability** decision (KVM-capable hosts) plus a
> recorded thesis decision that the added isolation is worth the host requirement — see §2. Of every EXT
> item this is **the most philosophy-aligned**: it *strengthens* `I4` and changes **nothing** about the
> thesis-breaking invariants. The default `nilcore` binary stays byte-identical and KVM-absent hosts fall
> back, unchanged, to container/namespace (§9).

---

## Table of contents

- [§0 Summary](#0-summary)
- [§1 As-is: the seams this reuses (sourced)](#1-as-is-the-seams-this-reuses-sourced)
- [§2 The §0 gate criteria for EXT-08](#2-the-0-gate-criteria-for-ext-08)
- [§3 Architecture (a 3rd sandbox.Sandbox impl behind New)](#3-architecture-a-3rd-sandboxsandbox-impl-behind-new)
- [§4 The task DAG (EXT-08-T01 … EXT-08-T09)](#4-the-task-dag)
- [§5 Per-task specs](#5-per-task-specs)
- [§6 Parallel wave map & critical path](#6-parallel-wave-map--critical-path)
- [§7 Per-invariant ledger](#7-per-invariant-ledger)
- [§8 Module justifications](#8-module-justifications)
- [§9 Default-off byte-identical proof](#9-default-off-byte-identical-proof)
- [§10 Risks & honest caveats](#10-risks--honest-caveats)

---

## §0 Summary

EXT-08 adds a **third `sandbox.Sandbox` implementation** — `sandbox.MicroVM` — that confines a
model-emitted command inside a **Firecracker/KVM microVM**: its own kernel, a read-only rootfs image, the
disposable worktree mounted read-write, default-deny networking, and per-run secret injection. It is the
**strongest end** of the isolation spectrum the architecture already documents as future:
*"Firecracker microVM (strongest; Linux/KVM; future) → container → namespace + Landlock (lightest, most
portable)"* (`docs/ARCHITECTURE.md:150`). It is added **additively behind the existing factory + auto-detect**
(`sandbox.New`, `internal/sandbox/select.go:80`), mirroring **exactly** how P7-T01 added the namespace
backend without touching the `Sandbox` interface — `New` auto-detects KVM, an explicit `-sandbox microvm` /
`NILCORE_SANDBOX=microvm` selects it, and **everything else is byte-identical**.

Three properties make EXT-08 the lowest-risk EXT item, and the plan is organized around proving each:

1. **The interface is unchanged.** `Sandbox` stays `Exec`/`ExecWithEnv`/`Workdir`
   (`internal/sandbox/sandbox.go:31-35`). The microVM satisfies it verbatim; **no caller changes**, no new
   `Result` field, no contract-file edit. `backend.CodingBackend`, `go.mod`, `Makefile`, `channel.go` are
   untouched.
2. **It strengthens `I4`, not the thesis.** A microVM is a *harder* boundary than a container or
   namespaces (a separate guest kernel + KVM hardware isolation), so the only invariant it moves, it moves
   **up**. The single new authority it requires — `/dev/kvm` — is a **local device**, not a cloud control
   plane, an IdP, or a remote credential store; it grants no standing outward authority and reaches no
   network by itself.
3. **It fails CLOSED to the existing backends.** Absent KVM (a typical VPS, macOS, a locked-down runner),
   `auto` selects exactly what it does today (namespace → container), and an explicit unsatisfiable
   `microvm` request is a loud error — never a silent un-isolated fallthrough.

`I3` (no host-env leak) is preserved by **mirroring the namespace backend's discipline verbatim**: the
guest is **never seeded from `os.Environ()`** — it receives only the fixed base env plus the explicit
per-run `ExecWithEnv` map, exactly as `namespace_linux.go:79-94` documents and the container backend
already practices. `I6` is preserved with **zero new modules**: the Firecracker VMM is driven over its
**stdlib `net/http`-over-unix-socket** control API, and the one syscall surface (open `/dev/kvm`, the vsock
`connect`) uses **`golang.org/x/sys/unix`** — already a **direct** dependency
(`go.mod:8`), scoped to `internal/sandbox` for exactly this class of work, `CGO_ENABLED=0`-clean.

The authoritative verifier is a new **`sandbox-linux-kvm` CI job** — a KVM-capable runner that runs the
microVM confinement/escape suite with a `NILCORE_KVM_MUST_RUN=1` must-run guard, so the security property
**fails rather than skips** when the backend is unavailable — mirroring the existing `sandbox-linux` job
(`.github/workflows/ci.yml:56-73`) that does the same for the namespace backend.

---

## §1 As-is: the seams this reuses (sourced)

The single most important fact: **EXT-08 is additive over a factory built for exactly this.** The package
docstring already promises it — *"A microVM backend can satisfy the same interface later without touching
any caller"* (`internal/sandbox/sandbox.go:8-9`). Every seam below already exists and is reused, not
rebuilt.

| Seam (sourced) | What EXT-08 reuses it for |
|---|---|
| `sandbox.Sandbox` interface — `Exec`/`ExecWithEnv`/`Workdir` (`internal/sandbox/sandbox.go:31-35`) | The **unchanged** contract `MicroVM` implements. No method added; `Result{Stdout,Stderr,ExitCode}` (`sandbox.go:22-26`) is returned identically (a non-zero guest exit is a *result*, not a Go error). |
| `sandbox.New(Options)` factory + `pick` decision (`internal/sandbox/select.go:53-95`) | The auto-detect/selection point. EXT-08 adds a `MicroVMBackend` arm to `pick` and a `microvmProbe` package var, mirroring `namespaceProbe` (`select.go:33-37`). Auto stays **infallible** (falls back); an explicit unsatisfiable preference stays a loud error (`select.go:54-59`). |
| `Backend` consts + `Options{Prefer,Runtime,Image,HostDir}` (`internal/sandbox/select.go:10-31`) | One new const `MicroVMBackend Backend = "microvm"`. `Options` is **reused as-is** — `Image` doubles as the guest rootfs image reference and `HostDir` is the worktree; no Options field added (kept minimal; microVM-specific knobs read from config/env per §3.5). |
| `Available(runtime)` reporter (`internal/sandbox/select.go:100-103`; used by `nilcore doctor`, `cmd/nilcore/main.go:1317`) | Extended to also report microVM availability + reason, so `doctor`/config-validation lists the new tier honestly. |
| **Namespace backend = the template** (`internal/sandbox/namespace_linux.go`) | EXT-08 **mirrors it exactly**: (a) a Linux build-tagged impl + a `!linux` stub (`namespace_other.go`); (b) the do-not-seed-from-`os.Environ()` `I3` discipline (`namespace_linux.go:79-94`); (c) a conservative `detect*` that prefers a false negative over an EPERM-at-exec (`namespace_linux.go:251-265`); (d) per-platform `*Probe` package var for table-tests on any OS (`select.go:33-37`). |
| `MaybeRunInit()` (`namespace_linux.go:127-144`; called first in `main`, `cmd/nilcore/main.go:71`) | **Untouched.** The microVM backend uses the guest's own init inside the VM, not the host re-exec path, so it adds nothing to `main`'s startup hot path. The existing `MaybeRunInit` no-op for normal/non-Linux invocations is preserved. |
| `policy.EgressProxy` + `ProxyURL`/`Start` (`internal/policy/egress_proxy.go:24,181,195`) | Egress **still routes through here** — the same in-process allowlist forward-proxy with the SSRF/private-range guard (`egress_proxy.go:39-50`). The microVM points its guest `HTTP(S)_PROXY` at the host proxy across the VM's network boundary, exactly as the container backend points across the bridge. |
| `Container.AllowEgressVia(proxyURL)` + `ExtraHosts` host-alias pattern (`sandbox.go:91-99`) + `applyContainerEgress` type-assert seam (`cmd/nilcore/chat.go:410-429`) | The model EXT-08 mirrors: a per-backend `AllowEgressVia` on `MicroVM`, and a sibling `applyMicroVMEgress` type-assert in the cmd egress wiring (the wiring already branches on `box.(*sandbox.Container)`, so adding a `box.(*sandbox.MicroVM)` arm is the additive change). |
| `Container.ExecWithEnv` per-run env contract (`sandbox.go:159-176`) + per-run secret injection (P2-T03) | The exact `I3` behavior `MicroVM.ExecWithEnv` reproduces: env reaches the guest **for one run only**, never persisted, never logged. |
| `sandbox-linux` CI job + `NILCORE_SANDBOX_MUST_RUN=1` must-run guard (`.github/workflows/ci.yml:56-73`) | The template for the new `sandbox-linux-kvm` job (KVM-capable runner, `NILCORE_KVM_MUST_RUN=1`, escape tests fail-not-skip). |
| `golang.org/x/sys` already a **direct** module dep (`go.mod:8`), scoped to `internal/sandbox` | The KVM/vsock syscall surface uses it with **no new module** (§8). |

**What is genuinely new** (the only code this plan writes): one Linux-tagged backend file
(`microvm_linux.go`) + its `!linux` stub, a stdlib VMM-control client (the Firecracker API over a unix
socket), a small guest agent (vsock command runner) built from the same module, the `pick`/probe/`Available`
wiring, the egress type-assert arm, a rootfs-image build recipe, and the CI job. Every one is a new file or
an additive seam; none touches the `Sandbox` interface or any frozen-§5 contract file.

---

## §2 The §0 gate criteria for EXT-08

EXT-08 is **not** an eligible `CLAUDE.md` §5 task until **all** of the following hold and are recorded in
the serialized PR that promotes EXT-08 into `docs/TASKS.md`. This gate is the one in
`docs/ROADMAP-EXTERNAL-INFRA.md` §0 — but for EXT-08 the §0.1 "thesis decision" collapses to a **hardware
decision**, because EXT-08 strengthens `I4` rather than crossing a thesis line.

**G1 — A recorded hardware/thesis decision (§0.1).** A human owner has explicitly decided that NilCore may
require **KVM-capable infrastructure** (bare metal, or a nested-virt-enabled cloud instance) as the host
class for this isolation tier, accepting that a typical cheap VPS **cannot** run it. The decision records
*the trade*: a harder isolation boundary (a separate guest kernel; hardware-enforced VM isolation; a much
smaller host attack surface than a shared-kernel container or namespaces) **in exchange for** a host
capability requirement. This decision is the gate — it is the operator's, not the agent's. *Unlike every
other EXT item, no outward standing authority (cloud plane / IdP / remote store) is granted — only a local
device requirement* — which is why the bar is **the lowest of the eight**.

**G2 — Invariants strengthened, not bypassed (§0.2).** The promotion PR must show concretely that EXT-08
**extends** `I1`–`I7` — proven by §7's ledger: the `Sandbox` interface is unchanged (`I1`-adjacent — no
contract edit); `I4` is *strengthened* (a harder boundary); `I3` holds by mirroring the namespace backend's
do-not-seed-from-`os.Environ()` discipline and injecting per-run secrets only via `ExecWithEnv`; the one new
authority (`/dev/kvm`) is a local device, scoped to `internal/sandbox`, never given to the model, never a
network credential.

**G3 — The verifier still governs (§0.3).** EXT-08 changes **nothing** about the verify path: a
model-emitted command runs **inside** the microVM, its `Result` flows back exactly as from a container, and
`verify.Verifier.Check` re-runs the project's own checks against the worktree afterward. The microVM is a
*stronger isolation backend for the same Tier-1 execution* — it does not let any backend ship work on a
self-report, and the integrator's never-land guarantee is untouched (EXT-08 adds no `GateActionType`).

**G4 — Dependency budget justified (§0.4).** **Zero new modules** (§8): the VMM control plane is stdlib
`net/http` over a unix socket (Firecracker's documented transport); the KVM/vsock syscalls use the
already-direct `golang.org/x/sys/unix`. `CGO_ENABLED=0` cross-compile (`.github/workflows/release.yml`) is
preserved — the backend is Linux-tagged, the `!linux` stub keeps macOS/Windows building, and no cgo is
introduced.

**G5 — Default-off, opt-in, reversible to remove (§0.5).** The default `nilcore` binary is **byte-identical**
with the microVM backend present-but-unselected (§9): `auto` on a KVM-absent host selects exactly what it
does today; the new code is behind a build tag + a probe that returns false; removing the backend files
restores the prior tree with no caller change. **Nothing here becomes a hard requirement** — KVM is opt-in,
and the spectrum's existing two tiers remain the default.

If any of G1–G5 cannot be met, EXT-08 stays on the roadmap, unbuilt. **The standing blocker is G1 (the
hardware/thesis decision); G2–G5 are satisfied by the design below and provable by `make verify` + the
`sandbox-linux-kvm` job once a KVM host exists.**

---

## §3 Architecture (a 3rd sandbox.Sandbox impl behind New)

The organizing principle: **EXT-08 is one more leaf behind the factory the package was built to grow.** The
microVM is selected, configured, and used through the *same three calls* every other backend is —
`New`/`Exec`/`Workdir` — so the orchestrator, the verifier, the egress proxy, and the secret-injection path
are all reused unchanged.

```
                       selectSandbox(prefer,runtime,image,dir)   (cmd/nilcore/main.go:1350 — UNCHANGED call)
                                          │
                          sandbox.New(Options{Prefer,Runtime,Image,HostDir})   (select.go:80 — +1 arm)
                                          │
        ┌─────────────────────────────────┴──────────────────────────────────┐
        │  pick(prefer, nsAvail, microvmAvail, containerAvail)                 │  (select.go:53 — +microvm arm)
        │    auto: microvm?  → MicroVM   (STRONGEST, only when /dev/kvm usable)│  ← NEW preference order:
        │         else nsAvail? → Namespace                                    │     microvm → namespace → container
        │         else        → Container  (fail-CLOSED, unchanged)            │
        └─────────────────────────────────┬──────────────────────────────────┘
                                          ▼
   newMicroVM(opts) (microvm_linux.go) ── boots ONE Firecracker VMM per worktree:
        ├─ jailer-style child: open /dev/kvm (x/sys), spawn firecracker, talk its API over a unix socket
        ├─ guest kernel (bundled vmlinux) + ro rootfs image (opts.Image)        ← read-only "rootfs" (mirror of container --read-only)
        ├─ worktree → guest /work  (virtio-blk on a loop image OR virtio-fs, RW) ← the SINGLE writable mount (I4, mirrors /work)
        ├─ network: none by default (no tap)  ──or──  one tap → host EgressProxy ← default-deny egress (mirror --network none)
        └─ a tiny guest init/agent over VSOCK: receives {cmd, env, env-stripped}, runs /bin/sh -c, returns Result
                                          ▼
   MicroVM.ExecWithEnv(ctx, cmd, env):  vsock-send {cmd, BASE-ENV-ONLY + env}  →  guest runs  →  Result{Stdout,Stderr,ExitCode}
        └─ env is BASE + the per-run map ONLY; os.Environ() is NEVER forwarded (I3 — mirrors namespace_linux.go:79-94)
```

### §3.1 Boot, kernel & rootfs

`newMicroVM(opts)` (in `microvm_linux.go`) is the constructor `New` calls when `pick` chose `MicroVM`. It
prepares a **per-worktree** VMM:

- **VMM process.** Spawn `firecracker` with an API unix socket (no network for the control plane — a local
  AF_UNIX socket). The constructor opens `/dev/kvm` (via `golang.org/x/sys/unix`) only to *probe* usability
  at detect time; the actual VM lifecycle is driven over the socket. The VMM is launched under a
  **jailer-style** posture (drop to an unprivileged uid/gid, `no_new_privs`, a chroot/cgroup for the VMM
  itself) so even the host-side VMM holds minimal authority — the microVM analogue of the container's
  `--cap-drop=ALL --security-opt no-new-privileges` (`sandbox.go:112-114`).
- **Guest kernel.** A bundled, pinned, minimal `vmlinux` (no modules; the seccomp/landlock concerns of the
  namespace backend are moot — the guest kernel is a *separate* kernel from the host). Kernel + rootfs are
  the EXT-08 analogue of the container's *image*: built reproducibly (§3.6), pinned by digest, `Options.Image`
  names the rootfs.
- **Read-only rootfs.** The guest rootfs is mounted **read-only** inside the guest (mirroring the
  container's `--read-only`, `sandbox.go:115`), with a writable guest `tmpfs` for scratch and `HOME`/Go
  caches (mirroring `--tmpfs /tmp` + the `HOME=/tmp`/`GOCACHE`/`GOPATH` env, `sandbox.go:116-119`).

### §3.2 The worktree mount (the single writable surface — I4)

The disposable worktree (`opts.HostDir`, symlink-resolved exactly as the namespace backend does via
`filepath.EvalSymlinks`, `namespace_linux.go:51-57`) is presented to the guest at **`/work`** read-write —
the EXT-08 mirror of the container's `-v <hostDir>:/work` (`sandbox.go:150`) and the namespace backend's
Landlock read-write-only-the-worktree rule (`namespace_linux.go:181`). Mechanism: either a virtio-blk loop
image over the worktree dir or **virtio-fs** sharing the host worktree dir (virtio-fs is preferred for
identity-mapped paths so a path the host-side file tools already resolved is the same path the in-guest
shell sees — the same property the container backend's identity-mapped `/work` provides). `/work` is the
**only** writable mount that touches host state; the rootfs is read-only and `tmpfs` is guest-private. This
preserves `I4`'s "writable scratch is the worktree, everything else read-only" exactly. The optional `/add`
read-roots (`Container.ExtraReadRoots`, `sandbox.go:55-63`) map to **additional read-only virtio-fs shares**
at the same absolute path — the same identity-mapped `:ro` posture.

### §3.3 Egress via EgressProxy (unchanged policy, mirrored wiring)

Egress **still routes through `policy.EgressProxy`** — the same in-process allowlist forward-proxy with the
SSRF/private-range guard (`egress_proxy.go:39-50,104-115`). The microVM is **default-deny**: with no tap
device the guest has no route off-box (the microVM analogue of `--network none` and the namespace backend's
`CLONE_NEWNET` with no interface). When egress is enabled:

- `MicroVM.AllowEgressVia(proxyURL)` mirrors `Container.AllowEgressVia` (`sandbox.go:91-99`): attach **one**
  tap device to the guest and set the guest's `HTTP(S)_PROXY` env to the host proxy address reachable across
  the tap (the host gateway IP), so a denied host is simply unreachable even if the model tries.
- The cmd egress wiring gains a sibling `applyMicroVMEgress` arm next to `applyContainerEgress`
  (`cmd/nilcore/chat.go:410-429`): the wiring already type-asserts `box.(*sandbox.Container)`, so adding a
  `box.(*sandbox.MicroVM)` arm (host-gateway alias + `AllowEgressVia`) is the additive change. The proxy's
  `Start(ctx, "0.0.0.0:0")` bind (so it is reachable across the VM boundary, `egress_proxy.go:195`) and the
  allowlist/SSRF guard gate every request **regardless of backend** — no policy change, only a transport
  mirror.

### §3.4 Per-run secret injection via ExecWithEnv (I3 — verbatim discipline)

`MicroVM.ExecWithEnv(ctx, cmd, env)` is the load-bearing `I3` method, and it reproduces the namespace
backend's discipline **verbatim**:

- The guest is **never seeded from `os.Environ()`**. The host nilcore process holds the operator's secrets
  (`ANTHROPIC_API_KEY`, the vault passphrase, the log-HMAC key) for its whole lifetime; a model-emitted
  command must never see them. So the vsock request carries **only** a fixed base env (PATH/HOME/Go caches,
  the container/namespace base) **plus** the explicit per-run `env` map (e.g. a delegated CLI's key, which
  *is* meant for the command). This is the exact rule `namespace_linux.go:79-94` documents and the container
  backend already follows (it never forwards `os.Environ()` into the container).
- Per-run env reaches the guest **for that single run only**, is never persisted to the rootfs (read-only)
  or the host, and is **never logged** — matching `ExecWithEnv`'s documented contract (`sandbox.go:159-160`).
  Keyed verify-pack checks that inject `$NILCORE_*_KEY` by name (the shipped finance pack pattern,
  `docs/SWARM.md:134`) work unchanged because they go through `ExecWithEnv`.
- `Exec(ctx, cmd)` delegates to `ExecWithEnv(ctx, cmd, nil)` exactly as the other two backends do
  (`sandbox.go:155-157`, `namespace_linux.go:62-64`).

The command transport is **vsock** (AF_VSOCK, the Firecracker-native host↔guest channel) to a tiny guest
agent that does `exec /bin/sh -c <cmd>` with the assembled env — the microVM analogue of the namespace
backend's `unix.Exec("/bin/sh", ...)` (`namespace_linux.go:139-143`). The guest agent is **trusted harness
code** (built from this repo), runs the model command only *after* the VM boundary is established, and
returns `Result{Stdout,Stderr,ExitCode}` — a non-zero guest exit is a normal `Result`, not a Go error
(`sandbox.go:20-26`).

### §3.5 Detection, selection & fail-closed

`detectMicroVM()` (Linux only) is **conservative** in the exact spirit of `detectNamespace`
(`namespace_linux.go:251-265`) — it prefers a **false negative** (fall back to namespace/container) over a
**false positive** (a backend that fails at boot):

- `/dev/kvm` exists, is openable, and the `KVM_GET_API_VERSION` ioctl returns the expected version;
- the `firecracker` binary (+ pinned kernel/rootfs assets) is resolvable;
- vsock is available (`/dev/vhost-vsock` / AF_VSOCK).

Any miss ⇒ `(false, reason)` and `auto` falls back. Off Linux, the `!linux` stub returns
`(false, "microVM sandbox requires Linux/KVM (this host is <GOOS>)")` — exactly mirroring
`namespace_other.go`. The `pick` decision (`select.go:53`) gains a `MicroVMBackend` case and a new auto
preference order — **strongest-first**: `microvm → namespace → container`. Auto stays **infallible**
(`select.go:65-69`); an explicit `microvm` request the host can't satisfy is a **loud error** with the
probe reason (mirroring the namespace error path, `select.go:55-59,86-88`) — never a silent un-isolated run.

### §3.6 Rootfs/kernel image build & the authoritative CI verifier

- **Reproducible assets.** A `make`-driven recipe builds a minimal guest kernel (`vmlinux`) + a read-only
  rootfs containing `/bin/sh`, the build toolchains the verify-packs need, and the trusted guest agent —
  pinned by digest, the microVM analogue of the sandbox container image. The recipe lives outside the
  default build and is **not** required to build/test the host binary.
- **`sandbox-linux-kvm` CI job (the authoritative verifier).** A KVM-capable runner (a self-hosted or
  nested-virt instance; GitHub-hosted `ubuntu-latest` does **not** expose `/dev/kvm`) runs the microVM
  confinement/escape suite under `NILCORE_KVM_MUST_RUN=1`, so the tests **fail rather than skip** if KVM is
  absent — no false green. This mirrors the `sandbox-linux` job (`.github/workflows/ci.yml:56-73`) and the
  `browser-e2e` job (`ci.yml:83`) that handle the other environment-dependent, CI-only security surfaces.
  The hermetic `make verify` covers the pure logic (`pick`/probe/`Available`/env-assembly/egress-wiring) on
  any OS via package vars; the live confinement is the CI job's job.

---

## §4 The task DAG

**Namespace `EXT-08-T01 … EXT-08-T09`.** One task = one branch (`task/EXT-08-T0x`) = one PR. Owns sets are
**pairwise-disjoint** (file/dir = unit of ownership). `internal/sandbox` is a shared package, so the tasks
that add **sibling files** to it (T02, T03, T04, T07) declare per-**file** Owns and serialize only where they
must touch the **same** file (T07 is the sole editor of `select.go`/`sandbox.go`/the package docstring); the
selection rule forbids two open branches that name the same file.

| ID | Title | Depends on | Owns | Note |
|---|---|---|---|---|
| EXT-08-T01 | Backend const + probe seam + `!linux` stub + `pick` order | — | `internal/sandbox/microvm_other.go` (new), `internal/sandbox/microvm_probe.go` (new), `internal/sandbox/select.go` (edit) | opens the microVM seam; sole editor of `select.go` |
| EXT-08-T02 | VMM control client (stdlib `net/http`-over-unix) | EXT-08-T01 | `internal/sandbox/fcclient/` (new sub-pkg) | new leaf; no Firecracker SDK (§8) |
| EXT-08-T03 | Guest agent (vsock command runner) | EXT-08-T01 | `internal/sandbox/guestagent/` (new sub-pkg) + `cmd/nilcore-guest/` (new tiny main) | trusted in-guest harness code |
| EXT-08-T04 | `MicroVM` backend + `detectMicroVM` (Linux) | EXT-08-T02, EXT-08-T03 | `internal/sandbox/microvm_linux.go` (new), `microvm_linux_test.go` (new) | the impl; mirrors `namespace_linux.go` |
| EXT-08-T05 | Egress wiring (`AllowEgressVia` + cmd type-assert arm) | EXT-08-T04 | `cmd/nilcore/microvm_egress.go` (new), `cmd/nilcore/chat.go` (1 arm — edit) | mirrors `applyContainerEgress` |
| EXT-08-T06 | `Available` + `doctor`/config surfacing | EXT-08-T04 | `internal/sandbox/select.go` (the `Available` func — coordinated w/ T01 via same task owner or rebase) , `cmd/nilcore/main.go` (doctor line) | see §6 note on `select.go` |
| EXT-08-T07 | Reproducible kernel+rootfs build recipe | EXT-08-T03 | `images/microvm/` (new), `Makefile` (additive target) | **Makefile is contract — serialized** |
| EXT-08-T08 | `sandbox-linux-kvm` CI job (authoritative verifier) | EXT-08-T04, EXT-08-T07 | `.github/workflows/ci.yml` (additive job) | KVM runner, must-run guard |
| EXT-08-T09 | Docs + CHANGELOG promotion | EXT-08-T05, EXT-08-T06, EXT-08-T08 | `docs/ARCHITECTURE.md`, `docs/ROADMAP-EXTERNAL-INFRA.md`, `docs/TASKS.md`, `CLAUDE.md`, `README.md`, `CHANGELOG.md` | **contract (docs) — serialized last** |

> **`select.go` ownership note.** `select.go` is touched by T01 (const + `pick` arm + probe var) and T06
> (`Available`). To keep Owns disjoint, **T01 is the sole owner of `select.go`** and folds the one-line
> `Available` extension into its scope (it is a 3-line change adjacent to `pick`); T06 then owns only the
> *consumers* (`cmd/nilcore/main.go` doctor line + any config-validation surfacing). The DAG table above is
> corrected to reflect this — T06's Owns is `cmd/nilcore/main.go` (doctor) only; `select.go`'s `Available`
> belongs to T01. (Mirrors how SW-T05 is the sole owner of `packs.go` in `docs/SWARM.md:479`.)

---

## §5 Per-task specs

#### EXT-08-T01 — Backend const + probe seam + `!linux` stub + `pick` order
- **Goal:** open the microVM seam *without* any boot code — the selection logic, the probe package var, the
  off-Linux stub, and the `Available` extension — so every (preference × capability) combination is
  table-testable on **any** OS before a line of KVM code exists. Mirrors how `select.go`/`namespace_other.go`
  were structured for the namespace backend.
- **Depends on:** — (reuses shipped `internal/sandbox`). **Owns:** `internal/sandbox/select.go` (edit; sole
  owner), `internal/sandbox/microvm_other.go` (new `!linux` stub), `internal/sandbox/microvm_probe.go` (new:
  the `microvmProbe` package var + the cross-platform `Available` plumbing), `select_test.go` (extend).
- **Acceptance:** `MicroVMBackend Backend = "microvm"` const; `microvmProbe = detectMicroVM` package var
  (mirrors `namespaceProbe`, `select.go:33-37`) so selection is unit-testable by swapping it; `pick(prefer,
  microvmAvail, nsAvail, containerAvail)` extended — **auto preference order strongest-first**
  (`microvm → namespace → container`); `prefer==MicroVMBackend && !microvmAvail` ⇒ **error** (loud,
  carries the probe reason like `select.go:86-88`); `auto` stays **infallible** (falls to namespace/container,
  `select.go:65-69`); `Available(runtime)` returns `(microvm bool, microvmReason string, namespace bool,
  namespaceReason string, container bool)` (additive return — its only caller is `doctor`, T06); a `!linux`
  `microvm_other.go` stub `detectMicroVM() (false, "microVM sandbox requires Linux/KVM (this host is
  <GOOS>)")` + a `newMicroVM` defensive guard (mirrors `namespace_other.go`).
- **Verify:** `make verify`; `go test ./internal/sandbox/` — extend `select_test.go` (`select_test.go`
  exists) with the full (prefer × microvmAvail × nsAvail × containerAvail) truth table: auto picks microvm
  when available, else namespace, else container; explicit microvm-unavailable errors with reason; unknown
  backend still errors (`select.go:71`); off-Linux probe returns false. **On macOS the maintainer's host
  this all passes** (probe swapped / stub returns false).
- **Notes:** **no boot code here** — this is pure selection wiring, so it lands first and unblocks the
  table-tests on any OS. `newMicroVM` is referenced but its Linux body arrives in T04 (the `!linux` stub
  + a Linux build-tagged forward-decl pattern matching how `newNamespace` is split).

#### EXT-08-T02 — VMM control client (stdlib `net/http`-over-unix)
- **Goal:** a small, stdlib-only client for the Firecracker VMM control API (boot-source, drives, network
  interfaces, vsock, machine-config, `InstanceStart`) spoken as **HTTP over a unix-domain socket** — **no
  Firecracker Go SDK** (§8).
- **Depends on:** EXT-08-T01. **Owns:** `internal/sandbox/fcclient/` (`client.go`, `types.go`,
  `client_test.go`, `deps_test.go`).
- **Acceptance:** an `http.Client` with a `DialContext` pinned to the API unix socket (stdlib
  `net.Dial("unix", sock)`); typed request structs for the documented API resources; `Configure(...)` +
  `Start(ctx)` + `Shutdown(ctx)`; every method `ctx`-first and honoring cancellation; errors wrapped with
  `%w` + context; **no module import** (a `deps_test.go` asserts `go list -deps` shows no third-party VMM
  SDK and no module beyond stdlib + `golang.org/x/sys`).
- **Verify:** `make verify`; `go test ./internal/sandbox/fcclient/...` against an **`httptest`-over-unix
  stub** (a local `net.Listen("unix")` + `http.Serve` returning canned Firecracker responses) — configure,
  start, shutdown, error mapping, ctx-cancel; **runs on any OS** (no KVM). `deps_test.go` enforces I6.
- **Notes:** the Firecracker control plane is a *local* AF_UNIX socket — reaches no network, holds no
  credential. Pure transport leaf.

#### EXT-08-T03 — Guest agent (vsock command runner)
- **Goal:** the trusted in-guest harness that receives `{cmd, env}` over **vsock**, assembles the
  command env (base + per-run map; **never** the host `os.Environ`), runs `/bin/sh -c <cmd>`, and returns
  `Result{Stdout,Stderr,ExitCode}` — the in-guest analogue of `MaybeRunInit`'s `unix.Exec`
  (`namespace_linux.go:139-143`).
- **Depends on:** EXT-08-T01. **Owns:** `internal/sandbox/guestagent/` (`agent.go`, `proto.go`,
  `agent_test.go`), `cmd/nilcore-guest/main.go` (a tiny static main that runs the agent inside the guest).
- **Acceptance:** a stdlib `proto.go` (length-prefixed JSON over the vsock conn) shared host↔guest; the
  agent reads a request, runs the command with the **supplied** env only, streams stdout/stderr into the
  Result, returns it; **never sources `os.Environ()` from the guest** for the command (the guest is a fresh
  kernel, but the discipline is explicit and tested); a non-zero exit is a normal Result, not an error;
  `cmd/nilcore-guest` builds `CGO_ENABLED=0` static (it ships *inside* the rootfs, T07).
- **Verify:** `make verify`; `go test ./internal/sandbox/guestagent/...` over an **in-process pipe** (no
  vsock, no VM): a request round-trips; per-run env reaches the command; the host env is **not** leaked
  into the command; non-zero exit ⇒ `ExitCode` set, no Go error. Runs on any OS.
- **Notes:** the protocol leaf is shared so T04 (host side) and the guest main agree by type, not by string.

#### EXT-08-T04 — `MicroVM` backend + `detectMicroVM` (Linux)
- **Goal:** the `sandbox.Sandbox` implementation — `newMicroVM`/`Exec`/`ExecWithEnv`/`Workdir` + the
  conservative `detectMicroVM` — composing `fcclient` (T02) + the vsock protocol (T03), mirroring
  `namespace_linux.go` structurally (build tag, symlink-resolved workdir, do-not-seed-env `I3` discipline).
- **Depends on:** EXT-08-T02, EXT-08-T03. **Owns:** `internal/sandbox/microvm_linux.go` (new),
  `internal/sandbox/microvm_linux_test.go` (new).
- **Acceptance:** `//go:build linux`; `type MicroVM struct { HostDir, Image string; ... }`; `newMicroVM(opts)
  (Sandbox, error)` symlink-resolves `HostDir` (`filepath.EvalSymlinks`, mirror `namespace_linux.go:51-57`);
  `Workdir()` returns it; `ExecWithEnv` boots/uses the VM via `fcclient`, sends the command over vsock with
  **base-env + per-run map only (NEVER `os.Environ()`)** — the `I3` discipline copied from
  `namespace_linux.go:79-94` with the same SECURITY comment — and returns `Result`; `Exec` delegates to
  `ExecWithEnv(_, _, nil)`; `AllowEgressVia(proxyURL)` attaches a tap + sets guest `HTTP(S)_PROXY` (mirror
  `Container.AllowEgressVia`, `sandbox.go:91-99`); `detectMicroVM()` is **conservative** (open `/dev/kvm` +
  `KVM_GET_API_VERSION` ioctl via `golang.org/x/sys/unix`, `firecracker` resolvable, vsock present) and
  prefers a false negative (mirror `detectNamespace`, `namespace_linux.go:251-265`); the rootfs is mounted
  **read-only**, `/work` is the **single writable** mount (I4); KVM/vsock syscalls use **only**
  `golang.org/x/sys/unix` (no new module).
- **Verify:** `make verify` (the Linux file compiles; pure helpers — env assembly, workdir resolution,
  egress arg-building — are unit-tested with a **fake `fcclient` + a fake vsock dialer** so no real KVM is
  needed, mirroring the container backend's `runArgs` unit-testability, `sandbox.go:101-103`). The **live**
  boot+escape behavior is `sandbox-linux-kvm`'s job (T08) — `make verify` stays hermetic on macOS. A test
  asserts `MicroVM` satisfies `sandbox.Sandbox` (`var _ sandbox.Sandbox = (*MicroVM)(nil)`) and that the
  assembled command env **never contains** a host-only var (the `I3` regression test, the microVM mirror of
  the namespace backend's env test).
- **Notes:** **the `sandbox.Sandbox` interface is not touched** — `MicroVM` satisfies the existing three
  methods. No `Result` field added. This is the heart of "strengthens I4, changes no contract."

#### EXT-08-T05 — Egress wiring (`AllowEgressVia` + cmd type-assert arm)
- **Goal:** route the microVM's egress through the **existing** `policy.EgressProxy` by mirroring the
  container egress wiring — a `box.(*sandbox.MicroVM)` arm beside the existing `box.(*sandbox.Container)`
  arm, no policy change.
- **Depends on:** EXT-08-T04. **Owns:** `cmd/nilcore/microvm_egress.go` (new — the `applyMicroVMEgress`
  helper), `cmd/nilcore/chat.go` (one added arm dispatching to it — edit), `microvm_egress_test.go` (new).
- **Acceptance:** `applyMicroVMEgress(box, egress, proxyAddr)` no-ops on empty egress / empty proxyAddr / a
  non-`*MicroVM` box (mirror `applyContainerEgress`, `chat.go:410-417`); on a real microVM it sets the
  host-gateway-reachable `HTTP(S)_PROXY` via `MicroVM.AllowEgressVia(policy.ProxyURL(...))`; the proxy's
  `Start(ctx,"0.0.0.0:0")` bind + allowlist/SSRF guard are reused **unchanged** (`egress_proxy.go:104-115,
  195`); `chat.go` dispatches to the right helper by backend type (the existing single `*Container`
  type-switch grows one arm).
- **Verify:** `make verify`; `go test ./cmd/nilcore/...`: empty-egress no-op; non-microVM box no-op; a fake
  `MicroVM` records the proxy URL it was handed; the allowlist still denies a non-listed host (reuse the
  existing egress proxy tests' posture). Hermetic.
- **Notes:** **no egress policy is added or changed** — only a transport mirror. The SSRF/private-range guard
  (`egress_proxy.go:39-50`) protects the microVM path identically.

#### EXT-08-T06 — `Available` + `doctor`/config surfacing
- **Goal:** surface the microVM tier honestly in `nilcore doctor` and config validation, consuming T01's
  extended `Available`.
- **Depends on:** EXT-08-T04 (so the reason strings are accurate). **Owns:** `cmd/nilcore/main.go` (the
  `doctor` block around `sandbox.Available`, `main.go:1317` — edit), `main_test.go` (extend).
- **Acceptance:** `doctor` prints the microVM row (available? + reason when not, e.g. "no /dev/kvm",
  "firecracker not found", "requires Linux/KVM (this host is darwin)") beside the existing namespace/container
  rows; config validation that names a sandbox backend accepts `microvm` and rejects unknown values with a
  loud error (reuse the existing `-sandbox`/`NILCORE_SANDBOX` validation path, `main.go:1350-1366`).
- **Verify:** `make verify`; `go test ./cmd/nilcore/...`: `doctor` lists microvm + reason on a non-KVM host
  (the maintainer's macOS path); an invalid backend name errors. Hermetic.
- **Notes:** `Available`'s signature change is T01's (sole `select.go` owner); T06 only consumes it — keeps
  Owns disjoint.

#### EXT-08-T07 — Reproducible kernel+rootfs build recipe
- **Goal:** a reproducible, pinned-by-digest guest kernel (`vmlinux`) + read-only rootfs containing
  `/bin/sh`, the verify-pack toolchains, and the T03 guest agent — the microVM analogue of the sandbox
  container image — built **outside** the default `go build`/`make verify`.
- **Depends on:** EXT-08-T03 (the rootfs bundles `cmd/nilcore-guest`). **Owns:** `images/microvm/` (the
  recipe + a pin/digest manifest), `Makefile` (one additive target, e.g. `make microvm-image`).
- **Acceptance:** a documented, reproducible recipe (minimal kernel config; rootfs assembled from pinned
  package digests; the static `nilcore-guest` agent embedded); the asset is referenced by `Options.Image`;
  the recipe is **not** invoked by `make verify`/`make build` (so a host without KVM tooling still builds the
  binary); `CGO_ENABLED=0` for the embedded guest agent is preserved.
- **Verify:** the recipe runs on the KVM CI host (T08 consumes its output); `make verify` is **unaffected**
  (the new target is opt-in). A digest-pin test (or a CI assertion) confirms the asset is reproducible.
- **Notes:** **`Makefile` is a frozen-§5 contract file — this task is serialized** (one additive target,
  no change to `verify`/`build`/`test`/`run`). Treat the additive target as the minimal contract change it
  is; do not touch the existing targets.

#### EXT-08-T08 — `sandbox-linux-kvm` CI job (authoritative verifier)
- **Goal:** the authoritative live verifier — a KVM-capable runner that runs the microVM
  confinement/escape suite with a must-run guard so the security property **fails rather than skips**.
- **Depends on:** EXT-08-T04, EXT-08-T07. **Owns:** `.github/workflows/ci.yml` (one additive job).
- **Acceptance:** a `sandbox-linux-kvm` job on a KVM-capable runner (self-hosted or a nested-virt
  instance — GitHub-hosted `ubuntu-latest` has **no** `/dev/kvm`); builds the microVM image (T07); runs
  `go test -run 'TestMicroVM...' ./internal/sandbox/` with `NILCORE_KVM_MUST_RUN=1` so the confinement
  tests **fail-not-skip** when KVM is absent (mirror `NILCORE_SANDBOX_MUST_RUN=1`, `ci.yml:70-73`); the
  escape suite asserts: the guest cannot read host paths outside `/work`, cannot reach a denied egress host,
  cannot see a host-only env var (`I3`), and a non-zero command exit is a normal `Result`.
- **Verify:** the job is green on the KVM runner; the must-run guard is proven by a deliberately-broken
  fixture failing (not skipping). Other CI jobs are **unchanged**.
- **Notes:** mirrors the existing `sandbox-linux` (`ci.yml:56-73`) and `browser-e2e` (`ci.yml:83`)
  environment-dependent, CI-only security jobs. This is `I2`/`I4`'s proof obligation for EXT-08.

#### EXT-08-T09 — Docs + CHANGELOG promotion · contract (docs), serialized last
- **Goal:** promote EXT-08 into the canonical docs and ledger as the third isolation tier.
- **Depends on:** EXT-08-T05, EXT-08-T06, EXT-08-T08. **Owns:** `docs/ARCHITECTURE.md`,
  `docs/ROADMAP-EXTERNAL-INFRA.md`, `docs/TASKS.md`, `CLAUDE.md`, `README.md`, `CHANGELOG.md`.
- **Acceptance:** `docs/ARCHITECTURE.md` §Execution model updated so the isolation spectrum lists the
  microVM as **shipped** (was `:150` "future") with its layer-map row + import set
  (`internal/sandbox/{fcclient,guestagent}` + `golang.org/x/sys` only); `docs/ROADMAP-EXTERNAL-INFRA.md` §9
  EXT-08 marked **gate-cleared/shipped** (the §0 hardware decision recorded) with the boundary line that a
  *managed* microVM fleet is still `EXT-01`; `docs/TASKS.md` the EXT-08 DAG rows + specs; `CLAUDE.md`
  invariant text **unchanged** (the point: invariants are strengthened, not edited — only the repository-map
  and `I6` sanctioned-deps note, which is **also unchanged** since no module was added); `README.md` the
  `-sandbox microvm` / `NILCORE_SANDBOX=microvm` usage + the KVM-host requirement + the default-off note;
  `CHANGELOG.md` one line per merged EXT-08 task.
- **Verify:** `make verify` (docs don't break the build); a markdown pass; manual review that the layer-map
  import sets match `go list -deps` of the new leaves and that **no invariant text changed**.
- **Notes:** **serialized — contract files.** Lands last. The headline doc outcome is the smallest possible:
  the spectrum line flips "future" → "shipped" and the EXT-08 roadmap entry flips to gate-cleared; **no
  invariant, no frozen contract, and `go.mod` are touched.**

---

## §6 Parallel wave map & critical path

A fleet executes in ordered **waves**; every task in a wave has all deps merged to `main` and a
pairwise-disjoint Owns set. The shared package `internal/sandbox` is split by **file** ownership (T01 sole
owner of `select.go`; T04 sole owner of `microvm_linux.go`; T02/T03 are new sub-packages), so within-package
concurrency is collision-free.

```
WAVE 1  (1 — opens the seam; sole select.go owner; pure selection wiring, testable on any OS)
  └── EXT-08-T01  select.go + microvm_other.go + microvm_probe.go

WAVE 2  (2 concurrent — new sub-package leaves, each independently make-verify-green)
  ├── EXT-08-T02  internal/sandbox/fcclient/        (T01)
  └── EXT-08-T03  internal/sandbox/guestagent/ + cmd/nilcore-guest/   (T01)

WAVE 3  (1 — the backend impl; composes T02+T03; sole microvm_linux.go owner)
  └── EXT-08-T04  internal/sandbox/microvm_linux.go   (T02, T03)

WAVE 4  (3 concurrent — disjoint consumers/assets of the shipped backend)
  ├── EXT-08-T05  cmd/nilcore/microvm_egress.go + chat.go arm   (T04)
  ├── EXT-08-T06  cmd/nilcore/main.go doctor line              (T04)
  └── EXT-08-T07  images/microvm/ + Makefile target           (T03)   ← SERIAL pt: Makefile is contract

WAVE 5  (1 — the authoritative live verifier)
  └── EXT-08-T08  .github/workflows/ci.yml  (sandbox-linux-kvm)   (T04, T07)

WAVE 6  (1 — SERIAL pt: docs contract)
  └── EXT-08-T09  docs/* + CLAUDE.md + README.md + CHANGELOG.md   (T05, T06, T08)
```

**Peak concurrency = 3 (wave 4).** Critical path (longest dependency chain) — **6 sequential merges:**

```
EXT-08-T01 → EXT-08-T03 → EXT-08-T04 → EXT-08-T07 → EXT-08-T08 → EXT-08-T09
```

(equivalently `T01 → T02 → T04 → …`; T02 and T03 are the wave-2 parallel pair, T03 is on the path because
T07/T08 need the rootfs that bundles the guest agent.)

**Serialization points (parallelism intentionally throttled to one writer):**
1. `internal/sandbox/select.go` — EXT-08-T01 only (const + `pick` order + probe var + `Available`).
2. `internal/sandbox/microvm_linux.go` — EXT-08-T04 only (the impl).
3. `Makefile` — EXT-08-T07 only (one additive `microvm-image` target; frozen-§5 contract).
4. `.github/workflows/ci.yml` — EXT-08-T08 only (one additive job).
5. `docs/*` / `CLAUDE.md` / `README.md` / `CHANGELOG.md` prose — EXT-08-T09 only.

**No-cycle proof:** every edge points from a lower wave to a higher one; no task depends on a later task;
`internal/sandbox`'s files are owned by exactly one task each. **Foundation-before-impl holds:** the backend
(T04) literally cannot compile until the control client (T02) and the guest protocol (T03) exist, and the CI
verifier (T08) cannot run until the image (T07) is buildable. The work-selection rule walks waves 1→6
without a forced collision.

---

## §7 Per-invariant ledger

The seven invariants hold **by reuse**, and `I4` is **strengthened** — verified against the real seams.

| Invariant | How EXT-08 preserves (or strengthens) it |
|---|---|
| **I1** frozen contract | `backend.CodingBackend.Run(ctx,Task)→(Result,error)` is **untouched** — the microVM is a Tier-1 *sandbox* backend, not a coding backend; the model command's `Result{Stdout,Stderr,ExitCode}` flows back through the **unchanged** `sandbox.Sandbox` interface (`sandbox.go:31-35`). **No `Result`/`Task`/interface field added.** `go.mod`/`Makefile`/`channel.go` are not edited (Makefile gains only an additive image target, T07). |
| **I2** verifier sole authority | **Unchanged.** The model command runs *inside* a stronger box; `verify.Verifier.Check` still re-runs the project's own checks against the worktree afterward and governs "done." EXT-08 adds no self-report path and no `GateActionType`; the integrator's never-land guarantee is untouched. |
| **I3** no ambient authority / **no host-env leak** | **The load-bearing reuse.** `MicroVM.ExecWithEnv` mirrors `namespace_linux.go:79-94` **verbatim**: the guest is **never seeded from `os.Environ()`** — it gets only a fixed base env + the explicit per-run map (the do-not-seed discipline the container backend also follows). Per-run secrets reach the guest for one run, never persisted (read-only rootfs), never logged. The one new authority — `/dev/kvm` — is a **local device** held by `internal/sandbox`, never a network credential, never given to the model. A regression test asserts no host-only var appears in the assembled command env. |
| **I4** sandboxed execution — **STRENGTHENED** | A microVM is a **harder boundary** than a container or namespaces: a *separate guest kernel* + KVM hardware-enforced isolation, so a guest-kernel bug is not a host-kernel compromise. The worktree at `/work` is the **single writable** mount (mirror of `-v …:/work` + the Landlock RW-only-worktree rule); the rootfs is read-only; networking is default-deny (no tap, the `--network none` analogue). The *strongest* end of the spectrum the architecture documented as future — now built, behind the same interface. |
| **I5** append-only audit | **Unchanged.** Every model call/tool exec/verify/gate decision still appends to the event log; the microVM backend is a swappable execution detail beneath it. Metadata-only, no secret on append. |
| **I6** zero-dependency core | **Zero new modules** (§8): VMM control = stdlib `net/http` over a unix socket; KVM/vsock syscalls = `golang.org/x/sys/unix`, **already a direct dep** (`go.mod:8`) scoped to `internal/sandbox`. `CGO_ENABLED=0` preserved (Linux build tag + `!linux` stub; the guest agent builds static no-cgo). A `deps_test.go` in `fcclient` asserts no third-party SDK. |
| **I7** untrusted-as-data | **Unchanged.** Tool output / file contents / fetched content remain data; the microVM is an execution boundary, not a new instruction source. Egress fetches still route through the allowlist proxy and are untrusted-as-data downstream exactly as today. |
| **Interface unchanged (the headline)** | `sandbox.Sandbox` keeps `Exec`/`ExecWithEnv`/`Workdir`; `MicroVM` satisfies it with `var _ sandbox.Sandbox`. Selection is the existing `New`/`pick`/probe machinery (`select.go`). **No caller changes** — `cmd/nilcore`'s `selectSandbox` call (`main.go:1350-1366`) is byte-identical; only `New`'s internals gain an arm. |

**EXT boundary (thesis).** EXT-08 grants **no outward standing authority** — no cloud control plane, no
IdP, no remote credential store, no network surface of its own. The one capability it requires is a **local
device** (`/dev/kvm`). The line it does **not** cross — *provisioning microVMs on remote hosts, a managed
fleet that leases work to them, cross-host VM state* — is `EXT-01`, named here as a future dependency and
explicitly out of scope. That is exactly why EXT-08 is the **lowest-risk EXT item** and the only one that
*strengthens* rather than stresses the invariants.

---

## §8 Module justifications

**Net new modules: zero.** This is the `I6`/G4 commitment, and it is achievable because the Firecracker
control plane and the KVM/vsock surface are both reachable from the existing dependency set.

- **VMM control plane — stdlib `net/http` over a unix socket (no SDK).** Firecracker's VMM is driven by a
  documented REST-ish API spoken as **HTTP over an AF_UNIX socket**. The Go stdlib serves this directly: an
  `http.Client` whose `Transport.DialContext` dials the unix socket. The official `firecracker-go-sdk`
  exists but pulls a dependency tree (logging, retry, etc.) that violates the zero-dep default — so EXT-08
  **hand-rolls a minimal client** (`internal/sandbox/fcclient`, T02), exactly as the codebase hand-rolls the
  MCP JSON-RPC client to stay stdlib (`docs/ARCHITECTURE.md` §I6: *"The MCP client is not a dependency: it
  speaks JSON-RPC 2.0 over the standard library"*) and hand-rolls PBKDF2 to avoid a module
  (`docs/ROADMAP-EXTERNAL-INFRA.md:18`). The `deps_test.go` enforces it.
- **KVM probe + vsock transport — `golang.org/x/sys/unix` (already a direct dep).** Opening `/dev/kvm`, the
  `KVM_GET_API_VERSION` ioctl, and the AF_VSOCK `connect` are syscalls already covered by
  `golang.org/x/sys/unix` — which is **already a direct module** (`go.mod:8`) and is the *sanctioned* dep
  scoped to `internal/sandbox` for exactly the namespace backend's Landlock/seccomp/`no_new_privs` syscalls
  (`docs/ARCHITECTURE.md` §I6). EXT-08 adds **no new import beyond it**, so the module graph is unchanged.
- **CGO-free.** The host backend is Linux-build-tagged with a `!linux` stub (so macOS/Windows keep building);
  the guest agent (`cmd/nilcore-guest`) builds `CGO_ENABLED=0` static. No cgo anywhere — the release
  cross-compile matrix (`.github/workflows/release.yml`) is preserved.

**Conclusion:** EXT-08 ships with **no `go.mod` change**, satisfying G4/`I6` outright. (If a future operator
*prefers* the official SDK, that is a separate, justified `go.mod` PR — not part of this plan.)

---

## §9 Default-off byte-identical proof

The default `nilcore` binary is **byte-identical** whether or not the microVM backend exists, on the hosts
that ship it today — proven, not asserted, the same way the namespace backend and swarm mode prove it
(`docs/SWARM.md:458-463`):

1. **Auto falls back, unchanged.** `pick` with `prefer=auto` on a **KVM-absent** host (a VPS, macOS,
   `ubuntu-latest`) returns exactly what it returns today: `namespace` when the kernel supports it, else
   `container` (`select.go:65-69`). The microVM arm is only reachable when `microvmProbe` returns true — and
   the probe is **conservative** (false-negative-preferring, §3.5), so on every current default host it
   returns false and the selection is the prior selection. **Absent KVM ⇒ container/namespace, unchanged.**
2. **No existing caller changes.** `cmd/nilcore`'s `selectSandbox` (`main.go:1350-1366`) passes the same
   `Options`; `Options` gains **no field** (the rootfs reference reuses `Image`); the egress wiring adds a
   *new* arm (`applyMicroVMEgress`) that the existing `*Container` path never reaches. The
   `*Container`/`*Namespace` code paths are untouched.
3. **Off Linux it cannot link a single KVM byte.** The Linux impl is build-tagged; the `!linux` stub
   (`microvm_other.go`) is a probe-returns-false + defensive-guard pair (mirror `namespace_other.go`), so the
   macOS maintainer build and the darwin release artifact contain **no** microVM execution code.
4. **No global side effects.** The new leaves (`fcclient`, `guestagent`, `microvm_linux.go`) have **no
   `init()`** with global side effects, so merely linking them (were they ever pulled in) cannot change
   behavior — the same property swarm mode's leaves are tested for (`docs/SWARM.md:462`). A test asserts it.
5. **Removable.** Deleting the microVM files restores the prior tree with **zero** caller change (the only
   edits to existing files are: one `pick` arm + probe var + `Available` field in `select.go`, one egress arm
   in `chat.go`, one doctor line in `main.go`, one additive Makefile target, one additive CI job — each
   additive and reversible).

**The byte-identity claim is bounded honestly:** it holds for the hosts that run NilCore today (no
`/dev/kvm`). On a host an operator *opts into* with `-sandbox microvm`, behavior changes by design — that is
the feature. The default never changes.

---

## §10 Risks & honest caveats

- **The gate is real and standing (G1).** KVM-capable infra (bare metal / nested virt) is the hard
  requirement; a typical cheap VPS cannot run EXT-08. This is the documented, accepted trade — but it means
  EXT-08 ships value **only** to operators with the hardware. The default tiers (container/namespace) remain
  the answer for everyone else; EXT-08 never becomes a requirement.
- **CI can't be GitHub-hosted.** `ubuntu-latest` exposes no `/dev/kvm`, so the authoritative
  `sandbox-linux-kvm` job (T08) needs a **self-hosted or nested-virt** runner. Until that exists, the live
  confinement is unverified — and per `I2`/G3 the security claim is only as good as the must-run job. The
  hermetic `make verify` covers the *logic* (pick/probe/env-assembly/egress-wiring) on any OS, but the
  *boundary* is proven only by the KVM job. This is the same posture as `browser-e2e` (`ci.yml:83`).
- **Boot latency & per-shard cost.** A microVM boot is heavier than a namespace re-exec or a container
  start. For high-fan-out swarm mode (`docs/SWARM.md`), microVM-per-shard may be too slow; the mitigation is
  the existing spectrum — swarm shards can stay on namespace/container while a *security-sensitive* single
  task uses microVM. EXT-08 does **not** mandate microVM for any path; it is opt-in per the same `-sandbox`
  selector.
- **Rootfs/kernel maintenance.** A guest kernel + rootfs is a maintained artifact (security updates,
  toolchain drift) — a standing cost the container image already has, but new for the namespace backend's
  "no image" world. Mitigated by the pinned-by-digest reproducible recipe (T07) and keeping the rootfs
  minimal.
- **virtio-fs vs virtio-blk for `/work`.** virtio-fs gives identity-mapped paths (the property the
  container's `/work` and the namespace backend's shared-filesystem provide) but is more complex; virtio-blk
  over a loop image is simpler but needs a sync-back step. T04 should pick one and document the trade; the
  invariant either way is **`/work` is the single writable host-touching surface (I4)**.
- **Jailer posture.** The host-side VMM still runs with enough authority to talk to `/dev/kvm`. The jailer
  posture (drop uid/gid, `no_new_privs`, cgroup/chroot the VMM) bounds that, but it is the one host-side
  attack surface EXT-08 adds — the security review (G2) must cover the VMM process, not just the guest.
- **Scope discipline.** A *managed* microVM fleet (remote provisioning, cross-host VM state) is `EXT-01`,
  not EXT-08 — the plan names it as out of scope to keep a follow-on PR from quietly turning a local
  isolation tier into a control plane.

*EXT-08 is the EXT item that proves the boundary file's thesis: the gap is acknowledged, the path exists,
and building it makes NilCore **better at being NilCore** — a harder sandbox behind the same small
interface, the verifier still the only vote on "done," no secret to the model, no module added, and the
default binary unchanged. It strengthens `I4` and touches nothing else. <3*
