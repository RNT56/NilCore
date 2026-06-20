# Roadmap — NilCore as a verifier-backed artifact factory

**North star.** NilCore today verifies *code*. This upgrade makes **code one artifact type among many**: reports, comparison matrices, audits, benchmarks, migration plans, release notes, and research dossiers become first-class outputs, each carrying **machine-verifiable acceptance criteria**. Every claim, number, and table-cell rides with provenance `{value, source_url, retrieved_at, extraction_method, verifier, status}`, and the artifact is **GREEN only because every claim passed a runnable check** — not because the agent sounded careful. The existing thesis is unchanged and *extended*: the verifier is still the sole authority on "done" (I2), the model is still the engine, the harness stays small.

This is **Phase 11**, namespace `P11-T##`. Like `docs/UPGRADE-PATH.md` and `docs/ROADMAP-REPORT-FIXES.md`, this is a **staging doc**, not the canon: it presumes all seven invariants and the frozen `backend.CodingBackend` contract and never restates the law — it points at it (`CLAUDE.md` → `docs/ARCHITECTURE.md`). Promoting these specs into `docs/TASKS.md` is itself a **serialized contract task** (`P11-T36`, §9). Every task is **additive, opt-in, flag/env-gated, stdlib-first** (no new module; `CGO_ENABLED=0`) — **the default binary stays byte-identical when the feature is off**, and each spec carries the test that proves it.

> **SHIPPED + built upon.** Phase 11 is merged (PR #47/#48). **Phase 12 — verified swarm mode** (`docs/SWARM.md`, `nilcore swarm`) is the high-throughput product surface built directly on this spine: it **reuses** `internal/{artifact, evverify, artifact/packs/*, requeue, report}` to fan hundreds of agents into a bounded in-process pool where every shard produces a typed artifact judged by a verify-pack and only verifier-green shards ship — failed shards requeue until clean. This doc remains the design of the spine itself; the swarm extends it, never rebuilds it.

---

## 1. Overview — the six pillars

| Pillar | What it adds | Why it's the value center |
|---|---|---|
| **1 — Evidence-verified artifacts (the spine)** | A typed artifact contract (`internal/artifact`): `Artifact{Claims[]}`, each `Claim` carrying `Evidence{value, source_url, retrieved_at, extraction_method, verifier, status}`; an `ArtifactVerifier` that runs every claim's check and sets its status. | This is what lets NilCore *say* "GREEN because every claim passed a runnable check". Every other pillar builds on this data model. Highest value. |
| **2 — Domain verifier packs** | Small, sandboxed, typed, **reusable** verifier-id packs for web-research, software-research, finance/market, and UI/browser — registered into the spine's `Registry`. | Turns ad-hoc shell snippets into a typed, reusable catalog. A finance pack and a `finance` egress profile are co-designed: the pack can only reach its sanctioned sources, and a denied host fails the claim closed. |
| **3 — Typed worker results** | A research subagent returns a typed `Artifact` (a worktree JSON file) instead of bounded prose; the supervisor treats the **verifier-set** claim statuses as the only mergeable output and keeps prose as fenced commentary. | Removes "trust the agent's summary" from research fan-out. Mergeable output is harness-computed, never model self-claimed. |
| **4 — Granular requeue** | Failure is addressable at **one claim**: requeue exactly "company-041 revenue mismatch / source 404 / margin missing", not "the run failed, try again". A bounded retry ledger converges red rather than spinning. | Reuses the existing DAG + `continue_from`; makes failures addressable at field granularity so a swarm fixes the broken cell, not the world. |
| **5 — Research egress profiles** | Named, opt-in egress presets (`--egress-profile finance\|docs\|web-research`) + a project-local `.nilcore/egress.json` allowlist, intersected narrow-only per role. | Keeps default-deny absolute while making live data access **intentional, auditable, reproducible**. The one audited toggle widens the tree; per-role intersection still clamps. |
| **6 — Verification UI / report** | A read-only `nilcore report <run>` projection over the append-only log + persisted artifacts: passed/failed checks, the per-claim `{value,source_url,verifier,status}` table, verifier output tails, retry history, final clean pass — text/HTML/markdown. | Makes the trust story **visible**, not buried in logs. The human face of I2. |

**NON-GOALS** (do not plan these): generic web browsing without typed verification; more model/config branding knobs as a headline; a vague "research mode" that still emits Markdown-with-citations and no machine checks. A markdown *render* of verifier-set statuses (Pillar 6) is permitted **only** because it is a read-only projection of harness-computed GREEN/RED, never a citations-emitter.

---

## 2. The shared SPINE architecture (Pillar 1)

Everything hangs off two new **leaf** packages plus one extracted confinement leaf.

### 2.1 The data model — `internal/artifact` (leaf, stdlib only)

```go
package artifact

type Status string
const (
	StatusUnverified   Status = "unverified"   // initial; verifier has not run
	StatusPass         Status = "pass"          // verifier ran and asserted true — the ONLY green
	StatusFail         Status = "fail"          // verifier ran and asserted false — value is WRONG (requeue: re-derive)
	StatusStale        Status = "stale"         // source resolved but freshness failed (requeue: re-fetch)
	StatusUnverifiable Status = "unverifiable"  // no decisive verdict: 404, no verifier bound, host denied (requeue: fix source/binding)
)

type Kind string
const (
	KindReport    Kind = "report"
	KindMatrix    Kind = "matrix"
	KindSpec      Kind = "spec"
	KindBenchmark Kind = "benchmark"
	KindDossier   Kind = "research-dossier"
)

type Evidence struct {
	Value            string    `json:"value"`                       // asserted datum — UNTRUSTED (model-authored)
	SourceURL        string    `json:"source_url,omitempty"`        // provenance — UNTRUSTED; MUST be key-free (I3)
	RetrievedAt      time.Time `json:"retrieved_at,omitempty"`      // provenance; a HINT, never a basis to PASS (I2)
	ExtractionMethod string    `json:"extraction_method,omitempty"` // UNTRUSTED
	Verifier         string    `json:"verifier,omitempty"`          // verifier-id resolved via the Registry
	Status           Status    `json:"status"`                      // set BY the verifier — TRUSTED
	Detail           string    `json:"detail,omitempty"`            // verifier's bounded output tail — TRUSTED
}

type Claim struct {
	ID        string   `json:"id"`        // stable, run-spanning requeue key (e.g. "company-041-revenue")
	Field     string   `json:"field"`     // semantic label (e.g. "revenue_fy2024")
	Statement string   `json:"statement,omitempty"` // optional prose context — UNTRUSTED, never an instruction
	Evidence  Evidence `json:"evidence"`
}

type Artifact struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Kind          Kind      `json:"kind"`
	Title         string    `json:"title,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	Claims        []Claim   `json:"claims"`
}

// Green is a PURE projection: true iff len(Claims)>0 && every claim StatusPass.
// Authoritative green is the verify.Report the ArtifactVerifier returns; Green() must AGREE.
// Empty Claims => NOT green (fail-closed: an artifact that asserts nothing cannot be trusted-green).
func (a *Artifact) Green() bool { ... }
```

Artifacts live as JSON files in the worktree at the fixed path **`.nilcore/artifacts/<id>.json`**, written by the worker via the existing sandboxed write/edit tools. They ride **entirely out-of-band** of `backend.Task`/`backend.Result` (so I1 holds), inside the sandboxed worktree (I4), and their verification outcomes append as new event kinds (I5).

### 2.2 Worktree confinement — `internal/worktreefs` (leaf, stdlib only)

The symlink-safe path-join + `O_NOFOLLOW` + atomic-temp-rename discipline lives **unexported** today in `internal/tools/fs.go`. Three new consumers (artifact store, report writer, the verifier write-back) need it, and **must not each hand-roll it** — that multiplies the most security-load-bearing code in the tree. `internal/worktreefs` extracts those primitives into a zero-nilcore-import leaf; `internal/tools/fs.go`, `internal/artifact`, and `internal/report` all import it. (Auditor B1.)

### 2.3 Verifier binding — `internal/artifact/evverify` (leaf: imports `artifact` + `verify` + `sandbox` + `worktreefs`)

```go
type CheckFunc func(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string)

type Registry struct{ /* map[string]CheckFunc */ }
func New() *Registry
func Default() *Registry                          // generic stdlib checks only; NO always-pass verifier
func (r *Registry) Register(id string, fn CheckFunc)
func (r *Registry) Lookup(id string) (CheckFunc, bool)

