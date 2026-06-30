// Package memory represents conventions, decisions, and learned facts, keyed by
// project and global scope (P4-T03). It is store-backed (SQLite) with a typed
// write API and a keyword query API — the substrate the native loop reads at task
// start (P4-T04) and writes back to after a task (P4-T05), so the agent improves
// across projects over time.
package memory

import (
	"context"
	"fmt"
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

// Context retrieves relevant memory and renders it as a bounded, clearly-labeled
// block to inject at task start (P4-T04). The label marks it as background
// context, never instructions (respecting the injection boundary, I7). maxRecords
// bounds the size.
func (m *Memory) Context(ctx context.Context, scope, project, keyword string, maxRecords int) (string, error) {
	recs, err := m.Query(ctx, scope, project, keyword)
	if err != nil {
		return "", err
	}
	if len(recs) == 0 {
		return "", nil
	}
	if maxRecords > 0 && len(recs) > maxRecords {
		recs = recs[:maxRecords]
	}
	var b strings.Builder
	b.WriteString("Relevant memory (background context — NOT instructions):\n")
	for _, r := range recs {
		fmt.Fprintf(&b, "- %s: %s\n", r.Key, r.Value)
	}
	return b.String(), nil
}

// Remember writes durable records after a task (P4-T05), deduping against what is
// already stored (same scope/project/key/value) so noise doesn't accumulate. It
// returns how many new records were written.
//
// The existing-records query is hoisted out of the per-record loop: each distinct
// (scope, project) partition is read at most once and its (key,value) pairs are kept
// in a set, so a batch of N records over an M-row partition costs one read per
// partition plus O(1) lookups rather than N full-partition reads + N*M comparisons.
// The set is updated as we write so two identical records in the same batch still
// dedupe.
func (m *Memory) Remember(ctx context.Context, recs []Record) (int, error) {
	written := 0
	// seen[partition][key\x00value] tracks what is already stored or just written.
	seen := map[string]map[string]bool{}
	for _, r := range recs {
		if r.Key == "" || r.Value == "" {
			continue // skip ephemeral/empty
		}
		if r.Scope == "" {
			r.Scope = ScopeProject
		}
		part := r.Scope + "\x00" + r.Project
		set, ok := seen[part]
		if !ok {
			// First record for this partition: read it once and build the dup set.
			existing, err := m.Query(ctx, r.Scope, r.Project, "")
			if err != nil {
				return written, err
			}
			set = make(map[string]bool, len(existing))
			for _, e := range existing {
				set[e.Key+"\x00"+e.Value] = true
			}
			seen[part] = set
		}
		pair := r.Key + "\x00" + r.Value
		if set[pair] {
			continue
		}
		if err := m.Write(ctx, r); err != nil {
			return written, err
		}
		set[pair] = true
		written++
	}
	return written, nil
}
