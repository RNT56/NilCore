// Package blastbudget is a stdlib-only capability budget: a single, composable
// "blast-radius" envelope that bounds an unattended run BEYOND dollars. Where
// internal/budget meters tokens and dollars, blastbudget meters the four axes a
// graduated-auto-approval / autonomous run must not exceed:
//
//   - the number of DISTINCT egress hosts reached,
//   - the count of IRREVERSIBLE (auto-approved) actions taken,
//   - cumulative SANDBOX WALL-TIME, and
//   - the per-UTC-day AUTO-APPROVAL DOLLAR spend.
//
// It is the hard runtime fence the graduated-auto-approval policy (Phase 16,
// docs/ROADMAP-CLOSED-LOOP.md Pillar 5) reads: a P5 grant may only ever proceed
// WITHIN the remaining blast envelope (min(P5, blast)), and a blast breach is
// final.
//
// Design: a SIBLING of internal/budget, never a modification of it, and it
// imports no nilcore package (a Sink interface decouples it from the event log —
// the capguard pattern), so it stays a pure leaf (see deps_test.go) and adds no
// module (I6). Every Charge* method is FAIL-CLOSED: it refuses BEFORE recording
// any charge that would breach a ceiling, exactly like budget.Ledger. A
// non-positive ceiling means that axis is UNLIMITED (matching budget's
// convention); a nil *Budget is a no-op on every method, so an unwired seam is
// byte-identical to today. The zero value is not usable — call New.
package blastbudget

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// epsilon absorbs float64 rounding on the dollar axis so a charge meant to land
// exactly on a ceiling is not spuriously refused (mirrors internal/budget).
const epsilon = 1e-9

// Budget meters the four capability axes and enforces their ceilings. All
// methods are safe for concurrent use and safe to call on a nil receiver. Build
// one with New; a non-positive ceiling leaves that axis unlimited.
type Budget struct {
	mu sync.RWMutex

	hosts    map[string]struct{} // distinct egress hosts charged this run
	hostCeil int                 // 0 = unlimited

	irrev     int // irreversible/auto-approved actions taken this run
	irrevCeil int // 0 = unlimited

	wall     time.Duration // cumulative sandbox wall-time this run
	wallCeil time.Duration // 0 = unlimited

	day     map[string]float64 // per-UTC-day ("2006-01-02") auto-approval dollars
	dayCeil float64            // 0 = unlimited

	log Sink // nil = silent
}

// New returns an empty budget with every axis unlimited until a ceiling is set.
func New() *Budget {
	return &Budget{
		hosts: map[string]struct{}{},
		day:   map[string]float64{},
	}
}

// SetSink installs the (optional) audit sink. A nil sink is silent.
func (b *Budget) SetSink(s Sink) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = s
}

// SetHostCeiling caps the number of distinct egress hosts. Non-positive = unlimited.
func (b *Budget) SetHostCeiling(n int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 {
		b.hostCeil = 0
		return
	}
	b.hostCeil = n
}

// SetIrreversibleCeiling caps the count of irreversible/auto-approved actions.
// Non-positive = unlimited.
func (b *Budget) SetIrreversibleCeiling(n int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 {
		b.irrevCeil = 0
		return
	}
	b.irrevCeil = n
}

// SetWallCeiling caps cumulative sandbox wall-time. Non-positive = unlimited.
func (b *Budget) SetWallCeiling(d time.Duration) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if d <= 0 {
		b.wallCeil = 0
		return
	}
	b.wallCeil = d
}

// SetAutoApprovalDollarCeiling caps per-UTC-day auto-approval dollars.
// Non-positive = unlimited.
func (b *Budget) SetAutoApprovalDollarCeiling(dollars float64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if dollars <= 0 {
		b.dayCeil = 0
		return
	}
	b.dayCeil = dollars
}

// ChargeHost records that host was reached. The host set is idempotent: a host
// already charged this run is free (never re-counted), so repeated requests to
// one allowed host never trip the fence. Reaching a NEW host when the distinct
// count is already at the ceiling is refused with ErrHostCeiling and nothing is
// recorded. host is normalized like policy.Egress.Allow (lower/trim).
func (b *Budget) ChargeHost(ctx context.Context, host string) error {
	if b == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return errors.New("blastbudget: empty host")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, seen := b.hosts[host]; seen {
		return nil // idempotent — already counted
	}
	if b.hostCeil > 0 && len(b.hosts)+1 > b.hostCeil {
		b.breach("host", float64(len(b.hosts)), float64(b.hostCeil))
		return ErrHostCeiling
	}
	b.hosts[host] = struct{}{}
	if b.hostCeil > 0 {
		b.charge("host", float64(len(b.hosts)), float64(b.hostCeil), map[string]any{"host": host})
	}
	return nil
}

