package board

// trace.go is the board's source–claim trace projection: it turns the trusted
// per-shard evidence the Board already holds into a flat, renderable trace WITHOUT ever
// touching a model-authored Value. This is the I7 boundary in miniature — the scoreboard
// must be able to show "which source backed which verdict" so an operator can audit a
// run, but the asserted DATUM (Evidence.Value) is untrusted and never enters the trace.
//
// What rides, what does not (I7 + I3). PROJECTED: the verifier-set Status/Detail/
// Verifier (TRUSTED verdict fields) and the key-free SourceURL (provenance — required
// key-free by I3, included here precisely because it is the audit anchor). NEVER
// PROJECTED: the model-authored Value/Statement/ExtractionMethod. A Board.ShardRow
// already carries only the safe fields, so this file cannot leak a Value even by
// mistake — there is no Value field to read.

import "sort"

// Trace is one shard's source-to-verdict row, projected from TRUSTED fields only. It
// ties the verifier's verdict (Status/Detail/Verifier) to the key-free SourceURL that
// backed it (I3 provenance) — and deliberately carries NO model-authored Value (I7), so
// the whole slice is safe to render, log, or marshal verbatim.
type Trace struct {
	Shard     string
	Pass      int
	Passed    bool
	Status    string
	Detail    string
	Verifier  string
	SourceURL string // key-free provenance (I3) — the audit anchor, safe to show
}

// Traces projects a Snapshot's per-shard rows into source–claim traces. It reads ONLY
// the trusted, renderable fields each ShardRow carries (Status/Detail/Verifier +
// key-free SourceURL) — there is no model-authored Value to project, which is the point
// (I7). The result is sorted by shard id for a deterministic render, mirroring the
// Snapshot's own ordering.
func Traces(s Snapshot) []Trace {
	out := make([]Trace, 0, len(s.Shards))
	for i := range s.Shards {
		r := s.Shards[i]
		out = append(out, Trace{
			Shard:     r.ID,
			Pass:      r.Pass,
			Passed:    r.Passed,
			Status:    r.Status,
			Detail:    r.Detail,
			Verifier:  r.Verifier,
			SourceURL: r.SourceURL,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Shard < out[j].Shard })
	return out
}