type ArtifactVerifier struct {
	Box       sandbox.Sandbox
	Reg       *Registry
	RelPath   string
	MaxAge    time.Duration          // 0 => staleness disabled
	EventSink func(ev any)           // nil-able; cmd supplies the eventlog-backed impl
}
func (v *ArtifactVerifier) Check(ctx context.Context) (verify.Report, error) // implements verify.Verifier
```

**Artifact-green is verifier-produced (I2).** `Check` loads the artifact via `worktreefs`, resolves each claim's `Evidence.Verifier` through `Reg`, runs the `CheckFunc` **in the box** (`box.Exec`/`ExecWithEnv` — I4, inherits the egress allowlist), **overwrites every claim's status** (so a worker that self-wrote `Status=pass` is replaced by a real verdict), writes the artifact back atomically, and returns `verify.Report{Passed: every claim StatusPass}`. It plugs into `verify.Composite` as one more `NamedVerifier` **after** `Named[0]` (the build/`make verify` verifier), so an artifact check can never mask a red build and any red claim turns the whole verdict red. Fail-closed: missing file / parse error / empty claims / unregistered id ⇒ `Passed:false`.

**The Registry is the seam Pillar 2 fills.** A claim names `Evidence.Verifier = "finance.sec_fact"`; a pack's `RegisterAll(r)` made that id resolvable. An unregistered id ⇒ `StatusUnverifiable`, never `Pass`.

---

## 3. Pillar designs

Each subsection states its packages, core types, integration seams (with `file:line` where it helps), an **Invariant compliance** line, and the **Additive/opt-in/byte-identical-when-off** line.

### 3.1 Pillar 1 — Evidence-verified artifacts (the spine)

**Design.** §2. New leaf packages `internal/worktreefs`, `internal/artifact`, `internal/artifact/evverify`. Wiring in `cmd/nilcore/verifier.go` (`behavioralVerifier`, the lone `Composite` assembly site) behind `NILCORE_EVIDENCE_VERIFY`/`-evidence-verify`, mirroring the existing `NILCORE_BROWSER_VERIFY` branch.

**Core types.** `artifact.{Artifact,Claim,Evidence,Status,Kind}`; `evverify.{CheckFunc,Registry,ArtifactVerifier}`.

**Integration seams.**
- `verify.Composite.Named` (`internal/verify/composite.go:17`) — append `NamedVerifier{Name:"evidence", V:av}` after `Named[0]`. No change to `internal/verify`.
- `cmd/nilcore/verifier.go behavioralVerifier` — env-gated assembly; unset ⇒ bare `verify.New` (byte-identical).
- Worktree FS — the artifact file is the out-of-band carrier; SpawnFunc/app verifier reads it back from the worktree it owns.
- eventlog `Detail` — additive kinds `artifact_verify {id,kind,green,pass,fail,stale,unverifiable}`, `claim_verify {claim_id,field,status,source_url}` via the nil-able `EventSink` func (leaf never imports eventlog).

**Invariant compliance.** I1 (no `backend.go` edit; artifact rides a worktree file). I2 (green produced by `ArtifactVerifier` in `Composite`, fail-closed). I4 (every check via `box.Exec`; FS via `worktreefs` `O_NOFOLLOW`). I5 (new append-only kinds). I6 (stdlib only). I7 (model-authored fields are data the verifier asserts over; only harness-set status/detail are trusted).

**Additive/opt-in.** With `NILCORE_EVIDENCE_VERIFY` unset, no `ArtifactVerifier` is wired, no kinds emit, `behavioralVerifier` returns bare `verify.New` — **byte-identical** (`P11-T05` asserts the unset path).

### 3.2 Pillar 2 — Domain verifier packs

**Design.** Four leaf packs under `internal/artifact/packs/<domain>` (`web`, `software`, `finance`, `ui`), each importing only `artifact` + `evverify` + `sandbox` + stdlib, exporting `RegisterAll(*evverify.Registry)` that registers namespaced ids. A `packs` aggregator (`packs.go`) exposes `Select(names, r)` and `HostsFor(name)`. Every check reduces to **one** `box.Exec`/`ExecWithEnv` (curl or the `nilcore-browser` CDP driver), parses the response **host-side as trusted Go** (verifier code, not the model — no `guard.Wrap` before parse), and returns a typed `Status` + bounded detail.

**Verifier-id catalog.**
- `web.url_resolves`, `web.quote_exists`, `web.date_matches`, `web.not_stale`
- `software.npm_version_exists`, `software.pypi_version_exists`, `software.crate_version_exists`, `software.github_release_exists`, `software.github_tag_exists`, `software.license_matches`
- `finance.sec_fact`, `finance.fred_series` *(keyed)*, `finance.worldbank_indicator`, `finance.imf_series`, `finance.market_quote` *(keyed)*
- `ui.flow_passes`, `ui.no_console_errors`, `ui.screenshot_captured`

**Integration seams.** `evverify.Registry.Register` (the one seam); `packs.Select` at the `cmd/nilcore/verifier.go` wiring; `sandbox.Sandbox.Exec`/`ExecWithEnv`; `SecretStore → box.ExecWithEnv` (`$NAME`, keyed packs); `packs.HostsFor → Pillar 5` profiles.

**Invariant compliance.** I1 (no backend touch). I2 (a pack asserts; never self-reports; unknown id ⇒ `Unverifiable`). I3 (keyed packs reference `$NAME`; the key VALUE is injected via `ExecWithEnv` from `SecretStore`, never in the curl string, the artifact `SourceURL`, or the event Detail — auditor B2). I4 (every reach via the box; nil box / denied host ⇒ `Unverifiable`). I6 (curl-in-box + `encoding/json`; no go-github / finance SDK; `go.mod` unchanged). I7 (fetched body parsed host-side as data; only a bounded harness detail leaves the pack).

**Additive/opt-in.** `NILCORE_VERIFY_PACKS` unset ⇒ no pack ids registered ⇒ any pack-claim resolves `Unverifiable` ⇒ registry equals `Default()` ⇒ byte-identical (`P11-T12` asserts).

### 3.3 Pillar 3 — Typed worker results

**Design.** A typed-research role writes its result as `.nilcore/artifacts/<id>.json` (existing write tools). The host-side `buildSpawnFunc` (`cmd/nilcore/build.go`), which already owns the worktree path and re-runs the verifier, reads it back **after** the `ArtifactVerifier` overwrote each status and attaches a harness-authored projection to a **new additive field** `spawn.Result.Artifact`. `renderReport` (`internal/super/dispatch.go:721`) — shared by serial AND concurrent dispatch — renders the GREEN/RED claim table as **trusted** control lines while the worker's prose `Summary` stays `guard.Wrap`-fenced.

```go
// internal/spawn — leaf flat strings (no import of internal/artifact)
type ClaimStatus struct { ID, Field, Status string }       // NO Value/SourceURL — trusted surface only
type ArtifactSummary struct { ID, Kind string; Green bool; Claims []ClaimStatus }
type Result struct { ID, Summary, Branch string; Passed bool; State State; Err error; Artifact *ArtifactSummary }
```

**Critical wiring (auditor blocker).** The `ArtifactVerifier` must be composed into the **per-subagent** `env.Verifier` (built in `buildEnvFactory`/`newEnv`, `cmd/nilcore/build.go:698`), **not only** the app-level `behavioralVerifier`. The two are separate paths; without `P11-T16` composing evidence verification into `env.Verifier` for the typed-research role, claim statuses stay `Unverified` and `spawn.Result.Passed` can never reflect per-claim green.

**Integration seams.** `buildSpawnFunc` (`build.go:719-755`, reads artifact after `env.Verifier.Check`); `spawn.Result.Artifact` additive field (carried verbatim by `Spawner`, `DAGScheduler.waveResults`); `renderReport` (the trusted/fenced split); roster typed-research `Profile`.

**Invariant compliance.** I1 (no `backend.Result` edit; field on `spawn.Result`, not a contract file). I2 (`Passed` governed solely by `env.Verifier.Check`; the artifact is read after `Passed` is decided; a non-green artifact never flips `Passed` true). I3 (projection copies only id/field/kind/status/bool — no `source_url`/`value`). I7 (only verifier-produced fields are trusted; prose stays fenced; `ClaimStatus` carries no model-authored field by construction).

**Additive/opt-in.** Flag off OR role ≠ typed-research ⇒ `Artifact` nil ⇒ `renderReport` byte-identical; `spawn.Result.Artifact` is `json:",omitempty"` pointer so default serialization has no `artifact` key (`P11-T14` golden test).

### 3.4 Pillar 4 — Granular requeue

**Design.** A leaf `internal/requeue` (imports only `artifact` + stdlib) derives a worklist of failed **units** from the verifier-set claim statuses, groups them into the **minimal** focused re-dispatch subtasks (Goal names only the red claim ids; base cut via `SubagentSpec.ContinueFrom` from the prior attempt so passing claims are preserved), and bounds retries with a per-unit `Ledger` persisted in `store.Task.Detail` alongside `agent.RunState`. **Invents no loop** — it reuses `spawn.DAGScheduler` + `ContinueFrom` + `preserveFailedAttempt` verbatim. A unit flips green only when a **fresh** `ArtifactVerifier` re-run reports `StatusPass` (I2).

```go
type Unit struct { ArtifactID, ClaimID, Field string; Status artifact.Status; Detail, OwnerSubagent string; Attempt int }
type Worklist struct { Units []Unit }
func Scan(root string, led *Ledger) (Worklist, error)   // one Unit per non-pass claim
type Ledger struct { MaxAttempts int; Attempts map[string]int }  // MaxAttempts==0 => requeue disabled
type Subtask struct { ID, Goal string; DependsOn []string; ContinueFrom string; UnitKeys []string }  // NOT spawn.Subtask
func Plan(w Worklist, led *Ledger) []Subtask
func Resolve(before, after Worklist, led *Ledger) (resolved, stillFailed, exhausted []Unit)
```

**Status routes the fix:** `fail` ⇒ re-derive the value; `stale` ⇒ re-fetch the source; `unverifiable` ⇒ fix source/binding.

**Integration seams.** `spawn.DAGScheduler` (reused); `SubagentSpec.ContinueFrom` + `preserveFailedAttempt` (`build.go:781`); `artifact.Read` + `evverify.ArtifactVerifier`; `store.Task.Detail` (Ledger as additive sibling JSON); a nil-gated `super.RequeueHook`; additive event kinds `claim_requeue`/`claim_resolved`/`requeue_exhausted`.

**Invariant compliance.** I1 (a requeue is an ordinary `spawn.Subtask`/`backend.Task` re-run). I2 (green only from a fresh verifier re-run, never a stored status). I5 (retry history is new append-only kinds; the mutable Ledger lives in `store.Task.Detail`; every disposition change appends, never edits). I6 (`internal/artifact` + stdlib only). I7 (units derive from harness-written statuses; the focused Goal is harness-authored control text).

**Additive/opt-in.** `NILCORE_REQUEUE` unset / `MaxAttempts==0` ⇒ no requeue code runs, no kinds emit, no extra store write ⇒ byte-identical (`P11-T23` asserts).

### 3.5 Pillar 5 — Research egress profiles

**Design.** A stdlib-only leaf `internal/egressprofile` owns three named presets (`finance`|`docs`|`web-research`) as labeled `policy.Egress` constructors (host sets **co-designed with Pillar 2's packs**) plus the project-local `.nilcore/egress.json` loader. A `-egress-profile` flag + `NILCORE_EGRESS_PROFILE` env select a preset; `Resolve(profileName, filePath)` unions preset + file into one tree allowlist, expanded into `resolveWeb` (`cmd/nilcore/chat.go:278`) before the search-host auto-add. The security direction is precise: a profile **widens the TREE** from deny-all to the named set, but `roster.EgressFor` (untouched, `internal/roster/worker.go:140`) still intersects each role's `Profile.Egress` against that tree (narrow-only, R9). `build.go:687`'s hardcoded deny-all tree is flipped to the resolved profile tree **only** when a profile is opted in (the single audited toggle).

**Integration seams.** `policy.Egress` (presets produce instances); `resolveWeb` (host expansion); `roster.EgressFor` (narrow-only intersection, unchanged); `onboard.WebConfig.{Profile,ProfileFile}` (persistence); `wizard.go:272` (`NILCORE_EGRESS_PROFILE`); `build.go:687` (the toggle); a metadata-only `egress_profile` event.

**Invariant compliance.** I3 (allowlist holds hostnames only; keyed sources keep keys in `SecretStore` via `ExecWithEnv`; the event is metadata-only). I4 (only ADDS hosts to the existing proxy/sandbox path; the namespace backend has no proxy path and stays hard deny-all — a profile requires the `*Container` backend, surfaced loudly). I5 (metadata-only `egress_profile` event). I6 (stdlib `encoding/json` + `os.ReadFile`). R9 (`EgressFor` unchanged; a deny-all role stays `--network none` under any profile).

**Additive/opt-in.** No profile + no flag + no enabled config ⇒ `resolveWeb` returns nil, no proxy, `build.go` keeps `policy.Egress{}` ⇒ byte-identical (`P11-T28` asserts the unset golden path).

### 3.6 Pillar 6 — Verification UI / report

**Design.** A read-only leaf `internal/report` (imports only `eventlog` + `artifact` + `worktreefs` + stdlib) replays the append-only log into a typed `ReportModel`, folds persisted artifacts, calls `eventlog.Verify`, and **refuses to render GREEN over a broken chain**. Pure renderers in `internal/report/render` (imports `report` + `termui` for the `Style` type only) produce text (TTY-styled, plain on pipe/CI), self-contained script-free HTML, and markdown. A new `nilcore report <run>` subcommand (`cmd/nilcore/report.go`) renders to stdout and optionally writes `.nilcore/reports/<run>.{html,md,txt}` via the report writer (reusing `worktreefs`). **Emits no new event kinds — purely a reader (I5).**

```go
type ReportModel struct {
	Run string; GeneratedAt time.Time; ChainVerified bool
	Checks []CheckResult; Artifacts []ArtifactView; Retries []RetryAttempt; FinalPass bool
}
type CheckResult struct { Family, Name, Task string; Passed, Stale bool; Output string; Seq uint64; At time.Time }
type ClaimRow struct { ClaimID, Field, Value, SourceURL string; RetrievedAt time.Time; Verifier string; Status artifact.Status; Detail string }
type RetryAttempt struct { Task, ContinueFrom, BaseBranch string; Passed bool; Seq uint64; At time.Time }
```

**Retry-history sources (auditor blocker).** The existing `subagent_report` event Detail carries only `{passed,branch,has_err}` (`internal/super/dispatch.go:169`) — **no `continue_from`**. So retry history is sourced from the **GRA-emitted** `claim_requeue`/`claim_resolved`/`requeue_exhausted` kinds (which carry `attempt`+`claim_id`), with `subagent_report` `continue_from` as a *secondary* signal only **after** `P11-T17a` additively enriches that Detail (gated; emitted only when `spec.ContinueFrom != ""`). A log lacking the requeue kinds still produces a valid `ReportModel`. **CostLine is dropped** (auditor blocker): no token/usage events are appended to the eventlog today (`internal/meter` charges `budget.Ledger` only), so a "cost" line would be permanently `unknown`/untestable — it is out of scope until a `meter`-emits-`usage` task exists.

**Integration seams.** `report.ReplayReport(logPath, worktreeRoot)`; `artifact.Read`; eventlog `Detail` (read-only) across families `verify`/`final_verify`/`project_verify`/`project_acceptance`/`integration_verify`/`integration_rollback`/`integration_conflict`/`artifact_verify`/`claim_verify`/`claim_requeue`/`claim_resolved`/`requeue_exhausted`; `termui.Style`; the `report` subcommand (parallel to `inspect`/`health`).

**Invariant compliance.** I5 (pure read; calls `eventlog.Verify`; broken chain ⇒ `ChainVerified=false` ⇒ no green). I2 (renders the verifier's verdict; `FinalPass` from logged statuses, never SelfClaimed). I7 (model-authored `Value`/`SourceURL` `html.EscapeString`-escaped; no `<script>`, no external asset). I3 (SourceURL/Detail run through the eventlog redact path; a secret seeded in a Detail tail is redacted in all three formats). I6 (hand-rolled HTML/markdown; no template/markdown module).

**Additive/opt-in.** `nilcore report` is a new subcommand; default `nilcore inspect` and all existing subcommands are byte-identical (`P11-T33` asserts).

---

## 4. Master DAG

| ID | Depends on | Owns | Contract? | Wave | Title |
|---|---|---|---|---|---|
| P11-T00 | — | `internal/worktreefs` | no | 1 | Extract worktree-confinement leaf (symlink-safe + O_NOFOLLOW + atomic rename) |
| P11-T01 | T00 | `internal/artifact` | no | 2 | artifact leaf: data model, JSON, status lifecycle |
| P11-T02 | T01 | `internal/artifact` | no | 3 | artifact worktree persistence (atomic, symlink-safe via worktreefs) |
| P11-T03 | T01 | `internal/artifact/evverify` | no | 3 | evverify: Registry + CheckFunc dispatch seam |
| P11-T04 | T02, T03 | `internal/artifact/evverify` | no | 4 | evverify.ArtifactVerifier: bind claims to the verifier (I2) |
| P11-T05 | T04 | `cmd/nilcore/verifier.go` | no | 5 | Wire evidence verification behind NILCORE_EVIDENCE_VERIFY |
| P11-T06 | — | `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` | no | 1 | Staging doc: spine (this doc) |
| P11-T07 | T03 | `internal/artifact/packs/web` | no | 4 | web-research pack |
| P11-T08 | T03 | `internal/artifact/packs/software` | no | 4 | software-research pack |
| P11-T09 | T03 | `internal/artifact/packs/finance` | no | 4 | finance/market pack (keyed+keyless) |
| P11-T10 | T03, T11a | `internal/artifact/packs/ui` | no | 4 | ui-browser pack (via nilcore-browser CDP driver) |
| P11-T11 | T07, T08, T09, T10 | `internal/artifact/packs/packs.go` | no | 5 | pack aggregator + selector |
| P11-T11a | — | `internal/browserwire` | no | 1 | Extract shellSingleQuote + browserObservation leaf |
| P11-T12 | T11, T05 | `cmd/nilcore/verifier.go` | no | 6 | Wire pack selection behind NILCORE_VERIFY_PACKS |
| P11-T13 | T06 | `docs/ROADMAP-DOMAIN-PACKS.md` | no | 2 | Staging doc: domain verifier packs |
| P11-T14 | — | `internal/spawn` | no | 1 | spawn.Result typed-artifact field (additive, nil-gated) |
| P11-T15 | — | `internal/roster` | no | 1 | Typed-research Role/Profile in roster |
| P11-T16 | T14, T15, T02, T04, T05 | `cmd/nilcore/build.go` | no | 6 | buildSpawnFunc reads verified artifact; compose ArtifactVerifier into env.Verifier |
| P11-T17 | T14 | `internal/super` | no | 2 | renderReport: typed claims trusted, prose fenced |
| P11-T17a | T17 | `internal/super` | no | 3 | Enrich subagent_report Detail with continue_from/base (gated) |
| P11-T18 | T13 | `docs/ROADMAP-TYPED-RESULTS.md` | no | 3 | Staging doc: typed worker results |
| P11-T19 | T02 | `internal/requeue` | no | 4 | requeue leaf: Unit, Worklist, Scan |
| P11-T20 | T19 | `internal/requeue` | no | 5 | requeue.Ledger: bounded retry budget |
| P11-T21 | T19, T20 | `internal/requeue` | no | 6 | requeue.Plan + Resolve: focused subtasks, green-flip |
| P11-T22 | T17a | `internal/super` | no | 4 | super: nil-gated RequeueHook at convergence-red |
| P11-T23 | T21, T22, T05, T16 | `cmd/nilcore/requeue_wiring.go` | no | 7 | Wire granular requeue behind NILCORE_REQUEUE |
| P11-T24 | — | `docs/ROADMAP-GRANULAR-REQUEUE.md` | no | 1 | Staging doc: granular requeue |
| P11-T25 | — | `internal/egressprofile` | no | 1 | egressprofile leaf: named presets + Resolve |
| P11-T26 | T25 | `internal/egressprofile` | no | 2 | egressprofile project-local allowlist file |
| P11-T27 | T25 | `internal/onboard` | no | 2 | onboard.WebConfig persistence + validation |
| P11-T28 | T26, T27, T05, T12, T16, T23, T33 | `cmd/nilcore` (egress wiring files) | no | 8 | Wire -egress-profile through both front doors |
| P11-T29 | T18 | `docs/ROADMAP-EGRESS-PROFILES.md` | no | 4 | Staging doc: research egress profiles |
| P11-T30 | T01, T02 | `internal/report` | no | 4 | report leaf: ReportModel + log-replay projection |
| P11-T31 | T30 | `internal/report` | no | 5 | report worktree writer (atomic, via worktreefs) |
| P11-T32 | T30 | `internal/report/render` | no | 5 | report/render: text + HTML + markdown renderers |
| P11-T33 | T31, T32 | `cmd/nilcore/report.go` | no | 7 | Wire `nilcore report` subcommand |
| P11-T34 | — | `docs/ROADMAP-VERIFICATION-REPORT.md` | no | 1 | Staging doc: verification report |
| P11-T35 | T07, T08, T09, T10, T11, T25 | `cmd/nilcore` (egress-pack consistency test) | no | 8 | Cross-check: every pack host ⊆ its egress profile |
| P11-T36 | T06, T13, T18, T29, T24, T34 | `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `CHANGELOG.md` | **YES** | 9 | PROMOTION (serialized contract task) |

