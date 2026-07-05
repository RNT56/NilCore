package maint

// cap.go bounds a FAMILY of deliberately-kept resources to a fixed budget. The
// worktree GC (maint.go) reclaims items that are stale by construction; kept chat
// branches are different — each one is verified work an operator may still /diff
// or /apply, so none is individually stale. Left unbounded they grow one ref (and
// its reachable objects) per verified drive forever, so the policy here is a CAP:
// keep the newest Keep items, prune the rest. Like GC it is deliberately blind to
// git — List/Remove are injected seams — and it never touches an Active item (the
// branch a live session still carries), so a prune can never strand a pending
// /apply on the current branch.

import (
	"context"
	"errors"
	"fmt"
)

// Cap prunes a kept-resource family down to its Keep newest members.
//
// List enumerates the candidates NEWEST-FIRST — the caller owns the ordering
// (e.g. `git for-each-ref --sort=-committerdate`) because only it knows what
// "newest" means for the resource. Remove disposes of one by ID. An Active item
// is never removed and still consumes one slot of the budget (it IS a kept item),
// so the total retained set stays bounded by Keep plus any active overflow.
type Cap struct {
	List   func() ([]Item, error)
	Remove func(id string) error
	Keep   int
}

// Collect removes every non-Active item past the Keep newest, returning the IDs
// it removed in List order. Collect-and-continue like GC.Collect: one failed
// removal is recorded and joined into the returned error, but the pass still
// attempts every remaining surplus item. ctx cancellation is honored between
// items. A non-positive Keep prunes every non-Active item (a deliberate "keep
// nothing" sweep).
func (c Cap) Collect(ctx context.Context) (removed []string, err error) {
	if c.List == nil {
		return nil, fmt.Errorf("maint: Cap.List is nil")
	}
	if c.Remove == nil {
		return nil, fmt.Errorf("maint: Cap.Remove is nil")
	}

	items, err := c.List()
	if err != nil {
		return nil, fmt.Errorf("maint: listing items: %w", err)
	}

	var errs []error
	kept := 0
	for _, it := range items {
		if cerr := ctx.Err(); cerr != nil {
			errs = append(errs, fmt.Errorf("maint: cap canceled: %w", cerr))
			break
		}
		// Active items are untouchable (a live session still points at them) but
		// still count against the budget — they are retained members of the family.
		if it.Active {
			kept++
			continue
		}
		if kept < c.Keep {
			kept++
			continue
		}
		if rerr := c.Remove(it.ID); rerr != nil {
			errs = append(errs, fmt.Errorf("maint: removing %q: %w", it.ID, rerr))
			continue
		}
		removed = append(removed, it.ID)
	}

	return removed, errors.Join(errs...)
}
