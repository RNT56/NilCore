// Package memory represents conventions, decisions, and learned facts, keyed by
// project and global scope (P4-T03). It is store-backed (SQLite) with a typed
// write API and a keyword query API — the substrate the native loop reads at task
// start (P4-T04) and writes back to after a task (P4-T05), so the agent improves
// across projects over time.
package memory

import (
	"context"
	"strings"

	"nilcore/internal/store"
)

// Scopes for a memory record.
const (
	ScopeProject = "project"
	ScopeGlobal  = "global"
)

// Record is one learned fact.
type Record struct {
	Scope   string // ScopeProject | ScopeGlobal
	Project string // set for project scope
	Key     string
	Value   string
}

// Memory is the store-backed memory API.
type Memory struct {
	store *store.Store
}

// New wraps a store.
func New(s *store.Store) *Memory { return &Memory{store: s} }

// Write persists a record (defaulting to project scope).
func (m *Memory) Write(ctx context.Context, r Record) error {
	if r.Scope == "" {
		r.Scope = ScopeProject
	}
	_, err := m.store.PutMemory(ctx, store.Memory{Scope: r.Scope, Project: r.Project, Key: r.Key, Value: r.Value})
	return err
}

// Query returns records in a scope (and project, for project scope), filtered by
// a case-insensitive keyword over key/value (empty keyword returns all).
func (m *Memory) Query(ctx context.Context, scope, project, keyword string) ([]Record, error) {
	recs, err := m.store.QueryMemory(ctx, scope, project)
	if err != nil {
		return nil, err
	}
	kw := strings.ToLower(keyword)
	var out []Record
	for _, r := range recs {
		if kw == "" || strings.Contains(strings.ToLower(r.Key), kw) || strings.Contains(strings.ToLower(r.Value), kw) {
			out = append(out, Record{Scope: r.Scope, Project: r.Project, Key: r.Key, Value: r.Value})
		}
	}
	return out, nil
}