> **cmd/nilcore file-granularity (read this).** `CLAUDE.md` §5 makes a *package directory* the ownership unit, but Phase 11 has six `cmd/nilcore` wiring edits. To keep them schedulable, the wiring tasks `T05`, `T12`, `T16`, `T23`, `T33`, `T28`, `T35` are claimed at **file granularity** and **fully serialized** — only one open `cmd/nilcore` branch at a time. `T28` and `T35` own the whole dir (egress front-doors + a cross-package test) and therefore run last among cmd tasks. The integrator MUST enforce one-cmd-task-at-a-time and reject any attempt to widen a file-scoped cmd task to the dir while another is open. (Auditors: parallel-readiness blockers 2/3.)

---

## 5. Per-task specs

### P11-T00 — Extract worktree-confinement leaf

- **Goal:** Move the symlink-safe path-join + `O_NOFOLLOW` + atomic temp-rename primitives out of `internal/tools/fs.go` (unexported) into a new stdlib-only leaf `internal/worktreefs`, so `internal/tools`, `internal/artifact`, and `internal/report` all import one audited copy instead of hand-rolling three.
- **Depends on:** —
- **Owns:** `internal/worktreefs`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/worktreefs | grep nilcore` returns only `nilcore/internal/worktreefs` (zero nilcore imports).
  - Exposes `SafeJoin(root, rel) (string, error)` (rejects `..`/separators/escape), `OpenNoFollow(path, flag, perm) (*os.File, error)` (`O_NOFOLLOW`), and `WriteAtomic(root, rel string, data []byte, perm) error` (temp + `os.Rename`).
  - A symlink planted at the target path causes `OpenNoFollow`/`WriteAtomic` to fail closed (test).
  - `internal/tools/fs.go` is refactored to call `worktreefs` and a `go test ./internal/tools/` regression passes unchanged (existing tool behavior byte-identical).
  - `git diff --quiet go.mod go.sum` (no new module).
- **Verify:** `make verify` + `go test ./internal/worktreefs/ -run TestConfine` + `go test ./internal/tools/`
- **Notes:** Resolves auditor B1 (one confinement implementation, not three). The `internal/tools` refactor must be behavior-preserving; if it risks a hot-file collision, scope to `fs.go` only.

### P11-T01 — artifact leaf: data model, JSON, status lifecycle

- **Goal:** The stable, stdlib-only artifact contract: `Artifact`/`Claim`/`Evidence`/`Status`/`Kind`, canonical `Marshal`/`Unmarshal` with `schema_version`, `Green()`, and the documented status lifecycle.
- **Depends on:** P11-T00
- **Owns:** `internal/artifact`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/artifact | grep nilcore` returns only `nilcore/internal/artifact` and `nilcore/internal/worktreefs` (used in T02; T01 may not yet import it — assert ≤ those two).
  - Types carry the §2.1 JSON tags exactly; `Status` constants `unverified/pass/fail/stale/unverifiable` defined with a doc comment distinguishing `stale` and `unverifiable` from `fail`.
  - `Green()` true iff `len(Claims)>0 && every claim StatusPass`; table test covers green / one-fail / one-stale / one-unverifiable / empty-claims (⇒ not green).
  - `Marshal`→`Unmarshal` round-trips byte-stable; golden-file test asserts the serialized shape incl. `schema_version`.
  - Public surface minimal (review: no unused exports).
- **Verify:** `make verify` + `go test ./internal/artifact/ -run TestArtifact`
- **Notes:** Dead code until a verifier imports it; default binary unaffected. Pure-data so it can never pull `sandbox`/orchestrator.

### P11-T02 — artifact worktree persistence

- **Goal:** `Write(root, *Artifact)` and `Read(root, id)` confined to `.nilcore/artifacts/<id>.json` via `worktreefs` (symlink-safe, `O_NOFOLLOW`, atomic).
- **Depends on:** P11-T01
- **Owns:** `internal/artifact`
- **Acceptance criteria:**
  - `Write` places JSON at `<root>/.nilcore/artifacts/<id>.json` via `worktreefs.WriteAtomic`; `Read` returns the same `Artifact`.
  - An `id` containing a separator or `..` is rejected; test asserts the error and that no escape file exists.
  - A symlink at the target path is refused (via `worktreefs.OpenNoFollow`); test fails closed.
  - `Read` of a missing file returns a typed not-found (`errors.Is`-distinguishable) distinct from a parse error; corrupt JSON returns a parse error (never silent zero-value).
  - `go list -deps` shows the path-safety comes from `worktreefs` (no hand-rolled `EvalSymlinks`/`OpenFile` in `internal/artifact`).
- **Verify:** `make verify` + `go test ./internal/artifact/ -run TestArtifactStore`
- **Notes:** `.nilcore/artifacts/` is the fixed out-of-band carrier every pillar agrees on.

### P11-T03 — evverify: Registry + CheckFunc dispatch seam

- **Goal:** The `evverify` leaf with `CheckFunc`, `Registry` (`New`/`Default`/`Register`/`Lookup`), and built-in generic checks (`web.url_resolves` curl-in-box; unregistered id ⇒ `Unverifiable`).
- **Depends on:** P11-T01
- **Owns:** `internal/artifact/evverify`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/artifact/evverify | grep nilcore` returns only `artifact`, `sandbox`, `worktreefs`, `verify`, and self (no orchestrator/super/roster).
  - `Lookup` of an unknown id ⇒ `(nil,false)`; resolving an unknown id yields `StatusUnverifiable` with a reason (never `Pass`).
  - `web.url_resolves` runs curl via `box.Exec`, `StatusPass` on HTTP 2xx, `StatusUnverifiable` on non-2xx/unreachable; a **fake sandbox** (exit 0 vs non-0) drives both branches.
  - A nil `Box` to a network `CheckFunc` ⇒ `StatusUnverifiable` (fail-closed), no panic.
  - `Default()` registers only safe generic checks and **explicitly does not** register any always-pass/noop verifier.
  - `git diff --quiet go.mod go.sum`.
- **Verify:** `make verify` + `go test ./internal/artifact/evverify/ -run TestRegistry`
- **Notes:** `CheckFunc` signature is stable — Pillar 2 packs depend on it. Hermetic tests use a fake `sandbox.Sandbox` (no network).

### P11-T04 — evverify.ArtifactVerifier (I2 keystone)

- **Goal:** `ArtifactVerifier` implements `verify.Verifier`: load the artifact, run each claim's `CheckFunc` sandboxed, apply `MaxAge` staleness, overwrite per-claim status, write back atomically, return one `verify.Report` (Passed iff every claim `StatusPass`). Add the nil-able `EventSink`.
- **Depends on:** P11-T02, P11-T03
- **Owns:** `internal/artifact/evverify`
- **Acceptance criteria:**
  - `var _ verify.Verifier = (*ArtifactVerifier)(nil)`.
  - `Report{Passed:true}` only when every claim `StatusPass`; any fail/stale/unverifiable ⇒ `Passed:false` with a per-claim `PASS/FAIL/STALE/UNVERIFIABLE` table in `Output`. Table test: all-pass; one-fail; one-stale (RetrievedAt older than `MaxAge`); one-unverifiable (unregistered id).
  - Missing file / parse error / empty Claims ⇒ `Report{Passed:false}` (fail-closed); test each.
  - After `Check`, the on-disk artifact has each `Evidence.Status` **overwritten** by the verdict (a self-written `Status=pass` is replaced); test asserts written-back statuses.
  - **Staleness never PASSES on a model-authored timestamp:** a claim with `RetrievedAt=now` but an unreachable/stale source does NOT become `StatusPass` via the freshness path — `MaxAge` can only DEMOTE (force re-fetch/`Unverifiable`), it is never the sole basis for `Pass`. `MaxAge==0` disables staleness; `MaxAge` is a struct field settable via `NILCORE_EVIDENCE_MAX_AGE`. Test both. (Auditor I2.)
  - **Model-authored fields are NOT echoed unfenced into `Output`:** the table contains ONLY harness-trusted fields (claim id, field label, status, verifier-id, the verifier's own detail tail) and does NOT echo `Value`/`Statement`/`SourceURL` verbatim. A claim whose `Value` contains injection phrases ("IGNORE PRIOR INSTRUCTIONS …") yields an `Output` where those phrases do not appear unfenced. (Auditor I7.)
  - A nil-`Box` `ArtifactVerifier` returns `Passed:false` with every network claim `Unverifiable` and makes NO host-side network call (test via a `CheckFunc` that errors if reached host-side). (Auditor I4.)
  - `EventSink` non-nil ⇒ called once per claim and once per artifact (Detail-only); nil ⇒ byte-identical behavior; evverify imports no eventlog package.
- **Verify:** `make verify` + `go test ./internal/artifact/evverify/ -run TestArtifactVerifier`
- **Notes:** The I2 keystone — green is PRODUCED here. Compose into `verify.Composite` as a trailing `NamedVerifier`; `Named[0]` stays the build verifier.

### P11-T05 — Wire evidence verification (NILCORE_EVIDENCE_VERIFY)

- **Goal:** In `cmd/nilcore/verifier.go behavioralVerifier`, append an `ArtifactVerifier` `NamedVerifier` when `NILCORE_EVIDENCE_VERIFY`/`-evidence-verify` is set AND an artifact file exists; supply the eventlog-backed `EventSink`. Unset ⇒ bare `verify.New`.
- **Depends on:** P11-T04
- **Owns:** `cmd/nilcore/verifier.go`
- **Acceptance criteria:**
  - Env/flag unset ⇒ `behavioralVerifier` returns the exact verifier as today (test asserts unset path unchanged; default `verify` bytes unaffected).
  - Set + artifact present ⇒ the `Composite` has the build verifier `Named[0]` and evidence verifier appended after; a red claim makes the `Composite` red (fake sandbox + one-fail artifact).
  - Set + NO artifact present ⇒ evidence verifier omitted (green build still greens).
  - `artifact_verify`/`claim_verify` events appended via eventlog only when the flag is on (test gated).
  - **No secret leak:** a keyed check's key is injected via `box.ExecWithEnv` (resolved from `SecretStore`) and asserted absent from the written artifact JSON (`Evidence.SourceURL`) and the event Detail.
  - Change confined to `cmd/nilcore/verifier.go`.
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestEvidenceVerifierWiring`
- **Notes:** Mirrors `NILCORE_BROWSER_VERIFY`. File-scoped Owns; serialized with other cmd tasks.

