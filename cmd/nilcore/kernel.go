package main

// kernel.go is the cmd-side wiring for the unified orchestration kernel (Phase 16,
// Pillar 8 — UOK, docs/ROADMAP-KERNEL.md). It is the SEAM where the three proven
// orchestration machines plug into the pure-leaf kernel as injected runners: `run`,
// `build`, and `swarm` become kernel Envelopes, so every entrypoint routes through ONE
// `kernel.Run` instead of calling a bespoke machine.
//
// DEFAULT-OFF (NILCORE_KERNEL unset): each *ViaKernel helper calls the legacy machine
// DIRECTLY, so the binary is byte-identical and the kernel is never constructed. When
// set, the helper wraps the SAME machine call as the envelope's Flat runner and routes
// it through kernel.Run — the machine's internal structure (orch's single-vs-supervised
// dispatch, the project loop's fan-out, the swarm's multi-pass) stays opaque to the
// kernel, so the event sequence is identical (proven by the equivalence harness,
// kernel_equiv_test.go). The kernel adds the unified entry + the granularity/recursion
// engine (available for MaxDepth>1); it never re-verifies or re-gates (I2/I3) — the
// wrapped machine owns all of that, and the native outcome flows back unchanged.

import (
	"context"
	"os"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/kernel"
	"nilcore/internal/project"
	"nilcore/internal/swarm"
)

// kernelEnabled reports whether orchestration routes through the unified kernel. It is
// the single default-off gate; unset ⇒ the legacy machine path, byte-identical.
func kernelEnabled() bool { return os.Getenv("NILCORE_KERNEL") != "" }

// agentToKernel / projectToKernel / swarmToKernel map a machine's native outcome onto the
// kernel's uniform Outcome. Verified is the machine's verifier verdict (I2) — never a
// self-report; the kernel only carries it, never sets it.
func agentToKernel(o agent.Outcome) kernel.Outcome {
	return kernel.Outcome{Backend: o.Backend, Summary: o.Summary, Verified: o.Verified, Detail: o.Detail, Branch: o.Branch}
}
func projectToKernel(o project.Outcome) kernel.Outcome {
	return kernel.Outcome{Backend: "project", Summary: o.Summary, Verified: o.Done, Detail: o.Reason, Branch: o.Branch}
}
func swarmToKernel(o swarm.Outcome) kernel.Outcome {
	return kernel.Outcome{Backend: "swarm", Summary: o.Reason, Verified: o.Done, Detail: o.Reason, Branch: o.TipBranch}
}

// runViaKernel routes a single-task run through the "run" envelope. The Flat runner wraps
// orch.Execute VERBATIM (orch owns its own FLAT-vs-supervised dispatch), so the run is
// event-for-event the legacy run. The native agent.Outcome is captured and returned
// unchanged, so the caller's reporting (which reads every field) is unaffected.
func runViaKernel(ctx context.Context, orch *agent.Orchestrator, t backend.Task) (agent.Outcome, error) {
	if !kernelEnabled() {
		return orch.Execute(ctx, t)
	}
	var native agent.Outcome
	env := kernel.Envelope{
		Name: "run",
		Flat: func(ctx context.Context, _ kernel.Node) (kernel.Outcome, error) {
			var err error
			native, err = orch.Execute(ctx, t)
			return agentToKernel(native), err
		},
	}
	_, err := kernel.Run(ctx, env, kernel.Node{ID: t.ID, Goal: t.Goal})
	return native, err
}

// buildViaKernel routes a supervised project build through the "build" envelope. The
// project loop's fan-out is opaque to the kernel (wrapped as the Flat runner); the full
// native project.Outcome (Iterations/Unmet/Promoted/…) is captured and returned.
func buildViaKernel(ctx context.Context, loop *project.Loop) (project.Outcome, error) {
	if !kernelEnabled() {
		return loop.Run(ctx)
	}
	var native project.Outcome
	env := kernel.Envelope{
		Name: "build",
		Flat: func(ctx context.Context, _ kernel.Node) (kernel.Outcome, error) {
			var err error
			native, err = loop.Run(ctx)
			return projectToKernel(native), err
		},
	}
	_, err := kernel.Run(ctx, env, kernel.Node{ID: "build", Goal: loop.Goal})
	return native, err
}

// swarmViaKernel routes a verified swarm through the "swarm" envelope. The controller's
// multi-pass convergence is opaque to the kernel; the full native swarm.Outcome is
// captured and returned.
func swarmViaKernel(ctx context.Context, c *swarm.Controller, st swarm.SwarmState, initial []swarm.Shard) (swarm.Outcome, error) {
	if !kernelEnabled() {
		return c.Run(ctx, st, initial)
	}
	var native swarm.Outcome
	env := kernel.Envelope{
		Name: "swarm",
		Flat: func(ctx context.Context, _ kernel.Node) (kernel.Outcome, error) {
			var err error
			native, err = c.Run(ctx, st, initial)
			return swarmToKernel(native), err
		},
	}
	_, err := kernel.Run(ctx, env, kernel.Node{ID: st.RunID, Goal: st.Goal})
	return native, err
}
