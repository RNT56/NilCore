package main

import (
	"context"
	"fmt"
	"strings"

	"nilcore/internal/codeintel/graph"
	"nilcore/internal/codeintel/live"
	"nilcore/internal/memory"
)

// liveSession builds the backend.Native LiveSession seam (P3-T16): for a run's
// worktree it opens an in-memory code graph, seeds it from the worktree's Go files,
// and fuses it with project memory. update() re-indexes one edited file; query()
// renders the current call-graph neighborhood + memory leads for a symbol;
// closeFn() releases the graph (called when the loop returns, so the handle is
// task-scoped — no leak). A graph-open failure degrades to nil funcs (the loop runs
// without the `live` tool). Enabled opt-in via NILCORE_LIVE_INDEX.
func liveSession(mem *memory.Memory, project string) func(string) (func(context.Context, string), func(context.Context, string) string, func()) {
	return func(dir string) (func(context.Context, string), func(context.Context, string) string, func()) {
		g, err := graph.Open(":memory:")
		if err != nil {
			return nil, nil, nil
		}
		ix := &live.Index{Graph: g, Memory: mem, Project: project}
		_ = ix.IndexDir(context.Background(), dir) // best-effort initial index

		update := func(ctx context.Context, path string) { _ = ix.Update(ctx, path) }
		query := func(ctx context.Context, symbol string) string {
			facts, err := ix.Query(ctx, symbol)
			if err != nil {
				return "live query failed: " + err.Error()
			}
			return renderLiveFacts(symbol, facts)
		}
		closeFn := func() { _ = g.Close() }
		return update, query, closeFn
	}
}

// renderLiveFacts formats fused live facts (graph neighborhood + memory leads) as a
// compact, model-readable block. The native loop fences it as untrusted data (I7).
func renderLiveFacts(symbol string, facts []live.Fact) string {
	if len(facts) == 0 {
		return fmt.Sprintf("no live facts for %q (unknown symbol, or no neighbors/memory yet).", symbol)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "live code intelligence for %q:\n", symbol)
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s [%s] — %s\n", f.Symbol, f.Provenance, f.Detail)
	}
	return strings.TrimRight(sb.String(), "\n")
}
