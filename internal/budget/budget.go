// Package budget is concurrent-safe cost metering with ceiling enforcement
// (P6-T02). A run can fan out across many tasks; left unbounded, a single
// looping task — or the run as a whole — can burn unbounded model spend. The
// Ledger meters tokens and dollars per task and globally, and refuses any
// charge that would breach a per-task or global dollar ceiling *before*
// recording it, so the ceiling is a hard wall, not an after-the-fact report.
package budget

import (
	"context"
	"errors"
	"sync"
)

// epsilon absorbs float64 rounding so a charge meant to land exactly on a
// ceiling (e.g. 0.90 + 0.10 == 1.0000000000000001 in binary float) is not
// spuriously refused. It is far below any meaningful sub-cent cost.
const epsilon = 1e-9

// ErrCeiling is returned by Charge when the charge would exceed the task's
// ceiling or the global ceiling. The charge is rejected and nothing is recorded.
var ErrCeiling = errors.New("budget: charge would exceed ceiling")

// meter is the running spend for one scope (a task, or the global total).
type meter struct {
	tokens  int
	dollars float64
}

// Ledger meters spend per task and globally and enforces dollar ceilings.
// The zero value is not usable; call New. All methods are safe for concurrent
// use.
type Ledger struct {
	mu       sync.RWMutex
	tasks    map[string]*meter
	ceilings map[string]float64 // per-task dollar ceiling; absent = unlimited
	global   meter
	gceiling float64 // global dollar ceiling; 0 = unlimited
}

// New returns an empty ledger with no ceilings set (everything unlimited until
// a ceiling is declared).
func New() *Ledger {
	return &Ledger{
		tasks:    map[string]*meter{},
		ceilings: map[string]float64{},
	}
}

// SetTaskCeiling caps total dollars chargeable to task. A non-positive value
// removes the cap (unlimited).
func (l *Ledger) SetTaskCeiling(task string, dollars float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dollars <= 0 {
		delete(l.ceilings, task)
		return
	}
	l.ceilings[task] = dollars
}

// SetGlobalCeiling caps total dollars across all tasks. A non-positive value
// removes the cap (unlimited).
func (l *Ledger) SetGlobalCeiling(dollars float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if dollars <= 0 {
		l.gceiling = 0
		return
	}
	l.gceiling = dollars
}

// Charge records tokens and dollars against task and the global total. It
// honors ctx cancellation. If the charge would push the task or the global
// total past its ceiling, it records nothing and returns ErrCeiling. A
// negative token or dollar amount is rejected with an error and not recorded.
func (l *Ledger) Charge(ctx context.Context, task string, tokens int, dollars float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tokens < 0 || dollars < 0 {
		return errors.New("budget: negative charge")
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	m := l.tasks[task]
	var taskDollars float64
	if m != nil {
		taskDollars = m.dollars
	}
	if cap, ok := l.ceilings[task]; ok && taskDollars+dollars > cap+epsilon {
		return ErrCeiling
	}
	if l.gceiling > 0 && l.global.dollars+dollars > l.gceiling+epsilon {
		return ErrCeiling
	}

	if m == nil {
		m = &meter{}
		l.tasks[task] = m
	}
	m.tokens += tokens
	m.dollars += dollars
	l.global.tokens += tokens
	l.global.dollars += dollars
	return nil
}

// Spent returns the tokens and dollars recorded against task. An unknown task
// reports zero.
func (l *Ledger) Spent(task string) (tokens int, dollars float64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if m := l.tasks[task]; m != nil {
		return m.tokens, m.dollars
	}
	return 0, 0
}

// Total returns the tokens and dollars recorded across all tasks.
func (l *Ledger) Total() (tokens int, dollars float64) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.global.tokens, l.global.dollars
}
