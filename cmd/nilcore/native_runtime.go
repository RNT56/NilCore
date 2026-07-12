package main

// The native loop is constructed by three production front-door families:
// run/watch/propose-edit (buildBackend), terminal chat/TUI (chatNativeBackend), and
// channel serve (serveNativeBackend). These shared seams used to be wired independently,
// which repeatedly let a capability ship on one door while remaining inert on another.
// Keep the front-door-specific tools, inbox, emitter, ask, and wake seams at their call
// sites; configure every cross-cutting runtime capability exactly once here.

import (
	"context"

	"nilcore/internal/advisor"
	"nilcore/internal/backend"
	"nilcore/internal/memory"
	"nilcore/internal/meter"
	"nilcore/internal/model"
	"nilcore/internal/sandbox"
)

type nativeRuntimeConfig struct {
	Provider    model.Provider
	Advisor     advisorCfg
	Box         sandbox.Sandbox
	Memory      *memory.Memory
	Project     string
	SteeringDir string
}

// configureNativeRuntime applies the cross-cutting seams every native front door must
// share. Optional features remain nil/off byte-identically: no advisor means no advisor,
// nil memory means no memory context, NILCORE_LIVE_INDEX off means no live session, and
// an absent steering file means no trusted steering context.
func configureNativeRuntime(n *backend.Native, cfg nativeRuntimeConfig) {
	if n == nil {
		return
	}

	// Repo orientation and proactive context compaction are baseline runtime features.
	// The sandbox worktree is the model's actual view; fall back to the project root only
	// for a defensive nil-box construction in tests or future read-only hosts.
	workdir := cfg.Project
	if cfg.Box != nil && cfg.Box.Workdir() != "" {
		workdir = cfg.Box.Workdir()
	}
	if workdir != "" {
		n.RepoContext = func(context.Context) string { return repoMap(workdir, repoMapBudget) }
	}
	n.CtxWindow = meter.CtxWindow

	// A fresh advisor per drive preserves the per-drive call ceiling. If the main
	// provider is metered, meteredAdvisor shares its ledger so strong-model spend cannot
	// escape the one budget wall.
	if cfg.Advisor.prov != nil {
		n.Advisor = advisor.New(meteredAdvisor(cfg.Provider, cfg.Advisor.prov), cfg.Advisor.maxCalls)
		n.EscalateAfter = cfg.Advisor.escalateAfter
	}

	// Live code intelligence is opt-in and worktree-aware; memory and steering are the
	// same shared context sources on every door. SteeringDir stays explicit because chat
	// loads from the principal's repo while run loads from its disposable worktree.
	if envOptIn("NILCORE_LIVE_INDEX") && cfg.Project != "" {
		n.LiveSession = liveSession(cfg.Memory, cfg.Project)
	}
	attachMemoryContext(n, cfg.Memory, cfg.Project)
	if cfg.SteeringDir != "" {
		attachSteering(n, cfg.SteeringDir)
	}
}
