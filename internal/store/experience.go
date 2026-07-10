// Experience-layer projection queries (EXP-T02). These typed methods are the
// persistent backing for the closed-loop experience layer — a derived read model
// rebuilt from the append-only event log, not a source of truth. They ride the
// package's single serialized writer (Store.db has SetMaxOpenConns(1)) and follow
// the existing hand-written database/sql style: ExecContext for writes, Query/
// QueryRow for reads, RFC3339Nano timestamps as TEXT. Every table is additive and
// default-empty; with nothing projected, the methods return zero rows and no
// existing query is affected.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BackendStanding is one (class, backend) standing row: the aggregate race
// outcome of a backend within a task class. It is a derived projection — the
// authority on whether any single race passed remains the verifier (I2); this is
// only a folded summary used to inform (never decide) routing.
type BackendStanding struct {
	Class     string
	Backend   string
	Races     int64
	Passes    int64
	CostUSD   float64
	LatencyNS int64
	LastSeen  time.Time
}

// UpsertBackendStanding inserts or replaces a backend standing keyed by
// (class, backend). LastSeen is stored as RFC3339Nano UTC; a zero time is written
// as the empty string (matching the column default) so a read round-trips to a
// zero Time.
func (s *Store) UpsertBackendStanding(ctx context.Context, bs BackendStanding) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO exp_backend_standing (class, backend, races, passes, cost_usd, latency_ns, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(class, backend) DO UPDATE SET
		     races=excluded.races, passes=excluded.passes, cost_usd=excluded.cost_usd,
		     latency_ns=excluded.latency_ns, last_seen=excluded.last_seen`,
		bs.Class, bs.Backend, bs.Races, bs.Passes, bs.CostUSD, bs.LatencyNS, formatTS(bs.LastSeen))
	if err != nil {
		return fmt.Errorf("upsert backend standing: %w", err)
	}
	return nil
}

// BackendStandings returns the standings for a class in (class, backend) order.
// An empty table (nothing projected) yields a nil slice and no error.
func (s *Store) BackendStandings(ctx context.Context, class string) ([]BackendStanding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT class, backend, races, passes, cost_usd, latency_ns, last_seen
		 FROM exp_backend_standing WHERE class = ? ORDER BY backend`, class)
	if err != nil {
		return nil, fmt.Errorf("query backend standings: %w", err)
	}
	defer rows.Close()
	var out []BackendStanding
	for rows.Next() {
		var bs BackendStanding
		var lastSeen string
		if err := rows.Scan(&bs.Class, &bs.Backend, &bs.Races, &bs.Passes, &bs.CostUSD, &bs.LatencyNS, &lastSeen); err != nil {
			return nil, err
		}
		bs.LastSeen = parseTS(lastSeen)
		out = append(out, bs)
	}
	return out, rows.Err()
}

// ConfigStanding is one configuration's aggregate standing: how a tunable config
// (model/prompt/tool set) fared across verified cases.
type ConfigStanding struct {
	Config    string
	PassRate  float64
	TotalCost float64
	Cases     int64
}

// UpsertConfigStanding inserts or replaces a config standing keyed by config.
func (s *Store) UpsertConfigStanding(ctx context.Context, cs ConfigStanding) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO exp_config_standing (config, pass_rate, total_cost, cases)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(config) DO UPDATE SET
		     pass_rate=excluded.pass_rate, total_cost=excluded.total_cost, cases=excluded.cases`,
		cs.Config, cs.PassRate, cs.TotalCost, cs.Cases)
	if err != nil {
		return fmt.Errorf("upsert config standing: %w", err)
	}
	return nil
}

// ConfigStandings returns all config standings in config order. An empty table
// yields a nil slice and no error.
func (s *Store) ConfigStandings(ctx context.Context) ([]ConfigStanding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT config, pass_rate, total_cost, cases FROM exp_config_standing ORDER BY config`)
	if err != nil {
		return nil, fmt.Errorf("query config standings: %w", err)
	}
	defer rows.Close()
	var out []ConfigStanding
	for rows.Next() {
		var cs ConfigStanding
		if err := rows.Scan(&cs.Config, &cs.PassRate, &cs.TotalCost, &cs.Cases); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// ExpMeta is the experience projection's rebuild watermark (a singleton row).
// SourceSeq is the last event seq folded in, ChainOK whether the event hash chain
// verified at that rebuild, and RebuiltAt when the projection was last rebuilt.
type ExpMeta struct {
	SourceSeq int64
	ChainOK   bool
	RebuiltAt time.Time
}

// ExpMeta returns the projection watermark. On a fresh DB (no row written) it
// returns a zero-value ExpMeta and ok=false — never sql.ErrNoRows — so a caller
// can treat "never rebuilt" without special-casing the error.
func (s *Store) ExpMeta(ctx context.Context) (ExpMeta, bool, error) {
	var m ExpMeta
	var chainOK int
	var rebuiltAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT source_seq, chain_ok, rebuilt_at FROM exp_meta WHERE id = 1`).
		Scan(&m.SourceSeq, &chainOK, &rebuiltAt)
	if err == sql.ErrNoRows {
		return ExpMeta{}, false, nil
	}
	if err != nil {
		return ExpMeta{}, false, fmt.Errorf("query exp meta: %w", err)
	}
	m.ChainOK = chainOK != 0
	m.RebuiltAt = parseTS(rebuiltAt)
	return m, true, nil
}

// SetExpMeta writes (creating or replacing) the singleton projection watermark.
// The id=1 CHECK in the schema keeps it a singleton; the upsert keys on id.
func (s *Store) SetExpMeta(ctx context.Context, m ExpMeta) error {
	chainOK := 0
	if m.ChainOK {
		chainOK = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO exp_meta (id, source_seq, chain_ok, rebuilt_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		     source_seq=excluded.source_seq, chain_ok=excluded.chain_ok, rebuilt_at=excluded.rebuilt_at`,
		m.SourceSeq, chainOK, formatTS(m.RebuiltAt))
	if err != nil {
		return fmt.Errorf("set exp meta: %w", err)
	}
	return nil
}

// ClearExpStandings deletes every row from both projection standing tables
// (exp_backend_standing and exp_config_standing) in ONE transaction, resetting the
// derived read model to empty WITHOUT touching the append-only event log (I5 — the
// projection is a droppable, rebuildable derivation, never a source of truth). It is
// the "truncate" half of an authoritative truncate-then-rebuild: the experience
// Projector calls it so a full Rebuild (or a rotation-triggered re-derive) reflects
// ONLY the current log, dropping standings for (class, backend) or config keys that no
// longer appear in it — which an upsert-only rebuild could never remove. The exp_meta
// watermark is owned separately by SetExpMeta and is intentionally left untouched here.
func (s *Store) ClearExpStandings(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("clear exp standings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM exp_backend_standing`,
		`DELETE FROM exp_config_standing`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("clear exp standings: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("clear exp standings: commit: %w", err)
	}
	return nil
}

// formatTS renders a time as RFC3339Nano UTC, or "" for the zero time (matching
// the column defaults so a zero Time round-trips through the empty string).
func formatTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(tsFmt)
}

// parseTS parses an RFC3339Nano timestamp, returning the zero time for "" (or any
// unparseable value) — the inverse of formatTS.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(tsFmt, s)
	return t
}
