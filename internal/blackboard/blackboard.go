// Package blackboard is concurrent-safe shared state for a multi-subworker run
// (P3-T03): task statuses (persisted to the store for durability) and named
// artifacts. Subworkers read their slice and write results — they share facts,
// not transcripts, so no cross-worker context stuffing.
package blackboard

import (
	"context"
	"sync"

	"nilcore/internal/store"
)

// Blackboard holds run-shared state. The zero value is not usable; call New.
type Blackboard struct {
	mu        sync.RWMutex
	statuses  map[string]string
	artifacts map[string]string
	store     *store.Store // optional durability for task statuses
}

// New returns a blackboard. If s is non-nil, statuses are persisted to it.
func New(s *store.Store) *Blackboard {
	return &Blackboard{statuses: map[string]string{}, artifacts: map[string]string{}, store: s}
}

// SetStatus records a task's status (and persists it when a store is wired).
func (b *Blackboard) SetStatus(ctx context.Context, taskID, goal, status string) error {
	b.mu.Lock()
	b.statuses[taskID] = status
	b.mu.Unlock()
	if b.store != nil {
		return b.store.UpsertTask(ctx, store.Task{ID: taskID, Goal: goal, Status: status})
	}
	return nil
}

// Status returns a task's status.
func (b *Blackboard) Status(taskID string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.statuses[taskID]
	return s, ok
}

// PutArtifact stores a named artifact (e.g. a subworker's result summary).
func (b *Blackboard) PutArtifact(key, value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.artifacts[key] = value
}

// Artifact returns a named artifact.
func (b *Blackboard) Artifact(key string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.artifacts[key]
	return v, ok
}

// Snapshot returns a copy of all task statuses.
func (b *Blackboard) Snapshot() map[string]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]string, len(b.statuses))
	for k, v := range b.statuses {
		out[k] = v
	}
	return out
}
