# NilCore × agentic-flows

NilCore is a **consumer** of the portable [`agentic-flows`](https://github.com/RNT56/agentic-flows)
contract — the versioned, runtime-agnostic workflow layer shared across the RNT56
agentic projects (ThinClaw, NilCore, CrustCore). `agentic-flows` owns the *contract*
(flow specs, JSON Schema, the `flowctl` CLI, adapter contracts, evidence bundles);
NilCore owns the *runtime*. The two stay decoupled: NilCore vendors no flow code and
adds no module — it consumes a decoded flow as data.

In the agentic-flows project model NilCore's natural role is **sandboxed worker
execution and supervision**: it consumes a flow's `agent_task` and `tool` nodes and
runs them through its own verified machinery (the verifier stays the sole authority on
"done", I2; secrets stay host-side, I3; model-emitted execution stays sandboxed, I4).

## The seam

| Piece | What it is |
| --- | --- |
| `internal/agenticflows` | The pure adapter: maps a decoded flow (`Flow`/`Node`/`Edge`) onto NilCore's `spawn.Subtask` (agent_task → worker dispatch) and `sandbox` plans (tool → in-box exec). It parses no YAML and imports no orchestrator — a leaf. |
| `cmd/nilcore/flows.go` | The `nilcore flows` command — the reachable entry point over the adapter. |

NilCore advertises this capability vocabulary to the contract (what the
sandboxed-worker runtime actually does):

```
repo.checkout · patch.apply · command.run · tool.exec
task.decompose · worker.dispatch · evidence.capture · human.review
```

A flow that requires anything outside this set is reported as **not consumable** by
`validate` rather than half-run.

## Using it

NilCore is **stdlib-only** (invariant I6 — no YAML module), so it consumes a flow as
**JSON**. The `flowctl` CLI in agentic-flows emits JSON; or convert a `flow.yaml`
(`python -c 'import yaml,json,sys;json.dump(yaml.safe_load(sys.stdin),sys.stdout)' < flow.yaml > flow.json`).

```sh
# Preflight: can NilCore consume this flow? (no execution; exit 1 if not)
nilcore flows validate -flow flow.json

# Run the flow's agent_task nodes through the verified decompose preset
nilcore flows run -flow flow.json -dir ./repo
```

`validate` prints whether the flow lists `nilcore` as a supported core, whether every
required capability is supported, the **worker-dispatch plan** (one subtask per
`agent_task`, with derived dependencies), and the **sandbox tool plans** (one per
`tool` node). It exits non-zero on an unconsumable flow, so it doubles as a CI/preflight
gate.

`run` composes the `agent_task` nodes into a goal and dispatches them through the proven
[`decompose`](ROADMAP-KERNEL-V2.md) preset: each becomes an independent verified
single-task run whose branch is integrated into one re-verified tip (a child that
conflicts or turns the tree red is dropped — I2). It fails closed if the flow is not
consumable. Node dependencies are derived from the flow's `produces`→`requires`
dataflow.

## What it deliberately does NOT do

- It does not fetch or vendor flows — the operator supplies the JSON (remote fetch is
  gated external infra).
- It does not interpret flow text as control instructions — a flow's goals are inert
  data executed through the normal verified path (I7).
- It does not pre-execute `tool`/`verify`/`approval` nodes itself — those are honored by
  the run machinery (the sandbox, the verifier, the human gate) during `run`.

## Status

The adapter + `validate` are complete and tested (`validate` is exercised against real
flows from the agentic-flows catalog). `run`'s end-to-end execution reuses the verified
decompose engine. The agentic-flows contract is under active development, so the decoder
ignores unknown fields (forward-compatible) and the capability set will grow as NilCore's
worker surface does.
