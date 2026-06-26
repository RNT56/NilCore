package experience

import (
	"context"

	"nilcore/internal/memory"
	"nilcore/internal/store"
	"nilcore/internal/trust"
)

// storeReader is the hot, store-backed Reader (EXP-T04): it answers from the
// derived projection tables the Projector writes, rather than replaying the whole
// log per query. It is the read path the orchestrator uses. Memory is read
// through internal/memory directly (the projection does not copy it).
type storeReader struct {
	s   *store.Store
	mem *memory.Memory // nil ⇒ no lessons
}

// OverStore returns a Reader backed by s's projection tables. mem may be nil.
func OverStore(s *store.Store, mem *memory.Memory) Reader { return &storeReader{s: s, mem: mem} }

func (r *storeReader) BackendStanding(ctx context.Context, taskClass string) ([]trust.Stat, error) {
	rows, err := r.s.BackendStandings(ctx, taskClass)
	if err != nil {
		return nil, err
	}
	out := make([]trust.Stat, 0, len(rows))
	for _, bs := range rows {
		out = append(out, trust.Stat{
			Backend:  bs.Backend,
			Races:    int(bs.Races),
			Wins:     int(bs.Passes),
			PassRate: rate(bs.Passes, bs.Races),
		})
	}
	return out, nil
}

func (r *storeReader) ConfigStanding(ctx context.Context) ([]trust.ConfigStat, error) {
	rows, err := r.s.ConfigStandings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]trust.ConfigStat, 0, len(rows))
	for _, cs := range rows {
		out = append(out, trust.ConfigStat{
			Config:    cs.Config,
			PassRate:  cs.PassRate,
			TotalCost: cs.TotalCost,
			Cases:     int(cs.Cases),
		})
	}
	return out, nil
}

func (r *storeReader) Lessons(ctx context.Context, scope, project, keyword string, max int) ([]memory.Record, error) {
	if r.mem == nil {
		return nil, nil
	}
	recs, err := r.mem.Query(ctx, scope, project, keyword)
	if err != nil {
		return nil, err
	}
	if max > 0 && len(recs) > max {
		recs = recs[:max]
	}
	return recs, nil
}

func (r *storeReader) Outcomes(ctx context.Context, taskClass string) (Aggregate, error) {
	rows, err := r.s.BackendStandings(ctx, taskClass)
	if err != nil {
		return Aggregate{}, err
	}
	agg := Aggregate{Class: taskClass}
	var costSum float64
	var latSum int64
	for _, bs := range rows {
		agg.Races += int(bs.Races)
		agg.Passes += int(bs.Passes)
		costSum += bs.CostUSD
		latSum += bs.LatencyNS
		if bs.LastSeen.After(agg.LastSeen) {
			agg.LastSeen = bs.LastSeen
		}
	}
	// The store keeps running sums, not raw samples, so the rollup reports the mean
	// per contest (a faithful summary; the per-sample median lives in the log-only
	// reader). Zero contests ⇒ zero, never a divide.
	if agg.Races > 0 {
		agg.MedianCostUSD = costSum / float64(agg.Races)
		agg.MedianLatency = float64(latSum) / float64(agg.Races)
	}
	return agg, nil
}

func (r *storeReader) ChainVerified(ctx context.Context) (bool, error) {
	m, ok, err := r.s.ExpMeta(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		// Never rebuilt ⇒ an empty projection is vacuously verified (it grants
		// nothing; an auto-approval bar simply finds no standings).
		return true, nil
	}
	return m.ChainOK, nil
}

func rate(passes, races int64) float64 {
	if races == 0 {
		return 0
	}
	return float64(passes) / float64(races)
}
