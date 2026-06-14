# test/

End-to-end fixtures and smoke checks (task P0-T04). Unit tests live next to the
code they cover under `internal/`; this tree is for whole-binary checks.

## `fixtures/failing-go/`

A tiny Go module with one **deliberately failing** test (`Add` returns `a - b`
instead of `a + b`). It is its **own module** (has its own `go.mod`) so the
project's `go test ./...` does not descend into it — the failing test is the
input to the smoke test, not part of the gate.

## `smoke/`

`TestNativeLoopConverges` builds the `nilcore` binary, copies the failing fixture
to a throwaway worktree, runs the **native backend** against it, and asserts the
verifier turns green (the agent fixed the bug). It **skips cleanly** unless all of:

- `ANTHROPIC_API_KEY` is set,
- a container runtime (`podman` or `docker`) is on `PATH`, and
- the sandbox image `nilcore/sandbox:latest` exists.

So `make verify` stays green without secrets or a runtime.

### Manual run

```sh
export ANTHROPIC_API_KEY=sk-...
podman build -t nilcore/sandbox:latest images/sandbox   # or docker
go test ./test/smoke/ -run TestNativeLoopConverges -v
```

> On Docker Desktop (macOS), ensure the temp dir used for the throwaway worktree
> is within a shared path. Podman rootless is the documented runtime.