### P11-T06 — Staging doc: spine

- **Goal:** This document (`docs/ROADMAP-EVIDENCE-ARTIFACTS.md`) — the spine + DAG + execution plan, in the house format.
- **Depends on:** — · **Owns:** `docs/ROADMAP-EVIDENCE-ARTIFACTS.md` · **Contract:** no
- **Acceptance criteria:** doc exists; states the additive+opt-in+flag-gated rule; pins Phase 11 / `P11-T##`; carries the full §2 spine + §4 DAG + §7 execution plan; notes that promotion (`P11-T36`) is a separate serialized contract task.
- **Verify:** `make verify` (doc-only) + manual review that every Owns is disjoint and every acceptance bullet is machine-checkable.
- **Notes:** Staging-doc-first house pattern.

### P11-T07 — web-research pack

- **Goal:** `internal/artifact/packs/web` leaf: `RegisterAll` registers `web.url_resolves`, `web.quote_exists`, `web.date_matches`, `web.not_stale`, each one `box.Exec` curl + trusted host-side parse.
- **Depends on:** P11-T03
- **Owns:** `internal/artifact/packs/web`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/artifact/packs/web | grep nilcore` returns only `artifact`, `evverify`, `sandbox`, `worktreefs`, and self.
  - `RegisterAll(r)` makes exactly those four ids `Lookup`-able (absent before, present after).
  - `web.url_resolves`: 2xx⇒`Pass`, non-2xx/unreachable⇒`Unverifiable` (fake sandbox exit 0 / exit 22).
  - `web.quote_exists`: `Pass` iff a whitespace-normalized substring of `Evidence.Value` is present, `Fail` iff absent (fake sandbox fixed body).
  - `web.not_stale`: freshness derived from a server header (Last-Modified/ETag/published-date) re-fetched in-box, NOT from `Evidence.RetrievedAt`; `RetrievedAt` is a hint only — a `now` timestamp over an unreachable source does not `Pass`.
  - nil `Box` ⇒ `Unverifiable` on every check; no host-side request (only `box.Exec`).
  - No check `guard.Wrap`s the body before parsing.
  - `git diff --quiet go.mod go.sum`; `go list -deps | grep -v '^nilcore'` pulls no non-sanctioned module.
- **Verify:** `make verify` + `go test ./internal/artifact/packs/web/ -run TestWebPack`
- **Notes:** Reuse `webfetch.go`'s URL guard + curl flags. Hermetic fake sandbox.

### P11-T08 — software-research pack

- **Goal:** `internal/artifact/packs/software`: `software.npm_version_exists`, `software.pypi_version_exists`, `software.crate_version_exists`, `software.github_release_exists`, `software.github_tag_exists`, `software.license_matches` — curl-in-box GET + `encoding/json`, NO SDK.
- **Depends on:** P11-T03
- **Owns:** `internal/artifact/packs/software`
- **Acceptance criteria:**
  - Imports only `artifact`/`evverify`/`sandbox`/`worktreefs`/self (`go list -deps`); `git diff --quiet go.mod go.sum`.
  - `RegisterAll` registers exactly the six ids.
  - `npm_version_exists`: curls `registry.npmjs.org/<pkg>`, `Pass` iff `Evidence.Value` is a key of `.versions`, `Fail` if absent, `Unverifiable` on non-2xx/parse error (fake JSON body covers all three).
  - `github_release_exists`: curls `api.github.com/repos/<o>/<r>/releases/tags/<tag>`, `Pass` on 2xx matching `tag_name`, `Fail` on 404.
  - nil `Box` ⇒ `Unverifiable` everywhere.
- **Verify:** `make verify` + `go test ./internal/artifact/packs/software/ -run TestSoftwarePack` + `git diff --quiet go.mod`
- **Notes:** owner/repo/pkg from typed claim params or parsed from `SourceURL`. JSON parsed host-side as trusted Go.

### P11-T09 — finance/market pack (keyed + keyless)

- **Goal:** `internal/artifact/packs/finance`: keyless `finance.sec_fact`, `finance.worldbank_indicator`, `finance.imf_series`; keyed `finance.fred_series`, `finance.market_quote` referencing `$NAME` via `box.ExecWithEnv`.
- **Depends on:** P11-T03
- **Owns:** `internal/artifact/packs/finance`
- **Acceptance criteria:**
  - Imports only the four leaves + self; `git diff --quiet go.mod go.sum` (no finance/SEC SDK).
  - `RegisterAll` registers exactly the five ids.
  - `sec_fact`: curls `data.sec.gov` companyfacts JSON, `Pass` iff the named fact equals `Evidence.Value` (**relative tolerance 1e-6 for floats, exact for ints** — documented constant; table test covers just-inside and just-outside the tolerance), `Fail` on mismatch, `Unverifiable` on non-2xx/parse error.
  - `fred_series` builds its curl referencing `$NILCORE_FRED_KEY` and calls `box.ExecWithEnv` with that env var; test asserts the **literal key value never appears in the command string** passed to `Exec` (only `$NILCORE_FRED_KEY` does) and the env map carries it.
  - **Keyed checks DERIVE the request URL from a key-free base URL + injected `$NAME` at run time** — they never trust a model-written full URL; the persisted `Evidence.SourceURL` is always the canonical key-free public URL. Test: after a keyed check runs, the on-disk artifact JSON and the emitted `claim_verify` event Detail contain neither the literal key nor an `api_key=`/token query param. (Auditor B2.)
  - A keyed check with no key supplied ⇒ `Unverifiable` (never `Pass`).
  - nil `Box` ⇒ `Unverifiable` everywhere; numeric tolerance constant documented.
- **Verify:** `make verify` + `go test ./internal/artifact/packs/finance/ -run TestFinancePack` + `git diff --quiet go.mod`
- **Notes:** Key NAME in the leaf; key VALUE injected by `P11-T12` wiring from `SecretStore`. Co-design hosts with the `finance` egress profile (`P11-T25`); `P11-T35` cross-checks the host superset.

### P11-T10 — ui-browser pack

- **Goal:** `internal/artifact/packs/ui`: `ui.flow_passes`, `ui.no_console_errors`, `ui.screenshot_captured` — drive the `nilcore-browser` CDP driver via `box.Exec`, reusing the `browserObservation` JSON contract.
- **Depends on:** P11-T03, P11-T11a
- **Owns:** `internal/artifact/packs/ui`
- **Acceptance criteria:**
  - Imports only `artifact`/`evverify`/`sandbox`/`worktreefs`/`browserwire`/self — NOT `internal/tools` and NOT `internal/model` (`go list -deps`).
  - `RegisterAll` registers exactly the three ids.
  - `no_console_errors`: parses the driver observation, `Fail` iff `console[]` non-empty, `Pass` iff empty, `Unverifiable` on driver non-zero exit / unparseable (fake sandbox fixed JSON covers all three).
  - `flow_passes`: `Pass` iff a normalized substring of `Evidence.Value` appears in the observation title/text after the flow; **an empty `Evidence.Value` ⇒ `Unverifiable`, never a vacuous `Pass`** (keeps it a real typed check, not generic browsing — NON-GOAL guard).
  - Model-supplied flow actions are quoted via `browserwire.ShellSingleQuote` (the shared, tested helper — not a hand-copy); a fuzz/table test with embedded single quotes, backslashes, `$()`, and newlines asserts the command stays exactly one driver invocation.
  - nil `Box` ⇒ `Unverifiable`; a non-zero driver exit fails closed (never a fabricated `Pass`).
- **Verify:** `make verify` + `go test ./internal/artifact/packs/ui/ -run TestUIPack` (fake sandbox; no Chromium in unit tests — live run is the `browser-e2e` CI job)
- **Notes:** `P11-T11a` provides the shared `ShellSingleQuote` + `browserObservation` so the I4 quoting boundary is one tested copy. (Auditor I4.)

### P11-T11 — pack aggregator + selector

- **Goal:** `internal/artifact/packs/packs.go`: `Select(names, r)` registers exactly the named packs (rejecting unknowns atomically); `HostsFor(name)` returns each pack's documented egress host-set.
- **Depends on:** P11-T07, P11-T08, P11-T09, P11-T10
- **Owns:** `internal/artifact/packs/packs.go`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/artifact/packs | grep nilcore` returns the four packs + `evverify`/`artifact`/`sandbox`/`worktreefs`/self (no orchestrator).
  - `Select([]string{"web","software"}, r)` calls only those two `RegisterAll`s; after, a web id and a software id `Lookup` and a finance id does not.
  - `Select` with an unknown name returns a non-nil error and registers NOTHING (atomic; registry unchanged on error).
  - `Select(nil/[]string{}, r)` is a no-op, no error (byte-identical default path).
  - `HostsFor("finance")` is non-empty and includes `data.sec.gov` and `api.stlouisfed.org`; `HostsFor(unknown)` ⇒ nil; table test all four + unknown.
  - Name parsing case-insensitive + space-trimmed (`" Web, Finance "` works).
  - `git diff --quiet go.mod go.sum`.
- **Verify:** `make verify` + `go test ./internal/artifact/packs/ -run TestPacksSelect`
- **Notes:** Owns the single file `packs.go` (subpackages owned by T07–T10), so it merges after the four packs.

### P11-T11a — Extract browserwire leaf

