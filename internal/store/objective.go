// Standing-objectives backlog persistence (AUTO-T01, Phase-16 Pillar 7). These
// typed methods are the durable backing for internal/objective's narrow Store
// seam: the autonomy daemon's idle self-service queue. *Store satisfies the
// objective.Store interface directly — Put/Get/List/Disable take and return the
// leaf's objective.Objective, so the wiring layer installs *store.Store into an
// objective.Backlog without an adapter.
//
// Import direction: store depends on the objective leaf for its typed shape, never
// the reverse (objective is a pure stdlib leaf whose deps_test forbids importing
// store). The objectives table is additive and default-empty, riding the package's
// single serialized writer (Store.db has SetMaxOpenConns(1)) and the existing
// hand-written database/sql style: ExecContext for writes, Query/QueryRow for
// reads, RFC3339Nano timestamps as TEXT. With nothing enqueued the methods return
// zero rows and no existing query is affected (the default-off contract).
//
// Goal is operator-authored, inert data: this store never interprets it as
// instructions (I7). The CRUD here is reachable only from the operator-only host
// verb (AUTO-T07), never from a sandboxed model tool.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"nilcore/internal/objective"
)

// PutObjective inserts or replaces a standing objective by ID. Enabled is stored as
// 0/1, MinPeriod as nanoseconds, LastRun as RFC3339Nano UTC (” for the zero time so
// a never-run objective round-trips to a zero Time). The whole record is carried
// verbatim — this satisfies objective.Store.Put.
func (s *Store) PutObjective(ctx context.Context, o objective.Objective) error {
	enabled := 0
	if o.Enabled {
		enabled = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO objectives (id, goal, priority, enabled, min_period_ns, last_run)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     goal=excluded.goal, priority=excluded.priority, enabled=excluded.enabled,
		     min_period_ns=excluded.min_period_ns, last_run=excluded.last_run`,
		o.ID, o.Goal, o.Priority, enabled, int64(o.MinPeriod), formatTS(o.LastRun))
	if err != nil {
		return fmt.Errorf("put objective %q: %w", o.ID, err)
	}
	return nil
}

// GetObjective returns the objective with the given ID, or objective.ErrNotFound if
// absent (the leaf's sentinel, so callers test with errors.Is). This satisfies
// objective.Store.Get; the Backlog normalizes any wrapping of ErrNotFound.
func (s *Store) GetObjective(ctx context.Context, id string) (objective.Objective, error) {
	var o objective.Objective
	var enabled int
	var minPeriodNS int64
	var lastRun string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, goal, priority, enabled, min_period_ns, last_run FROM objectives WHERE id = ?`, id).
		Scan(&o.ID, &o.Goal, &o.Priority, &enabled, &minPeriodNS, &lastRun)
	if err == sql.ErrNoRows {
		return objective.Objective{}, fmt.Errorf("get objective %q: %w", id, objective.ErrNotFound)
	}
	if err != nil {
		return objective.Objective{}, fmt.Errorf("get objective %q: %w", id, err)
	}
	o.Enabled = enabled != 0
	o.MinPeriod = time.Duration(minPeriodNS)
	o.LastRun = parseTS(lastRun)
	return o, nil
}

// ListObjectives returns every objective (enabled or not). Order is by descending
// priority then ascending id — deterministic, though the Backlog re-sorts the same
// way, so the store's order is not contractual. An empty table yields a nil slice
// and no error. This satisfies objective.Store.List.
func (s *Store) ListObjectives(ctx context.Context) ([]objective.Objective, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, goal, priority, enabled, min_period_ns, last_run
		 FROM objectives ORDER BY priority DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list objectives: %w", err)
	}
	defer rows.Close()
	var out []objective.Objective
	for rows.Next() {
		var o objective.Objective
		var enabled int
		var minPeriodNS int64
		var lastRun string
		if err := rows.Scan(&o.ID, &o.Goal, &o.Priority, &enabled, &minPeriodNS, &lastRun); err != nil {
			return nil, fmt.Errorf("scan objective: %w", err)
		}
		o.Enabled = enabled != 0
		o.MinPeriod = time.Duration(minPeriodNS)
		o.LastRun = parseTS(lastRun)
		out = append(out, o)
	}
	return out, rows.Err()
}

// DisableObjective marks the objective inert (paused, not deleted) by clearing its
// enabled flag — a disabled objective is retained so an operator can re-enable it.
// Disabling an absent ID returns objective.ErrNotFound. This satisfies
// objective.Store.Disable.
func (s *Store) DisableObjective(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE objectives SET enabled = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("disable objective %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("disable objective %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("disable objective %q: %w", id, objective.ErrNotFound)
	}
	return nil
}

// objectiveStore adapts *Store to objective.Store by mapping the interface's short
// method names onto the typed *Objective methods above. The wiring layer constructs
// it with ObjectiveStore and hands it to objective.New; *Store carries the longer
// names (PutObjective/…) so its method set stays unambiguous alongside UpsertTask et al.
type objectiveStore struct{ s *Store }

func (a objectiveStore) Put(ctx context.Context, o objective.Objective) error {
	return a.s.PutObjective(ctx, o)
}

func (a objectiveStore) Get(ctx context.Context, id string) (objective.Objective, error) {
	return a.s.GetObjective(ctx, id)
}

func (a objectiveStore) List(ctx context.Context) ([]objective.Objective, error) {
	return a.s.ListObjectives(ctx)
}

func (a objectiveStore) Disable(ctx context.Context, id string) error {
	return a.s.DisableObjective(ctx, id)
}

// ObjectiveStore returns an objective.Store backed by this *Store, for the wiring
// layer to install into an objective.Backlog (objective.New). It is a thin adapter
// over the typed *Objective methods; constructing it never touches the DB.
func (s *Store) ObjectiveStore() objective.Store { return objectiveStore{s: s} }

// Compile-time proof the adapter satisfies the leaf's narrow Store seam. If
// objective.Store ever drifts, this fails the build in this package.
var _ objective.Store = objectiveStore{}