// ChargeIrreversible records n irreversible/auto-approved actions. A charge that
// would push the count past the ceiling is refused with ErrIrrevCeiling and
// nothing is recorded. n must be non-negative.
func (b *Budget) ChargeIrreversible(ctx context.Context, n int) error {
	if b == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n < 0 {
		return errors.New("blastbudget: negative count")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.irrevCeil > 0 && b.irrev+n > b.irrevCeil {
		b.breach("irreversible", float64(b.irrev), float64(b.irrevCeil))
		return ErrIrrevCeiling
	}
	b.irrev += n
	if b.irrevCeil > 0 {
		b.charge("irreversible", float64(b.irrev), float64(b.irrevCeil), nil)
	}
	return nil
}

// ChargeWall records d of sandbox wall-time. A charge that would push the
// cumulative total past the ceiling is refused with ErrWallCeiling and nothing
// is recorded. d must be non-negative.
func (b *Budget) ChargeWall(ctx context.Context, d time.Duration) error {
	if b == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d < 0 {
		return errors.New("blastbudget: negative duration")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.wallCeil > 0 && b.wall+d > b.wallCeil {
		b.breach("wall", b.wall.Seconds(), b.wallCeil.Seconds())
		return ErrWallCeiling
	}
	b.wall += d
	if b.wallCeil > 0 {
		b.charge("wall", b.wall.Seconds(), b.wallCeil.Seconds(), nil)
	}
	return nil
}

// CreditWall returns d of previously-charged wall-time to the budget (clamped at
// zero), so a pre-charged bound can be reconciled against the actual elapsed
// time after a sandbox exec without ever over-counting. d must be non-negative;
// crediting never emits a breach.
func (b *Budget) CreditWall(d time.Duration) {
	if b == nil || d <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.wall -= d
	if b.wall < 0 {
		b.wall = 0
	}
}

// CreditIrreversible returns n previously-charged irreversible slots to the budget
// (clamped at zero). It lets a charge taken speculatively at the top of a decision be
// rolled back when a LATER gate in the SAME decision refuses (e.g. the per-day dollar
// ceiling) — so a denied auto-approval consumes nothing and the axes never over-count.
// n must be non-negative; crediting never emits a breach. A nil receiver is a no-op.
func (b *Budget) CreditIrreversible(n int) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.irrev -= n
	if b.irrev < 0 {
		b.irrev = 0
	}
}

// ChargeAutoApprovalDollars records amount against the per-UTC-day auto-approval
// window keyed by day ("2006-01-02"). The caller supplies the day key (so the
// leaf stays pure and testable — no wall-clock read here), and the window rolls
// at midnight by construction because each day is a distinct key. A charge that
// would push that day past the ceiling is refused with ErrDayCeiling and nothing
// is recorded. amount must be non-negative.
func (b *Budget) ChargeAutoApprovalDollars(ctx context.Context, day string, amount float64) error {
	if b == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if amount < 0 {
		return errors.New("blastbudget: negative amount")
	}
	if day == "" {
		return errors.New("blastbudget: empty day key")
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.dayCeil > 0 && b.day[day]+amount > b.dayCeil+epsilon {
		b.breach("auto_dollars", b.day[day], b.dayCeil)
		return ErrDayCeiling
	}
	b.day[day] += amount
	if b.dayCeil > 0 {
		b.charge("auto_dollars", b.day[day], b.dayCeil, map[string]any{"day": day})
	}
	return nil
}

// Usage is a point-in-time snapshot of per-axis consumption, for surfacing
// (e.g. a gauge or the graduated-approval evidence). Dollars/DayCeiling are for
// the requested day key.
type Usage struct {
	Hosts        int
	HostCeiling  int
	Irreversible int
	IrrevCeiling int
	Wall         time.Duration
	WallCeiling  time.Duration
	Dollars      float64
	DayCeiling   float64
}

// Used returns a snapshot of current consumption. A nil budget reports zero.
func (b *Budget) Used(day string) Usage {
	if b == nil {
		return Usage{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return Usage{
		Hosts:        len(b.hosts),
		HostCeiling:  b.hostCeil,
		Irreversible: b.irrev,
		IrrevCeiling: b.irrevCeil,
		Wall:         b.wall,
		WallCeiling:  b.wallCeil,
		Dollars:      b.day[day],
		DayCeiling:   b.dayCeil,
	}
}

// charge emits a metadata-only blast_charge event. The caller holds b.mu.
func (b *Budget) charge(axis string, used, ceiling float64, extra map[string]any) {
	b.emit("blast_charge", axis, used, ceiling, extra, "")
}

// breach emits a metadata-only blast_breach event. The caller holds b.mu.
func (b *Budget) breach(axis string, used, ceiling float64) {
	b.emit("blast_breach", axis, used, ceiling, nil, "blast-radius: "+axis+" budget exhausted")
}

func (b *Budget) emit(kind, axis string, used, ceiling float64, extra map[string]any, action string) {
	if b.log == nil {
		return
	}
	d := map[string]any{"axis": axis, "used": used, "ceiling": ceiling, "host_count": len(b.hosts)}
	if action != "" {
		d["action"] = action
	}
	for k, v := range extra {
		d[k] = v
	}
	b.log.Emit(kind, d)
}
