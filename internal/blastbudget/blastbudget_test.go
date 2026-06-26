package blastbudget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recSink records emitted events for assertion.
type recSink struct {
	mu     sync.Mutex
	events []struct {
		kind   string
		detail map[string]any
	}
}

func (s *recSink) Emit(kind string, detail map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, struct {
		kind   string
		detail map[string]any
	}{kind, detail})
}

func (s *recSink) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.kind
	}
	return out
}

func count(kinds []string, want string) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
}

func TestHostAxis(t *testing.T) {
	ctx := context.Background()
	b := New()
	b.SetHostCeiling(2)

	if err := b.ChargeHost(ctx, "Api.Example.com"); err != nil {
		t.Fatalf("host 1: %v", err)
	}
	// idempotent (and case/space-normalized): same host is free, never re-counted.
	if err := b.ChargeHost(ctx, "  api.example.com  "); err != nil {
		t.Fatalf("host 1 repeat: %v", err)
	}
	if u := b.Used(""); u.Hosts != 1 {
		t.Fatalf("hosts = %d, want 1 (idempotent)", u.Hosts)
	}
	if err := b.ChargeHost(ctx, "b.example.com"); err != nil {
		t.Fatalf("host 2: %v", err)
	}
	// third distinct host breaches.
	err := b.ChargeHost(ctx, "c.example.com")
	if !errors.Is(err, ErrHostCeiling) || !errors.Is(err, ErrBlast) {
		t.Fatalf("3rd host err = %v, want ErrHostCeiling (wrapping ErrBlast)", err)
	}
	if u := b.Used(""); u.Hosts != 2 {
		t.Fatalf("after breach hosts = %d, want 2 (nothing recorded on breach)", u.Hosts)
	}
	// empty host rejected, not a ceiling error.
	if err := b.ChargeHost(ctx, "   "); err == nil || errors.Is(err, ErrBlast) {
		t.Fatalf("empty host err = %v, want a plain rejection", err)
	}
}

func TestHostAxisUnlimited(t *testing.T) {
	ctx := context.Background()
	b := New() // no ceiling
	for _, h := range []string{"a", "b", "c", "d", "e"} {
		if err := b.ChargeHost(ctx, h); err != nil {
			t.Fatalf("unlimited host %q: %v", h, err)
		}
	}
	if u := b.Used(""); u.Hosts != 5 {
		t.Fatalf("hosts = %d, want 5", u.Hosts)
	}
}

func TestIrreversibleAxis(t *testing.T) {
	ctx := context.Background()
	b := New()
	b.SetIrreversibleCeiling(3)
	if err := b.ChargeIrreversible(ctx, 2); err != nil {
		t.Fatalf("charge 2: %v", err)
	}
	// 2+2 > 3 breaches; nothing recorded.
	if err := b.ChargeIrreversible(ctx, 2); !errors.Is(err, ErrIrrevCeiling) {
		t.Fatalf("breach err = %v, want ErrIrrevCeiling", err)
	}
	if u := b.Used(""); u.Irreversible != 2 {
		t.Fatalf("irreversible = %d, want 2 (nothing recorded on breach)", u.Irreversible)
	}
	// exact landing is allowed.
	if err := b.ChargeIrreversible(ctx, 1); err != nil {
		t.Fatalf("charge to exact ceiling: %v", err)
	}
	if err := b.ChargeIrreversible(ctx, -1); err == nil || errors.Is(err, ErrBlast) {
		t.Fatalf("negative err = %v, want a plain rejection", err)
	}
}

func TestWallAxisAndReconcile(t *testing.T) {
	ctx := context.Background()
	b := New()
	b.SetWallCeiling(10 * time.Second)

	// pre-charge a bound, then credit back the unused remainder (the reconcile
	// pattern BR-T03 uses around a sandbox exec).
	if err := b.ChargeWall(ctx, 10*time.Second); err != nil {
		t.Fatalf("pre-charge bound: %v", err)
	}
	b.CreditWall(7 * time.Second) // actual elapsed was 3s
	if u := b.Used(""); u.Wall != 3*time.Second {
		t.Fatalf("wall = %v, want 3s after reconcile", u.Wall)
	}
	// a further 8s would breach (3+8 > 10).
	if err := b.ChargeWall(ctx, 8*time.Second); !errors.Is(err, ErrWallCeiling) {
		t.Fatalf("breach err = %v, want ErrWallCeiling", err)
	}
	if u := b.Used(""); u.Wall != 3*time.Second {
		t.Fatalf("after breach wall = %v, want 3s", u.Wall)
	}
	if err := b.ChargeWall(ctx, -1); err == nil || errors.Is(err, ErrBlast) {
		t.Fatalf("negative duration err = %v, want a plain rejection", err)
	}
	// credit never drops below zero.
	b.CreditWall(time.Hour)
	if u := b.Used(""); u.Wall != 0 {
		t.Fatalf("wall = %v, want clamped to 0", u.Wall)
	}
}

