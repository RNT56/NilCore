// Package maint runs the housekeeping pass that keeps a long-lived NilCore host
// from drowning in its own debris: stale task worktrees, dead delegate
// containers, and ever-growing log files. Every run is disposable by
// construction (CLAUDE.md §5), but "disposable" only pays off if something
// actually disposes — that something is here.
//
// The collector is deliberately blind to git and docker. It talks to two
// injectable seams (List and Remove) so the real wiring (git worktree list,
// docker ps) lives at the call site and the policy here — never touch anything
// still Active, and one failed removal must not abandon the rest — stays small,
// pure, and hermetically testable.
package maint

import (
	"context"
	"errors"
	"fmt"
)

// Item is one reclaimable resource — a worktree or a container. Active marks it
// as in-use by a live run; the collector must leave Active items untouched.
type Item struct {
	ID     string
	Active bool
}

// GC is a garbage collector over reclaimable resources. List enumerates the
// current candidates; Remove disposes of one by ID. Both are injected so the
// same Collect logic drives git worktrees, docker containers, or a fake.
type GC struct {
	List   func() ([]Item, error)
	Remove func(id string) error
}

// Collect removes every stale (!Active) item and leaves Active ones in place,
// returning the IDs it removed in List order. It is collect-and-continue: a
// Remove failure on one item is recorded and joined into the returned error,
// but the pass still attempts every remaining stale item. ctx cancellation is
// honored between items so a long sweep can be interrupted cleanly.
func (g GC) Collect(ctx context.Context) (removed []string, err error) {
	if g.List == nil {
		return nil, fmt.Errorf("maint: GC.List is nil")
	}
	if g.Remove == nil {
		return nil, fmt.Errorf("maint: GC.Remove is nil")
	}

	items, err := g.List()
	if err != nil {
		return nil, fmt.Errorf("maint: listing items: %w", err)
	}

	var errs []error
	for _, it := range items {
		if cerr := ctx.Err(); cerr != nil {
			errs = append(errs, fmt.Errorf("maint: collection canceled: %w", cerr))
			break
		}
		if it.Active {
			continue
		}
		if rerr := g.Remove(it.ID); rerr != nil {
			errs = append(errs, fmt.Errorf("maint: removing %q: %w", it.ID, rerr))
			continue
		}
		removed = append(removed, it.ID)
	}

	return removed, errors.Join(errs...)
}