- **Goal:** Promote `shellSingleQuote` and the `browserObservation` struct out of `internal/tools/browser.go` into a stdlib-only leaf `internal/browserwire`, imported by both `internal/tools/browser.go` and `internal/artifact/packs/ui` — so the shell-quoting boundary (I4) is one tested copy, not a hand-copy.
- **Depends on:** —
- **Owns:** `internal/browserwire`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/browserwire | grep nilcore` returns only self (zero nilcore imports).
  - Exposes `ShellSingleQuote(string) string` and the `Observation` (formerly `browserObservation`) struct with the existing JSON tags.
  - A fuzz/table test covers single quotes, backslashes, `$()`, newlines — the quoted output decodes to the original under `sh -c`-equivalent rules and is a single argument.
  - `internal/tools/browser.go` refactored to import `browserwire`; `go test ./internal/tools/` regression passes (browser behavior byte-identical).
  - `git diff --quiet go.mod go.sum`.
- **Verify:** `make verify` + `go test ./internal/browserwire/ -run TestShellSingleQuote` + `go test ./internal/tools/`
- **Notes:** The `internal/tools` refactor must be behavior-preserving; coordinate with any concurrent `internal/tools` task.

### P11-T12 — Wire pack selection (NILCORE_VERIFY_PACKS)

- **Goal:** In `cmd/nilcore/verifier.go`, when evidence verification is on AND `NILCORE_VERIFY_PACKS`/`-verify-packs` is set, call `packs.Select` on the registry before building the `ArtifactVerifier`; resolve keyed packs' keys from `SecretStore` and route them to `box.ExecWithEnv`. Unset ⇒ registry equals `Default()`.
- **Depends on:** P11-T11, P11-T05
- **Owns:** `cmd/nilcore/verifier.go`
- **Acceptance criteria:**
  - Unset ⇒ registry equals `evverify.Default()` (test asserts the registry id-set), byte-identical to the `P11-T05` state.
  - `NILCORE_VERIFY_PACKS=web,software` ⇒ the registry resolves `web.*`/`software.*` (fake sandbox + an artifact naming `software.npm_version_exists` shows the claim is verified, not `Unverifiable`-by-missing-id).
  - An unknown pack name ⇒ startup error (fail-closed), not a silent skip.
  - A keyed finance pack's key resolved from `SecretStore` and injected via `box.ExecWithEnv`; test asserts the literal key appears in NEITHER the artifact JSON NOR the event Detail.
  - Change confined to `cmd/nilcore/verifier.go` (serialized after `P11-T05`).
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestVerifyPacksWiring`
- **Notes:** Shares `verifier.go` with `P11-T05` — strictly after it.

### P11-T13 — Staging doc: domain verifier packs

- **Goal:** Author `docs/ROADMAP-DOMAIN-PACKS.md` with the Pillar-2 specs in the house format, the verifier-id catalog per pack, each pack's egress host-set, and the I6/I3 rules.
- **Depends on:** P11-T06 · **Owns:** `docs/ROADMAP-DOMAIN-PACKS.md` · **Contract:** no
- **Acceptance criteria:** every DOM task in the `### <ID>` template with machine-checkable criteria + disjoint Owns; enumerates `web.*`/`software.*`/`finance.*`/`ui.*` ids; records `HostsFor` sets and cross-references Pillar 5; states the curl-in-box / no-SDK rule and the key-`$NAME` rule; notes that `P11-T12` shares `verifier.go` with `P11-T05` (serialized) and that promotion is `P11-T36`.
- **Verify:** `make verify` (doc-only) + manual review.
- **Notes:** Per-pillar staging docs (one Owns each) so the append tasks are genuinely disjoint (auditor: no four-co-owners-of-one-file).

### P11-T14 — spawn.Result typed-artifact field

- **Goal:** Add a nil-able `spawn.Result.Artifact *ArtifactSummary` plus flat `ArtifactSummary`/`ClaimStatus` (plain strings; no import of `internal/artifact`). Nil ⇒ today's behavior, byte-identical.
- **Depends on:** —
- **Owns:** `internal/spawn`
- **Acceptance criteria:**
  - `spawn.Result` gains exactly one new field `Artifact *ArtifactSummary` (`json:",omitempty"`, pointer); existing fields/order unchanged.
  - `ArtifactSummary`/`ClaimStatus` use only `string`/`bool`/slice fields; `go list -deps nilcore/internal/spawn | grep nilcore` shows no new nilcore import.
  - **Byte-identical proof:** a golden test asserts a `spawn.Result` with nil `Artifact` marshals with NO `artifact` key (omitempty), byte-identical to the pre-change shape.
  - A non-nil `ArtifactSummary` is preserved verbatim through `Spawner.Spawn` and a `DAGScheduler` wave (no drop/mutation), including a cancelled/panicking subtask path (stays nil).
  - `ClaimStatus` carries only `ID`/`Field`/`Status` — a doc comment forbids ever adding `Value`/`SourceURL` (trusted surface only).
- **Verify:** `make verify` + `go test ./internal/spawn/ -run TestResultArtifactField`
- **Notes:** `spawn.Result` is NOT a contract file. Flat strings keep `spawn` a leaf.

### P11-T15 — Typed-research Role/Profile in roster

- **Goal:** Add a typed-research `Role` const + `Profile` to `roster.NewDefault`: write-capable, `WantsWebFetch`, a research `Egress` (narrowed by `EgressFor`), and a System prompt instructing the worker to emit a spine `Artifact` JSON at `.nilcore/artifacts/<id>.json`.
- **Depends on:** —
- **Owns:** `internal/roster`
- **Acceptance criteria:**
  - A new role const + `Profile`; `rost.Resolve(role)` returns ok=true.
  - The profile is NOT `ReadOnly` and includes write+edit tools (`hasWriteTool` true).
  - The System prompt names the fixed path `.nilcore/artifacts/<id>.json` and the spine `Claim`/`Evidence` shape (substring test). The path string equals the spine's fixed `RelPath` (documented shared constant).
  - `EgressFor(profile, deny-all tree)` ⇒ empty allowlist (narrow-only preserved; `--network none` under deny-all).
  - Existing five roles unchanged (snapshot test).
- **Verify:** `make verify` + `go test ./internal/roster/ -run TestTypedResearchProfile`
- **Notes:** Dead config until `P11-T16` requests it; no `NewWorker` signature change (artifact via existing write tools).

### P11-T16 — buildSpawnFunc reads verified artifact; compose ArtifactVerifier into env.Verifier

