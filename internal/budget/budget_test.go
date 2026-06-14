package budget_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"nilcore/internal/budget"
)

// near reports whether two dollar amounts are equal within float rounding. The
// ledger sums float64s, so exact == on decimal literals is unreliable.
func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSpendAccruesPerTaskAndGlobally(t *testing.T) {
	l := budget.New()
	ctx := context.Background()

	if err := l.Charge(ctx, "a", 100, 0.10); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(ctx, "a", 50, 0.05); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(ctx, "b", 200, 0.20); err != nil {
		t.Fatal(err)
	}

	if tok, dol := l.Spent("a"); tok != 150 || !near(dol, 0.15) {
		t.Errorf("Spent(a) = %d, %.4f; want 150, 0.15", tok, dol)
	}
	if tok, dol := l.Spent("b"); tok != 200 || !near(dol, 0.20) {
		t.Errorf("Spent(b) = %d, %.4f; want 200, 0.20", tok, dol)
	}
	if tok, dol := l.Total(); tok != 350 || !near(dol, 0.35) {
		t.Errorf("Total = %d, %.4f; want 350, 0.35", tok, dol)
	}
}

func TestUnknownTaskReportsZero(t *testing.T) {
	l := budget.New()
	if tok, dol := l.Spent("nope"); tok != 0 || dol != 0 {
		t.Errorf("Spent(nope) = %d, %.2f; want 0, 0", tok, dol)
	}
}

func TestTaskCeilingRefusesAndDoesNotRecord(t *testing.T) {
	l := budget.New()
	ctx := context.Background()
	l.SetTaskCeiling("a", 1.00)

	if err := l.Charge(ctx, "a", 100, 0.90); err != nil {
		t.Fatal(err)
	}
	// Would push task a to 1.20 > 1.00: must be refused.
	if err := l.Charge(ctx, "a", 100, 0.30); !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("over-ceiling charge err = %v; want ErrCeiling", err)
	}
	// The refused charge must not have been recorded — task and global unchanged.
	if tok, dol := l.Spent("a"); tok != 100 || !near(dol, 0.90) {
		t.Errorf("after refusal Spent(a) = %d, %.4f; want 100, 0.90", tok, dol)
	}
	if tok, dol := l.Total(); tok != 100 || !near(dol, 0.90) {
		t.Errorf("after refusal Total = %d, %.4f; want 100, 0.90", tok, dol)
	}
	// A charge that reaches the ceiling is allowed.
	if err := l.Charge(ctx, "a", 10, 0.10); err != nil {
		t.Fatalf("charge to ceiling err = %v; want nil", err)
	}
	if _, dol := l.Spent("a"); !near(dol, 1.00) {
		t.Errorf("Spent(a) dollars = %.4f; want 1.00", dol)
	}
}

func TestGlobalCeilingEnforcedAcrossTasks(t *testing.T) {
	l := budget.New()
	ctx := context.Background()
	l.SetGlobalCeiling(1.00)

	if err := l.Charge(ctx, "a", 10, 0.60); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge(ctx, "b", 10, 0.30); err != nil {
		t.Fatal(err)
	}
	// a+b = 0.90; charging 0.20 against c would push global to 1.10 > 1.00.
	if err := l.Charge(ctx, "c", 10, 0.20); !errors.Is(err, budget.ErrCeiling) {
		t.Fatalf("over-global charge err = %v; want ErrCeiling", err)
	}
	// c never recorded.
	if tok, dol := l.Spent("c"); tok != 0 || dol != 0 {
		t.Errorf("Spent(c) = %d, %.2f; want 0, 0", tok, dol)
	}
	if _, dol := l.Total(); !near(dol, 0.90) {
		t.Errorf("Total dollars = %.4f; want 0.90", dol)
	}
}

func TestNegativeAndCanceled(t *testing.T) {
	l := budget.New()

	if err := l.Charge(context.Background(), "a", -1, 0.10); err == nil {
		t.Error("negative tokens: want error, got nil")
	}
	if err := l.Charge(context.Background(), "a", 1, -0.10); err == nil {
		t.Error("negative dollars: want error, got nil")
	}
	if tok, dol := l.Total(); tok != 0 || dol != 0 {
		t.Errorf("after rejected charges Total = %d, %.2f; want 0, 0", tok, dol)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Charge(ctx, "a", 1, 0.10); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx err = %v; want context.Canceled", err)
	}
}

func TestNonPositiveCeilingRemovesCap(t *testing.T) {
	l := budget.New()
	ctx := context.Background()
	l.SetTaskCeiling("a", 1.00)
	l.SetTaskCeiling("a", 0) // clears the cap
	l.SetGlobalCeiling(2.00)
	l.SetGlobalCeiling(-1) // clears the cap

	if err := l.Charge(ctx, "a", 10, 5.00); err != nil {
		t.Fatalf("charge after caps cleared err = %v; want nil", err)
	}
}

func TestConcurrentChargesAreRaceFree(t *testing.T) {
	l := budget.New()
	ctx := context.Background()
	const goroutines = 64
	const perGoroutine = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			task := fmt.Sprintf("task-%d", g%8)
			for i := 0; i < perGoroutine; i++ {
				if err := l.Charge(ctx, task, 1, 0.01); err != nil {
					t.Errorf("charge err = %v", err)
					return
				}
				_, _ = l.Spent(task)
				_, _ = l.Total()
			}
		}(g)
	}
	wg.Wait()

	wantTokens := goroutines * perGoroutine
	if tok, _ := l.Total(); tok != wantTokens {
		t.Errorf("Total tokens = %d; want %d", tok, wantTokens)
	}
}