func TestDayDollarAxisRolls(t *testing.T) {
	ctx := context.Background()
	b := New()
	b.SetAutoApprovalDollarCeiling(5.0)

	if err := b.ChargeAutoApprovalDollars(ctx, "2026-06-26", 4.0); err != nil {
		t.Fatalf("day1 charge: %v", err)
	}
	// 4+2 > 5 on the SAME day breaches.
	if err := b.ChargeAutoApprovalDollars(ctx, "2026-06-26", 2.0); !errors.Is(err, ErrDayCeiling) {
		t.Fatalf("same-day breach err = %v, want ErrDayCeiling", err)
	}
	if u := b.Used("2026-06-26"); u.Dollars != 4.0 {
		t.Fatalf("day1 dollars = %v, want 4.0 (nothing recorded on breach)", u.Dollars)
	}
	// the NEXT day key is a fresh window (rolls at midnight by construction).
	if err := b.ChargeAutoApprovalDollars(ctx, "2026-06-27", 4.0); err != nil {
		t.Fatalf("day2 charge should be a fresh window: %v", err)
	}
	if u := b.Used("2026-06-27"); u.Dollars != 4.0 {
		t.Fatalf("day2 dollars = %v, want 4.0", u.Dollars)
	}
	if err := b.ChargeAutoApprovalDollars(ctx, "", 1.0); err == nil || errors.Is(err, ErrBlast) {
		t.Fatalf("empty day err = %v, want a plain rejection", err)
	}
}

func TestContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := New()
	b.SetHostCeiling(1)
	if err := b.ChargeHost(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChargeHost on cancelled ctx = %v, want context.Canceled", err)
	}
	if err := b.ChargeIrreversible(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChargeIrreversible on cancelled ctx = %v", err)
	}
	if err := b.ChargeWall(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChargeWall on cancelled ctx = %v", err)
	}
	if err := b.ChargeAutoApprovalDollars(ctx, "2026-06-26", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ChargeAutoApprovalDollars on cancelled ctx = %v", err)
	}
	if u := b.Used(""); u.Hosts != 0 || u.Irreversible != 0 || u.Wall != 0 {
		t.Fatalf("cancelled charges must record nothing, got %+v", u)
	}
}

func TestNilReceiverIsNoOp(t *testing.T) {
	var b *Budget // nil
	ctx := context.Background()
	// every method must be safe and inert on nil (an unwired seam).
	b.SetSink(&recSink{})
	b.SetHostCeiling(1)
	b.SetIrreversibleCeiling(1)
	b.SetWallCeiling(time.Second)
	b.SetAutoApprovalDollarCeiling(1)
	b.CreditWall(time.Second)
	if err := b.ChargeHost(ctx, "a"); err != nil {
		t.Fatalf("nil ChargeHost = %v, want nil", err)
	}
	if err := b.ChargeIrreversible(ctx, 99); err != nil {
		t.Fatalf("nil ChargeIrreversible = %v, want nil", err)
	}
	if err := b.ChargeWall(ctx, time.Hour); err != nil {
		t.Fatalf("nil ChargeWall = %v, want nil", err)
	}
	if err := b.ChargeAutoApprovalDollars(ctx, "2026-06-26", 99); err != nil {
		t.Fatalf("nil ChargeAutoApprovalDollars = %v, want nil", err)
	}
	if u := b.Used(""); u != (Usage{}) {
		t.Fatalf("nil Used = %+v, want zero", u)
	}
}

func TestSinkEmitsChargeAndBreach(t *testing.T) {
	ctx := context.Background()
	s := &recSink{}
	b := New()
	b.SetSink(s)
	b.SetHostCeiling(1)

	if err := b.ChargeHost(ctx, "a.example.com"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if err := b.ChargeHost(ctx, "b.example.com"); !errors.Is(err, ErrHostCeiling) {
		t.Fatalf("breach: %v", err)
	}
	kinds := s.kinds()
	if count(kinds, "blast_charge") != 1 {
		t.Errorf("blast_charge count = %d, want 1; events=%v", count(kinds, "blast_charge"), kinds)
	}
	if count(kinds, "blast_breach") != 1 {
		t.Errorf("blast_breach count = %d, want 1; events=%v", count(kinds, "blast_breach"), kinds)
	}
}

// TestSinkSilentWhenUnlimited proves an unmetered axis does not spam the sink.
func TestSinkSilentWhenUnlimited(t *testing.T) {
	ctx := context.Background()
	s := &recSink{}
	b := New()
	b.SetSink(s)
	// no ceilings set ⇒ unlimited ⇒ charges advance state but emit nothing.
	_ = b.ChargeHost(ctx, "a")
	_ = b.ChargeIrreversible(ctx, 3)
	_ = b.ChargeWall(ctx, time.Second)
	_ = b.ChargeAutoApprovalDollars(ctx, "2026-06-26", 1)
	if k := s.kinds(); len(k) != 0 {
		t.Fatalf("unlimited axes emitted %v, want silent", k)
	}
}

func TestConcurrentChargesAreRaceFree(t *testing.T) {
	ctx := context.Background()
	b := New()
	b.SetIrreversibleCeiling(1000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = b.ChargeHost(ctx, string(rune('a'+n%26)))
			_ = b.ChargeIrreversible(ctx, 1)
			_ = b.ChargeWall(ctx, time.Millisecond)
			_ = b.ChargeAutoApprovalDollars(ctx, "2026-06-26", 0.01)
			_ = b.Used("2026-06-26")
		}(i)
	}
	wg.Wait()
	if u := b.Used("2026-06-26"); u.Irreversible != 50 {
		t.Fatalf("irreversible = %d, want 50 after 50 concurrent charges", u.Irreversible)
	}
}