- **Goal:** Two things in `cmd/nilcore/build.go`: (a) **compose** `evverify.ArtifactVerifier` into the **per-subagent** `env.Verifier` for the typed-research role when `NILCORE_EVIDENCE_VERIFY` is on (the app-level `behavioralVerifier` is a separate path); (b) after `env.Verifier.Check` passes, read `.nilcore/artifacts/<spec.ID>.json` via `artifact.Read` and project it into `spawn.Result.Artifact` + append a `typed_result` event. Fail-closed; `Passed` governed by the verifier verdict.
- **Depends on:** P11-T14, P11-T15, P11-T02, P11-T04, P11-T05
- **Owns:** `cmd/nilcore/build.go`
- **Acceptance criteria:**
  - With the EVA flag off OR `spec.Role` ≠ typed-research ⇒ `spawn.Result.Artifact == nil`, no `typed_result` event, and the `env.Verifier` is unchanged (byte-identical path test).
  - With the flag on + typed-research role, the `env.Verifier` composed for that subagent includes the `ArtifactVerifier` (test: a one-fail artifact ⇒ `env.Verifier.Check` `Passed=false` ⇒ `spawn.Result.Passed=false`). **This is the satisfiable form of the I2 guarantee** the typed path needs.
  - A GREEN artifact ⇒ `spawn.Result.Artifact` non-nil, `Green=true`, one `ClaimStatus` per claim mirroring the verifier-set status.
  - Missing / parse-broken / empty-claims artifact ⇒ `Artifact` nil (fail-closed), existing prose `workReport` summary unchanged.
  - **No secret in the projection:** only id/field/kind/status/bool copied (no `source_url`/`value`); test asserts the struct has no value/url fields populated.
  - `typed_result` event (Detail: id/kind/green/claim-count) appended only when `Artifact` non-nil and flag on.
  - Change confined to `cmd/nilcore/build.go`.
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestSpawnTypedArtifact`
- **Notes:** Resolves the auditor blocker that EVA-T05 (app-level) and the per-subagent `env.Verifier` were two paths that never met. File-scoped Owns; serialized with other cmd tasks; distinct file from `verifier.go`.

### P11-T17 — renderReport: typed claims trusted, prose fenced

- **Goal:** Extend `internal/super/dispatch.go renderReport` (shared by serial+concurrent) to render `r.Artifact` (when non-nil) as TRUSTED lines (`artifact <id> green=<bool>`, `claim <id> field=<field> status=<...>`) while the worker `Summary` stays `guard.Wrap`-fenced. Nil ⇒ byte-identical.
- **Depends on:** P11-T14
- **Owns:** `internal/super`
- **Acceptance criteria:**
  - `r.Artifact == nil` ⇒ byte-identical output (golden test asserts current rendering unchanged).
  - Non-nil ⇒ the trusted `artifact`/`claim` lines emit BEFORE the `guard.Wrap`'d prose block (golden test asserts ordering + exact format); claim/artifact lines are NOT `guard.Wrap`'d, prose IS.
  - Serial and concurrent `Result`s with identical `Artifact`s render byte-for-byte identically (the byte-identical-serial contract).
  - `mergeOrder`/`doIntegrate` unchanged — still gate on `Passed`; a non-nil-but-`green=false`, `Passed=false` `Result` is NOT in `mergeOrder`.
  - A `renderReport` test asserts only id/field/status surface as trusted (never a model-authored value), enforcing the trusted/untrusted split.
  - No new import beyond `strings`/`fmt`.
- **Verify:** `make verify` + `go test ./internal/super/ -run TestRenderReportTypedArtifact`
- **Notes:** I7 keystone for Pillar 3. `internal/super` is single-owner-at-a-time; rebase before starting.

### P11-T17a — Enrich subagent_report Detail with continue_from/base

- **Goal:** Additively include `continue_from` and `base` in the `subagent_report` event Detail at `internal/super/dispatch.go:169` (and `:385`) **when `spec.ContinueFrom != ""`**, gated so default logs stay byte-identical (the extra keys appear only on an actual retry). This gives Pillar 6 a secondary retry-history signal.
- **Depends on:** P11-T17
- **Owns:** `internal/super`
- **Acceptance criteria:**
  - When `spec.ContinueFrom == ""`, the `subagent_report` Detail is byte-identical to today (`{passed,branch,has_err}`) — golden test.
  - When `spec.ContinueFrom != ""`, Detail additively carries `continue_from` and `base`; test asserts presence only on retry.
  - `eventlog.Verify` still passes; no prior event mutated (log only grows).
  - `internal/super` import set unchanged.
- **Verify:** `make verify` + `go test ./internal/super/ -run TestSubagentReportContinueFrom`
- **Notes:** Resolves the auditor blocker that `subagent_report` carried no `continue_from`, so Pillar 6's retry projection had no source. Primary retry source is the GRA `claim_*` kinds; this is the secondary one. `internal/super` serialized after `P11-T17`.

### P11-T18 — Staging doc: typed worker results

- **Goal:** `docs/ROADMAP-TYPED-RESULTS.md` with the Pillar-3 specs.
- **Depends on:** P11-T13 · **Owns:** `docs/ROADMAP-TYPED-RESULTS.md` · **Contract:** no
- **Acceptance criteria:** TYP tasks in the template with disjoint Owns; states the additive rule (no `Artifact` ⇒ byte-identical), notes I1 untouched (no `backend.Result` change), records the per-subagent `env.Verifier` composition requirement and the cross-pillar deps (`P11-T02`/`P11-T04`/`P11-T05`); notes promotion is `P11-T36`.
- **Verify:** `make verify` (doc-only) + manual review.
- **Notes:** Own staging doc (disjoint Owns).

### P11-T19 — requeue leaf: Unit, Worklist, Scan

- **Goal:** `internal/requeue` leaf: `Unit`, `Worklist`, `Scan(root, *Ledger)` walking `.nilcore/artifacts/*.json` via `artifact.Read`, one `Unit` per non-`StatusPass` claim. Imports only `artifact` + stdlib.
- **Depends on:** P11-T02
- **Owns:** `internal/requeue`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/requeue | grep nilcore` returns only `internal/requeue` and `internal/artifact` (and transitively `worktreefs` via artifact — assert no `spawn`/`super`/`store`).
  - `Unit` fields ArtifactID, ClaimID, Field, Status, Detail, OwnerSubagent, Attempt with JSON tags.
  - `Scan` produces exactly one `Unit` per non-pass claim; a `{pass,fail,stale,unverifiable}` artifact ⇒ exactly 3 Units with correct ids/statuses; a pass claim ⇒ no Unit.
  - `Scan` stamps `Attempt` from the Ledger (0 when absent); missing artifacts dir ⇒ empty Worklist no error; corrupt JSON ⇒ error (not silent empty).
  - All-pass / zero-artifact ⇒ empty Worklist.
- **Verify:** `make verify` + `go test ./internal/requeue/ -run TestScan`
- **Notes:** Reuses `artifact.Read` (no new FS-safety code). Status semantics come from the spine.

### P11-T20 — requeue.Ledger

- **Goal:** Per-Unit attempt counter keyed `ArtifactID/ClaimID` with `MaxAttempts`, `Bump`/`Exhausted`, `Marshal`/`UnmarshalLedger`. `MaxAttempts==0` disables requeue.
- **Depends on:** P11-T19
- **Owns:** `internal/requeue`
- **Acceptance criteria:**
  - `key(u) == u.ArtifactID+"/"+u.ClaimID`; distinct claims ⇒ distinct counters.
  - `Bump` increments+returns; `Exhausted(u)` true iff `Attempts[key] >= MaxAttempts`; `MaxAttempts==0` ⇒ `Exhausted` true for every unit at attempt 0 (disabled-by-default).
  - `Marshal`→`UnmarshalLedger` round-trips; an empty/absent blob ⇒ zero Ledger (MaxAttempts 0) no error (an old snapshot resumes disabled).
  - Boundary table test for MaxAttempts 1/2/3.
  - Imports only `artifact` + stdlib.
- **Verify:** `make verify` + `go test ./internal/requeue/ -run TestLedger`
- **Notes:** Bounds retries (no spinning on a permanently-red unit). Embeds as a sibling JSON field beside `agent.RunState` in `store.Task.Detail`.

### P11-T21 — requeue.Plan + Resolve

- **Goal:** `Plan` (minimal focused subtasks, one per artifact/owner, Goal names only red claim ids, `ContinueFrom`=prior attempt) and `Resolve` (classify resolved/stillFailed/exhausted, decide loop continuation). Leaf-typed (no `spawn` import).
- **Depends on:** P11-T19, P11-T20
- **Owns:** `internal/requeue`
- **Acceptance criteria:**
  - `requeue.Subtask` is a leaf struct (`go list -deps` asserts no `internal/spawn` import).
  - `Plan` groups Units by ArtifactID(+OwnerSubagent) into the MINIMAL set: N artifacts with red claims ⇒ N Subtasks; each Goal contains every failed ClaimID for that artifact and `UnitKeys` lists them; exhausted Units excluded.
  - `Plan` ⇒ zero Subtasks when the worklist is empty or all exhausted.
  - `Resolve(before, after, ledger)`: resolved = in `before` absent from `after`; stillFailed = in both (Bump'd); exhausted = stillFailed hitting `MaxAttempts`; table test covers green-flip, stay-red, ceiling.
  - Loop-continue iff `len(stillFailed) > len(exhausted)` (test continue vs stop).
  - `ContinueFrom` set to the prior attempt id supplied to `Plan`.
- **Verify:** `make verify` + `go test ./internal/requeue/ -run TestPlanResolve`
- **Notes:** Emits LEAF descriptors; cmd wiring translates to `spawn.Subtask`+`SubagentSpec`. Green from the verifier's after-worklist (I2).

### P11-T22 — super: nil-gated RequeueHook

- **Goal:** Add `Supervisor.RequeueHook func(ctx context.Context) (remaining []string, exhausted bool)` (the exact signature, pinned) consulted only at convergence-red; nil ⇒ byte-identical. Hook supplied by cmd wiring (super stays a leaf).
- **Depends on:** P11-T17a
- **Owns:** `internal/super`
- **Acceptance criteria:**
  - Nil hook ⇒ run loop byte-identical (test asserts same Outcome/round count as baseline).
  - Set + project verifier red at convergence ⇒ hook consulted exactly once per convergence-red; if it reports remaining non-exhausted units, one additional focused round runs; not consulted on green (fake hook test).
  - Hook reports exhausted ⇒ converge red, no extra round (no infinite consult).
  - `internal/super` gains NO new import of `requeue`/`artifact`/`store` (`go list -deps` unchanged except stdlib/context).
  - Doc comment: returned ids are trusted control data; any prose to the model still passes `guard.Wrap`.
- **Verify:** `make verify` + `go test ./internal/super/ -run TestRequeueHook`
- **Notes:** Mirrors the `SaveState`/`EventSink` nil-able-func pattern. Pinned signature so `P11-T23` supplies a matching func. `internal/super` serialized after `P11-T17a`.

### P11-T23 — Wire granular requeue (NILCORE_REQUEUE)

- **Goal:** In `cmd/nilcore/requeue_wiring.go`, behind `NILCORE_REQUEUE`/`-requeue` (+`-requeue-max-attempts N`, default 0): after the `ArtifactVerifier` pass, `Scan`→`Plan`→drive focused subtasks through the existing `spawn.DAGScheduler` (`ContinueFrom` base cut)→re-run the `ArtifactVerifier`→`Resolve`→persist the Ledger in `store.Task.Detail`→emit `claim_requeue`/`claim_resolved`/`requeue_exhausted`. Bounded by `MaxAttempts`.
- **Depends on:** P11-T21, P11-T22, P11-T05, P11-T16
- **Owns:** `cmd/nilcore/requeue_wiring.go`
- **Acceptance criteria:**
  - Unset ⇒ no requeue code runs, no `claim_*` events, no extra store write, Outcome byte-identical (test).
  - `-requeue-max-attempts N` enforced: an always-red claim ⇒ exactly N rounds then `requeue_exhausted` then stop (converge red, no further round).
  - A claim failing once then passing on re-verify ⇒ `claim_requeue` then `claim_resolved`, and the focused Subtask Goal named only the red claim id (passing claims untouched).
  - Each wave dispatched through the existing `spawn.DAGScheduler` (no new scheduler) with `ContinueFrom`; re-verify uses the same `evverify.ArtifactVerifier`; test asserts green is set only after a verifier re-run, never from a stored status.
  - **Append-only proof:** after N rounds the event log byte length only GROWS, `eventlog.Verify` still passes, and `count(claim_requeue)+count(claim_resolved)` equals the rounds (no in-place edits). (Auditor I5.)
  - The Ledger marshals into `store.Task.Detail` as an additive sibling of `agent.RunState` (existing `run_state` unchanged); a resume test asserts attempt counts survive a reload and an old (no-requeue) blob loads as a zero Ledger.
  - No secret in any Unit/Goal/Ledger/`claim_*` Detail (keyed re-verify key absent).
  - Supplies a `RequeueHook` of the exact `P11-T22` signature.
  - Change confined to the single new file.
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestRequeueWiring`
- **Notes:** Reuses `spawn.DAGScheduler` + `ContinueFrom` + `preserveFailedAttempt` verbatim — invents no loop. File-scoped Owns; serialized with other cmd tasks.

### P11-T24 — Staging doc: granular requeue

- **Goal:** `docs/ROADMAP-GRANULAR-REQUEUE.md` with the Pillar-4 specs.
- **Depends on:** — · **Owns:** `docs/ROADMAP-GRANULAR-REQUEUE.md` · **Contract:** no
- **Acceptance criteria:** GRA tasks in the template with disjoint Owns; states the additive+opt-in rule (`NILCORE_REQUEUE` default off ⇒ byte-identical); records the EVA spine deps and that requeue REUSES `DAGScheduler`+`ContinueFrom`+`preserveFailedAttempt`; documents bounded-retry termination and status-routed fixes; cross-references how Pillar 6 reads the `claim_*` events; notes promotion is `P11-T36`.
- **Verify:** `make verify` (doc-only) + manual review.
- **Notes:** Own staging doc (no EVA-doc dependency).

### P11-T25 — egressprofile leaf: named presets + Resolve

- **Goal:** `internal/egressprofile`: `Named(name)→(policy.Egress,bool)`, `Names()`, `Resolve(profileName, filePath)→(tree, sources, err)`. Three presets `finance`|`docs`|`web-research`, host sets co-designed with Pillar 2.
- **Depends on:** —
- **Owns:** `internal/egressprofile`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/egressprofile | grep nilcore` returns only `internal/egressprofile` and `internal/policy`.
  - `Named("finance"/"docs"/"web-research")` each non-empty; `Named("bogus")` ⇒ `(zero,false)`.
  - Finance preset contains `data.sec.gov`/`api.stlouisfed.org`/World Bank/IMF/market hosts; preset hosts are **literal** (or documented as requiring matching role-side wildcards) so they survive `roster.intersectEgress`; a test asserts each preset host passes `policy.Egress.Allow` for itself.
  - `Resolve("finance","")` ⇒ finance hosts + sources `["profile:finance"]`; `Resolve("",path)` ⇒ file hosts + `["file:<path>"]`; `Resolve("finance",path)` ⇒ deduped union + both sources; `Resolve("","")` ⇒ empty `policy.Egress` + empty sources.
  - `Names()` returns the closed set `{finance,docs,web-research}`.
  - `git diff --quiet go.mod go.sum`.
- **Verify:** `make verify` + `go test ./internal/egressprofile/ -run TestPresets -run TestResolve`
- **Notes:** Keep presets literal-host to survive the conservative wildcard intersection. `P11-T35` cross-checks the host superset against the packs.

### P11-T26 — egressprofile project-local allowlist file

- **Goal:** `FileSpec{schema_version, allow[]}` + `LoadFile(path)→policy.Egress` at the fixed `.nilcore/egress.json`, typed not-found distinct from parse error, default path constant.
- **Depends on:** P11-T25
- **Owns:** `internal/egressprofile`
- **Acceptance criteria:**
  - `DefaultFilePath == ".nilcore/egress.json"`; `LoadFile` reads+unmarshals `FileSpec` into a `policy.Egress` whose `Allowed == allow` (order preserved, trimmed); golden-file test asserts the JSON incl. `schema_version`.
  - Missing file ⇒ typed not-found (`errors.Is`-distinguishable) ≠ parse error; malformed JSON ⇒ parse error (never silent zero-value).
  - A literal host and a `*.suffix` wildcard both feed `policy.Egress.Allow` correctly (no reachability validation — the proxy enforces).
  - `Resolve` consumes `LoadFile` output (file composes with a preset); imports only `internal/policy` + stdlib (T25 `go list -deps` still holds).
  - Doc comment: meant to be committed; hostnames only, never secrets (I3).
- **Verify:** `make verify` + `go test ./internal/egressprofile/ -run TestLoadFile`
- **Notes:** Plain `os.ReadFile` + `encoding/json`. Keyed sources still resolve via `SecretStore` at the wiring layer.

### P11-T27 — onboard.WebConfig persistence + validation

- **Goal:** Add `Profile string` + `ProfileFile string` to `onboard.WebConfig` (`omitempty`), validate `Profile` against a closed set sourced from `egressprofile.Names()`, map `NILCORE_EGRESS_PROFILE` in `wizard.go` alongside `NILCORE_WEB_ALLOW`.
- **Depends on:** P11-T25
- **Owns:** `internal/onboard`
- **Acceptance criteria:**
  - `WebConfig` gains the two `omitempty` fields; round-trip marshal/unmarshal persists both.
  - **Byte-identical proof:** a golden test asserts `WebConfig{}` (no profile) marshals with NO `profile`/`profile_file` keys (omitempty) — existing `config.json` unaffected.
  - `Config.Validate` rejects an unknown `Web.Profile` with an actionable error; empty allowed; test valid/empty/bogus.
  - `wizard.go` maps `NILCORE_EGRESS_PROFILE` into `Web.Profile` (unset ⇒ empty).
  - The valid set is sourced from / consistent with `egressprofile.Names()` (every `Names()` entry validates; a non-member fails).
  - Doc comment: keyed sources resolve via `SecretStore` at the wiring layer, never here (I3).
- **Verify:** `make verify` + `go test ./internal/onboard/ -run TestWebConfigProfile`
- **Notes:** Additive fields default-zero ⇒ existing config stays valid, default binary unaffected.

### P11-T28 — Wire -egress-profile through both front doors

- **Goal:** Register `-egress-profile` (chat AND serve/build front doors) + `NILCORE_EGRESS_PROFILE`; expand `egressprofile.Resolve` hosts into `resolveWeb` before the search-host auto-add; flip `build.go:687`'s deny-all tree to the resolved profile tree ONLY when a profile is opted in; emit a metadata-only `egress_profile` event. Default byte-identical.
- **Depends on:** P11-T26, P11-T27, P11-T05, P11-T12, P11-T16, P11-T23, P11-T33
- **Owns:** `cmd/nilcore` (egress wiring across `chat.go`, `main.go`, the egress lines of `build.go`)
- **Acceptance criteria:**
  - Unset (flag/env/config) ⇒ `resolveWeb` returns the same allowlist as today (nil when web off); no `egress_profile` event; `build.go:687` still passes `policy.Egress{}` (deny-all). Byte-identical path test.
  - `-egress-profile finance` (or env) ⇒ `resolveWeb`'s allowlist contains the finance preset hosts (added before the search host); composes with `-allow-egress` (profile = base, flag = extra) — table test of the merged set.
  - **Resolved tree equals EXACTLY the union of named-preset hosts + project-local file hosts and nothing else** (golden test). (Auditor I4/R9.)
  - Unknown profile name (flag or env) ⇒ loud actionable error referencing `egressprofile.Names()`; a project-local file that fails to parse ⇒ fail-closed to deny-all (never fail open).
  - **Explicit `build.go:687` toggle:** when a profile is opted in for build/serve, `build.go:687` passes the resolved profile tree (not `policy.Egress{}`) into `roster.EgressFor`; a researcher role under the finance tree yields the intersection while a deny-all role stays `--network none` (R9). Assert deny-all preserved when unset.
  - **Namespace-backend degrade:** on a non-`*Container` backend the profile surfaces a loud diagnostic (a specific sentinel: an `egress_profile` event field `backend=namespace` + a stderr warning) and the box stays deny-all (`applyContainerEgress` no-ops); test with a fake namespace box.
  - Both front doors honor the flag and env (test each entrypoint's `resolveWeb` path).
  - The `egress_profile` event carries `{profile,file,host_count,sources,backend}` — NO hostnames-with-query-strings, NO keys (I3 redaction path).
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestEgressProfileWiring`
- **Notes:** Owns the cmd egress files; serialized LAST among cmd tasks (after `T05`/`T12`/`T16`/`T23`/`T33` merge) because the toggle widens every role at once — the single audited switch. (Auditors: build.go:687 overlap with `T16`; `T16` edits `buildSpawnFunc` ~710-755, not line 687 — verified disjoint regions, still serialized.)

### P11-T29 — Staging doc: research egress profiles

- **Goal:** `docs/ROADMAP-EGRESS-PROFILES.md` with the Pillar-5 specs.
- **Depends on:** P11-T18 · **Owns:** `docs/ROADMAP-EGRESS-PROFILES.md` · **Contract:** no
- **Acceptance criteria:** RES tasks in the template with disjoint Owns; states the security direction precisely (profile widens the TREE; `EgressFor` clamps narrow-only per role; a deny-all role stays `--network none`); specifies the `.nilcore/egress.json` `schema_version`+`allow[]` format (committed, hostnames only, no secrets); notes the namespace backend has no proxy path and `build.go:687` is the single audited toggle; cross-references Pillar 2 (co-designed host sets) and Pillar 6 (`egress_profile` event feeds the report); notes promotion is `P11-T36`.
- **Verify:** `make verify` (doc-only) + manual review.
- **Notes:** Own staging doc.

### P11-T30 — report leaf: ReportModel + log-replay projection

- **Goal:** `internal/report` leaf: `ReportModel` (`CheckResult`, `ArtifactView`, `ClaimRow`, `RetryAttempt`) and `ReplayReport(logPath, worktreeRoot)` scanning the log once (own decode of Time/Seq/Task/Kind/Backend/Detail), calling `eventlog.Verify`, folding artifacts via `artifact.Read`. Pure read, fail-closed on a broken chain. Imports only `eventlog` + `artifact` + `worktreefs` + stdlib.
- **Depends on:** P11-T01, P11-T02
- **Owns:** `internal/report`
- **Acceptance criteria:**
  - `go list -deps nilcore/internal/report | grep nilcore` returns only `eventlog`, `artifact`, `worktreefs`, self (NOT `inspect`/`termui`/`emit`/`super`/`backend`).
  - Reads the families `verify`/`final_verify`/`project_verify`/`project_acceptance`/`integration_verify`/`integration_rollback`/`integration_conflict`/`artifact_verify` into `CheckResult` with correct `Family`/`Passed` (synthetic JSONL log table test).
  - **Retry history from the GRA `claim_requeue`/`claim_resolved`/`requeue_exhausted` kinds** (which carry `attempt`+`claim_id`) as primary, with `subagent_report.continue_from` (when enriched, `P11-T17a`) as secondary; ordered by Seq. **A log lacking the `claim_*` events still produces a valid `ReportModel`** (graceful degradation test). (Auditor blocker.)
  - For each id in an `artifact_verify` event, `artifact.Read(worktreeRoot, id)` ⇒ an `ArtifactView` with 1:1 `ClaimRow`s; `ArtifactView.Green == artifact.Green()` (seeded-artifact test).
  - `ReplayReport` calls `eventlog.Verify`; a broken chain ⇒ `ChainVerified=false` and `FinalPass=false` (not an error that hides the model); a clean chain ⇒ `ChainVerified=true`.
  - `FinalPass` true only when `ChainVerified` AND every relevant check passed (green-over-broken-chain ⇒ `FinalPass=false`).
  - **No `CostLine`** — explicitly out of scope (no usage events in the log today).
- **Verify:** `make verify` + `go test ./internal/report/ -run TestReplayReport`
- **Notes:** Do NOT widen/import `inspect.Summary` (its event struct is depended on by `inspect_test.go` + the health probe). Own decode struct. Degrades gracefully when requeue/EVA events are absent (so it sits in wave 4, not gated behind requeue waves).

### P11-T31 — report worktree writer

- **Goal:** `WriteReport(root, run, ext, content)` writing `<root>/.nilcore/reports/<run>.<ext>` via `worktreefs` (symlink-safe, `O_NOFOLLOW`, atomic), rejecting path-escape, `ext` ∈ `{html,md,txt}`. Content-agnostic.
- **Depends on:** P11-T30
- **Owns:** `internal/report`
- **Acceptance criteria:**
  - Writes to `<root>/.nilcore/reports/<run>.<ext>` via `worktreefs.WriteAtomic`; read-back byte-equal to the passed content.
  - `run`/`ext` with a separator or `..` ⇒ error, no escape file (test).
  - Symlink at the target ⇒ refused via `worktreefs.OpenNoFollow` (test).
  - `ext` restricted to `{html,md,txt}`; unknown ⇒ error.
  - Imports only `eventlog`/`artifact`/`worktreefs`/stdlib (T30 `go list -deps` holds).
- **Verify:** `make verify` + `go test ./internal/report/ -run TestWriteReport`
- **Notes:** Reuses `worktreefs` (no hand-rolled path safety). Content-agnostic so the renderers stay pure.

### P11-T32 — report/render: text + HTML + markdown

- **Goal:** `internal/report/render`: pure `RenderText(*ReportModel, termui.Style)`, `RenderHTML(*ReportModel)`, `RenderMarkdown(*ReportModel)`. Render passed/failed checks, the per-claim `{value,source_url,verifier,status}` table, verifier output tails, retry history, final pass. Style-gated text; self-contained script-free escaped HTML; stdlib-only markup.
- **Depends on:** P11-T30
- **Owns:** `internal/report/render`
- **Acceptance criteria:**
  - Imports only `report`, `termui`, stdlib (`go list -deps`; NOT `eventlog`/`artifact` directly, NOT orchestrator).
  - `RenderText(m, off-Style)` contains NO ANSI escapes (CI/pipe degrade); `on-Style` wraps failed rows Danger / passed rows Success (test both).
  - All three render a RED "chain broken — report not trustworthy" banner and DO NOT print a GREEN final-pass headline when `ChainVerified==false` (test).
  - **`RenderMarkdown`'s GREEN headline present ONLY when `ChainVerified && all-statuses-pass`** — proving the markdown is a verifier projection, not a citations-emitter (NON-GOAL guard).
  - Every failed `ClaimRow` rendered with ClaimID, Field, Value, SourceURL, Verifier, Status (test in all three formats).
  - `RenderHTML` has no `<script>` and no external asset URL (inline CSS only); untrusted `Value`/`SourceURL` `html.EscapeString`-escaped (a `<script>` payload is escaped).
  - **Secret redaction:** `SourceURL` and verifier `Output`/`Detail` run through the eventlog redact path (or a shared redact); a secret-looking token seeded in a `Detail` tail or `SourceURL` is redacted in all three formats (test). (Auditor I3.)
  - Retry history renders each `RetryAttempt` as an ordered chain in all three formats.
- **Verify:** `make verify` + `go test ./internal/report/render/ -run TestRender`
- **Notes:** Pure functions, unit-testable without stdout capture. Hand-roll HTML/markdown (no template/markdown module). `Style` is the only `termui` dependency.

### P11-T33 — Wire `nilcore report` subcommand

- **Goal:** `cmd/nilcore/report.go`: a `report` subcommand calling `report.ReplayReport`→`render.RenderText` (Style detected from stdout, plain on non-TTY); with `-report-out` also write `.nilcore/reports/` via `report.WriteReport`. Default `inspect` byte-identical. Add the single `case "report"` to the `switch args[0]` dispatch in `cmd/nilcore/main.go:82`.
- **Depends on:** P11-T31, P11-T32
- **Owns:** `cmd/nilcore/report.go` (+ the single `case "report"` dispatch line in `main.go`)
- **Acceptance criteria:**
  - `nilcore report [-log path] [-root worktree] [-report-out path] [-format text|html|md] [<run>]` dispatched from `main`; all existing subcommands byte-identical (test asserts `nilcore inspect` output unchanged).
  - Clean log ⇒ prints the text report, exit 0; broken-chain log ⇒ prints the RED banner, exit non-zero (fail-closed) — test both exit codes + banner.
  - `-report-out` + `-format html` ⇒ a self-contained `.html` under `.nilcore/reports/` byte-equal to `render.RenderHTML(model)`, no `<script>`.
  - Styling detected from the actual stdout writer (a non-TTY buffer ⇒ no ANSI escapes).
  - Reads only the log + worktree artifacts; test asserts the event log byte length is unchanged after running `nilcore report`.
  - Owns `cmd/nilcore/report.go` + the one dispatch line; the dispatch insertion point is the `switch args[0]` block (`main.go:82`).
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestReportSubcommand`
- **Notes:** Mirrors the `inspect`/`health` dispatch. The `main.go` dispatch line is routed through this task (the sole `main.go` dispatch edit); file-scoped Owns; serialized with other cmd tasks. (Auditor: T33 self-containment — the dispatch edit is explicit.)

### P11-T34 — Staging doc: verification report

- **Goal:** `docs/ROADMAP-VERIFICATION-REPORT.md` with the Pillar-6 specs.
- **Depends on:** — · **Owns:** `docs/ROADMAP-VERIFICATION-REPORT.md` · **Contract:** no
- **Acceptance criteria:** VER tasks in the template with disjoint Owns; states the additive+opt-in rule; records the import-direction rule (`internal/report` imports only `eventlog`+`artifact`+`worktreefs`; `render` imports `report`+`termui`; report NEVER mutates the log and refuses GREEN over a broken chain); enumerates exactly the event Kinds + Detail fields read and the artifact fields rendered; states CostLine is out of scope (no usage events); notes promotion is `P11-T36`.
- **Verify:** `make verify` (doc-only) + manual review.
- **Notes:** Own staging doc.

### P11-T35 — Cross-check: pack hosts ⊆ egress profiles

- **Goal:** A consistency test asserting for each domain `d ∈ {finance,docs,web-research}`: every host in `packs.HostsFor(d)` satisfies `egressprofile.Named(d).Allow(host)`. The one machine-checkable proof that "Pillar 5 unlocks Pillar 2".
- **Depends on:** P11-T07, P11-T08, P11-T09, P11-T10, P11-T11, P11-T25
- **Owns:** `cmd/nilcore` (a single new test file `egress_packs_test.go`)
- **Acceptance criteria:**
  - For each domain, `packs.HostsFor(d)` is non-empty and every host passes `egressprofile.Named(d).Allow(host)` — fails if the two lists drift.
  - The test lives in the cmd layer (importing both `packs` and `egressprofile`) so the two leaves stay decoupled.
  - `make verify` green.
- **Verify:** `make verify` + `go test ./cmd/nilcore/ -run TestEgressPackHostConsistency`
- **Notes:** Resolves the auditor finding that pillar-5-unlocks-pillar-2 was a doc promise with no test. Owns a single test file; serialized with other cmd tasks (after `T28`).

### P11-T36 — PROMOTION (serialized contract task)

- **Goal:** Promote all Phase 11 staging specs into the canonical `docs/TASKS.md` master DAG + a new `## Phase 11` section; register the new leaf packages as extension points in `docs/ARCHITECTURE.md`; append the `CHANGELOG.md` entry.
- **Depends on:** P11-T06, P11-T13, P11-T18, P11-T29, P11-T24, P11-T34
- **Owns:** `docs/TASKS.md`, `docs/ARCHITECTURE.md`, `CHANGELOG.md` · **Contract:** **YES**
- **Acceptance criteria:**
  - `grep -c 'P11-T' docs/TASKS.md` ≥ 37 (every task has a master-DAG row in the exact 6-column shape `| ID | Phase | Title | Depends on | Owns | Note |`).
  - A new `## Phase 11 — NilCore as a verifier-backed artifact factory` section contains every task's full spec.
  - Each new leaf package path (`internal/worktreefs`, `internal/browserwire`, `internal/artifact`, `internal/artifact/evverify`, `internal/artifact/packs/*`, `internal/requeue`, `internal/egressprofile`, `internal/report`, `internal/report/render`) appears in `docs/ARCHITECTURE.md` (grep per path).
  - The "later phases" note in `docs/TASKS.md` is updated to mention Phase 11.
  - One `CHANGELOG.md` `## [Unreleased]` entry per merged task (or one umbrella Phase-11 entry per the maintainer's call).
  - No stray `=======` conflict marker; no reference to the retired `internal/config`.
  - `make verify` green.
- **Verify:** `make verify` + a doc-lint shell test asserting the greps above.
- **Notes:** The ONLY contract task in Phase 11 — runs SOLO, last (`CLAUDE.md` §5). Touches `docs/TASKS.md` + `docs/ARCHITECTURE.md` + `CHANGELOG.md`. Every other Phase-11 task is additive and routes around the frozen `backend.go`/`channel.go`/`go.mod`/`Makefile`/`CLAUDE.md`.

---

## 6. Execution plan (parallel subworkers)

**Protocol (`CLAUDE.md` §5).** One task = one branch (`task/P11-T##`) = one worktree. Before starting, select a task whose dependencies are all merged and whose `Owns` is disjoint from every open `task/*` branch (a package dir is the ownership unit; the cmd wiring files are the documented file-granular exception, claimed one-at-a-time). `make verify` green is part of the Definition of Done. Merge is the gate.

### Waves (tasks within a wave have disjoint Owns and may run concurrently)

| Wave | Task ids | Why disjoint |
|---|---|---|
| **1** | T00, T06, T11a, T14, T15, T24, T25, T34 | `internal/worktreefs`, the spine staging doc, `internal/browserwire`, `internal/spawn`, `internal/roster`, the GRA staging doc, `internal/egressprofile`, the VER staging doc — eight distinct paths/dirs, zero shared files. |
| **2** | T01, T13, T17, T26, T27 | `internal/artifact` (needs T00), the DOM staging doc (its own file), `internal/super` (renderReport; needs T14), `internal/egressprofile` (file; needs T25), `internal/onboard` (needs T25) — all disjoint dirs/files. |
| **3** | T02, T03, T17a, T18, T19 | `internal/artifact` (T02) and `internal/artifact/evverify` (T03) are **separate dirs** (Go package boundary) so they parallelize; `internal/super` (T17a, after T17 merged); the TYP staging doc; `internal/requeue` (needs T02). |
| **4** | T04, T07, T08, T09, T10, T22, T29, T30 | `evverify` (T04); the four pack dirs `packs/{web,software,finance,ui}` (each its own leaf, need only T03); `internal/super` (T22, after T17a); the RES staging doc; `internal/report` (needs T01/T02). All unique paths. |
| **5** | T05, T11, T20, T31, T32 | `cmd/nilcore/verifier.go` (T05); `packs/packs.go` (T11, after the four packs); `internal/requeue` (T20); `internal/report` (T31) and `internal/report/render` (T32) — **distinct dirs**. cmd file T05 is the only cmd task this wave. |
| **6** | T12, T16, T21 | `cmd/nilcore/verifier.go` (T12, after T05 — same file, different wave) and `cmd/nilcore/build.go` (T16) are **distinct files**, serialized one-cmd-at-a-time so only one runs at a moment; `internal/requeue` (T21). Per the cmd file-granularity rule the integrator runs T12 then T16 (or vice versa) serially. |
| **7** | T23, T33 | `cmd/nilcore/requeue_wiring.go` (T23) and `cmd/nilcore/report.go` + the one `main.go` dispatch line (T33) — distinct files, serialized one-cmd-at-a-time. |
| **8** | T28, T35 | `cmd/nilcore` egress wiring (T28, dir-wide) then the egress-pack consistency test (T35) — both dir-touching, serialized after every file-scoped cmd task has merged. |
| **9** | T36 | The serialized contract promotion — solo. |

> The cmd tasks (T05, T12, T16, T23, T33, T28, T35) appear across waves 5–8 but are **fully serialized** as a chain — only one open `cmd/nilcore` branch at a time, in dependency order. The waves show their earliest eligible slot; the integrator opens them one after another. Non-cmd tasks in those waves run concurrently with whichever single cmd task is open.

### Serialization points

- **`P11-T36` is the only contract task** — it edits `docs/TASKS.md` + `docs/ARCHITECTURE.md` + `CHANGELOG.md`, runs solo in wave 9 after all six staging docs are final. No other Phase-11 task touches a contract file (`backend.go`, `channel.go`, `CLAUDE.md`, `go.mod`, `Makefile` are all untouched — the upgrade is additive and routes around the frozen backend contract via worktree artifact files + `spawn.Result` fields).
- **`cmd/nilcore` is claimed at file granularity and fully serialized.** `verifier.go` is touched by T05 then T12; `build.go` by T16 (and the egress lines by T28); `requeue_wiring.go` by T23; `report.go`+the `main.go` dispatch line by T33; the egress front-doors + consistency test by T28/T35. Only one open `cmd/nilcore` branch at a time; the integrator rejects any attempt to widen a file-scoped cmd task to the dir while another is open.
- **`internal/super`** is touched by T17 (wave 2) → T17a (wave 3) → T22 (wave 4), strictly serialized by dependency; single-owner-at-a-time.
- **`internal/artifact`** T01→T02; **`internal/artifact/evverify`** T03→T04; **`internal/requeue`** T19→T20→T21; **`internal/report`** T30→T31; **`internal/egressprofile`** T25→T26 — each serialized by dependency.
- **Six separate staging docs** (one Owns each: EVA/DOM/TYP/GRA/RES/VER) — no shared-file co-ownership, so the doc tasks are genuinely disjoint (auditor fix: no four co-owners of one file).

### Critical path

`T00 → T01 → T02 → T04 → T05 → T12 → T16 → T23 → T28 → T35 → T36`

(spine confinement → data model → persistence → verifier keystone → evidence wiring → pack wiring → typed-result wiring → requeue wiring → egress wiring → consistency test → promotion). Length 11. The cmd serialization is the dominant tail; the integrator may compress it by negotiating T28 down to strict file-scope (chat.go + main.go + the egress lines of build.go) so it need not wait on unrelated cmd wiring, but as specified it owns the egress front-doors and runs last.

---

## 7. Invariant compliance (I1–I7)

- **I1 — Frozen backend contract.** Zero edits to `internal/backend/backend.go`. Typed artifacts ride **out-of-band** as worktree JSON (`.nilcore/artifacts/<id>.json`); the typed result is a field on `spawn.Result` (not a contract file), populated host-side. `backend.Result.Summary` stays bounded prose. The only contract task is `P11-T36` (promotion), which edits `docs/TASKS.md`/`docs/ARCHITECTURE.md`/`CHANGELOG.md` — never `backend.go`.
- **I2 — Verifier is the sole authority.** Artifact-green is **produced by** `evverify.ArtifactVerifier` (a `verify.Verifier`) composed into `verify.Composite` after the build verifier; any red claim turns the whole verdict red. A worker's self-written `Status=pass` is overwritten by a real `CheckFunc` run; an unregistered id ⇒ `Unverifiable`; staleness can only DEMOTE, never PASS on a model timestamp. Typed results merge only on `Passed`; granular requeue flips green only on a fresh re-verify; the report renders the verifier's verdict, never SelfClaimed.
- **I3 — No ambient authority / secrets.** No secret field anywhere. Keyed packs reference `$NAME` via `box.ExecWithEnv` from `SecretStore`; the persisted `Evidence.SourceURL` is always the key-free public URL (run-time-derived), asserted absent from the artifact JSON, the `claim_verify`/`egress_profile` events, and the rendered report (which also runs SourceURL/Detail through the redact path). The egress allowlist and `.nilcore/egress.json` hold hostnames only.
- **I4 — Model-emitted execution sandboxed.** Every `CheckFunc` and pack reach runs via `box.Exec`/`ExecWithEnv` under the role's egress allowlist (nil box / denied host ⇒ `Unverifiable`, fail-closed). All worktree FS I/O (artifact store, report writer, verifier write-back) routes through the single audited `internal/worktreefs` leaf (symlink-safe + `O_NOFOLLOW` + atomic rename) — no three hand-rolled copies. The UI pack's shell-quoting uses the shared, fuzz-tested `internal/browserwire.ShellSingleQuote`. The namespace backend stays hard deny-all; profiles require the `*Container` backend (surfaced loudly).
- **I5 — Append-only audit.** All verification outcomes are **new additive event kinds** (`artifact_verify`, `claim_verify`, `typed_result`, `claim_requeue`, `claim_resolved`, `requeue_exhausted`, `egress_profile`) appended via `Log.Append`; nothing mutates or reorders history. Requeue re-runs append fresh verdicts (the byte-length-only-grows test). The verification report is a **pure read** projection that calls `eventlog.Verify` and refuses GREEN over a broken chain; the mutable Ledger lives in `store.Task.Detail` (working state), the immutable record is the event chain.
- **I6 — Zero-dependency core.** Every new package is stdlib-only or imports only sanctioned leaves; packs reach live data via curl-in-box + `encoding/json` (no go-github / finance / SEC SDK). Each pack/leaf task carries a `git diff --quiet go.mod go.sum` assertion. `CGO_ENABLED=0` preserved; the core dependency count stays two (`modernc.org/sqlite`, `golang.org/x/sys`); Charm links only under `//go:build tui`.
- **I7 — Untrusted input is data.** Model-authored `Evidence.{Value,SourceURL,Statement,ExtractionMethod}` are DATA the verifier asserts over — never instructions, never echoed unfenced into the verifier's `Output` (the table carries only harness-trusted fields). `renderReport` surfaces only verifier-produced status/id/field as trusted and keeps prose `guard.Wrap`-fenced; the report HTML escapes all untrusted cells and carries no script. Verifier code is trusted Go and may parse a raw fetched body host-side before any fencing.

---

## 8. Promotion into `docs/TASKS.md` (closing note)

Promoting these specs into the canonical DAG is **itself a serialized contract task** (`P11-T36`, §5). It is the only Phase-11 task that touches a contract file, and it runs solo, last, after every staging doc is final. Promotion touches:

- **`docs/TASKS.md`** (contract) — add the `P11-T##` rows to the master DAG table (the exact 6-column shape `| ID | Phase | Title | Depends on | Owns | Note |`), paste the full per-task specs into a new `## Phase 11 — NilCore as a verifier-backed artifact factory` section, and update the "later phases" note.
- **`docs/ARCHITECTURE.md`** (contract) — register the new leaf packages (`internal/worktreefs`, `internal/browserwire`, `internal/artifact`, `internal/artifact/evverify`, `internal/artifact/packs/*`, `internal/requeue`, `internal/egressprofile`, `internal/report`, `internal/report/render`) as extension points; note the artifact/claim/evidence out-of-band carrier and the `ArtifactVerifier`-into-`Composite` seam.
- **`CHANGELOG.md`** (append-only) — one entry under `## [Unreleased]`.

Suggested CHANGELOG line:

```
- **P11** — NilCore as a verifier-backed artifact factory: typed evidence artifacts (`internal/artifact` + `internal/artifact/evverify`) where every claim's GREEN is produced by an `ArtifactVerifier` composed into `verify.Composite` (I2), riding out-of-band of the frozen `backend.Result` as worktree JSON (I1); reusable domain verifier packs (web/software/finance/ui, curl-in-box, no new module, I6); typed worker results on `spawn.Result` (prose stays fenced, I7); granular per-claim requeue reusing the DAG + `continue_from`; named research egress profiles (narrow-only, default-deny preserved, I4/R9); and a read-only `nilcore report` verification UI over the append-only log (I5). All additive, opt-in, flag/env-gated — default binary byte-identical. _Owns:_ internal/{worktreefs,browserwire,artifact,artifact/evverify,artifact/packs/*,requeue,egressprofile,report,report/render}, internal/{spawn,roster,super,onboard}, cmd/nilcore, docs/ROADMAP-{EVIDENCE-ARTIFACTS,DOMAIN-PACKS,TYPED-RESULTS,GRANULAR-REQUEUE,EGRESS-PROFILES,VERIFICATION-REPORT}.md. _(Phase 11)_
```

*Nothing here is "Done" until it lands through the `CLAUDE.md` §5 protocol — one branch, one verified change, in a disposable worktree, the way NilCore ships everything.*
