package requeue

import (
	"reflect"
	"testing"
)

// TestLedger exercises the bounded retry budget: distinct keys stay independent,
// Bump increments and returns, Exhausted enforces the MaxAttempts ceiling (with
// the disabled-by-default MaxAttempts==0 case), and Marshal/UnmarshalLedger
// round-trips (including the empty-blob-resumes-disabled path).
func TestLedger(t *testing.T) {
	t.Run("key joins ArtifactID and ClaimID; distinct claims distinct counters", func(t *testing.T) {
		ua := Unit{ArtifactID: "co-041", ClaimID: "revenue"}
		ub := Unit{ArtifactID: "co-041", ClaimID: "margin"}
		uc := Unit{ArtifactID: "co-099", ClaimID: "revenue"}
		if key(ua) != "co-041/revenue" {
			t.Fatalf("key(ua)=%q, want co-041/revenue", key(ua))
		}
		led := &Ledger{MaxAttempts: 3}
		led.Bump(ua)
		led.Bump(ua)
		led.Bump(ub)
		// ua and ub share an artifact but are distinct claims => distinct counters.
		if led.attemptFor(ua) != 2 {
			t.Errorf("ua attempts=%d, want 2", led.attemptFor(ua))
		}
		if led.attemptFor(ub) != 1 {
			t.Errorf("ub attempts=%d, want 1", led.attemptFor(ub))
		}
		// uc shares a claim id with ua but a different artifact => its own counter.
		if led.attemptFor(uc) != 0 {
			t.Errorf("uc attempts=%d, want 0 (different artifact)", led.attemptFor(uc))
		}
	})

	t.Run("Bump increments and returns; lazily allocates the map", func(t *testing.T) {
		led := &Ledger{MaxAttempts: 5} // nil Attempts map
		u := Unit{ArtifactID: "a", ClaimID: "c"}
		if got := led.Bump(u); got != 1 {
			t.Errorf("first Bump returned %d, want 1", got)
		}
		if got := led.Bump(u); got != 2 {
			t.Errorf("second Bump returned %d, want 2", got)
		}
		if led.Attempts["a/c"] != 2 {
			t.Errorf("stored count=%d, want 2", led.Attempts["a/c"])
		}
	})

	t.Run("MaxAttempts==0 disables requeue: every unit exhausted at attempt 0", func(t *testing.T) {
		led := &Ledger{} // MaxAttempts 0
		u := Unit{ArtifactID: "a", ClaimID: "c"}
		if !led.Exhausted(u) {
			t.Fatal("disabled ledger: want Exhausted true at attempt 0")
		}
	})

	// Boundary table for MaxAttempts 1/2/3: a unit is eligible (not exhausted) for
	// exactly MaxAttempts Bumps, then exhausted from the ceiling onward.
	t.Run("exhaustion boundary across MaxAttempts 1/2/3", func(t *testing.T) {
		for _, max := range []int{1, 2, 3} {
			led := &Ledger{MaxAttempts: max}
			u := Unit{ArtifactID: "a", ClaimID: "c"}
			for attempt := 0; attempt < max; attempt++ {
				if led.Exhausted(u) {
					t.Errorf("max=%d: Exhausted true at attempt %d, want false (budget remaining)", max, attempt)
				}
				led.Bump(u)
			}
			// After MaxAttempts Bumps the count equals MaxAttempts => exhausted.
			if !led.Exhausted(u) {
				t.Errorf("max=%d: want Exhausted true after %d bumps", max, max)
			}
			// One more Bump keeps it exhausted (>= ceiling, never loops past).
			led.Bump(u)
			if !led.Exhausted(u) {
				t.Errorf("max=%d: want Exhausted true past the ceiling", max)
			}
		}
	})

	t.Run("Marshal then UnmarshalLedger round-trips", func(t *testing.T) {
		orig := &Ledger{MaxAttempts: 3, Attempts: map[string]int{"co-041/revenue": 2, "co-041/margin": 1}}
		data, err := orig.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got, err := UnmarshalLedger(data)
		if err != nil {
			t.Fatalf("UnmarshalLedger: %v", err)
		}
		if got.MaxAttempts != orig.MaxAttempts {
			t.Errorf("MaxAttempts=%d, want %d", got.MaxAttempts, orig.MaxAttempts)
		}
		if !reflect.DeepEqual(got.Attempts, orig.Attempts) {
			t.Errorf("Attempts=%v, want %v", got.Attempts, orig.Attempts)
		}
	})

	t.Run("empty blob yields zero Ledger no error (old snapshot resumes disabled)", func(t *testing.T) {
		led, err := UnmarshalLedger(nil)
		if err != nil {
			t.Fatalf("UnmarshalLedger(nil): %v", err)
		}
		if led.MaxAttempts != 0 || len(led.Attempts) != 0 {
			t.Errorf("want zero Ledger, got %+v", led)
		}
		// A zero Ledger is disabled: every unit reads exhausted.
		if !led.Exhausted(Unit{ArtifactID: "a", ClaimID: "c"}) {
			t.Error("zero Ledger from empty blob should report Exhausted true")
		}
	})

	t.Run("nil receiver Marshal yields a loadable zero-Ledger blob", func(t *testing.T) {
		var nilLed *Ledger
		data, err := nilLed.Marshal()
		if err != nil {
			t.Fatalf("nil Marshal: %v", err)
		}
		got, err := UnmarshalLedger(data)
		if err != nil {
			t.Fatalf("UnmarshalLedger: %v", err)
		}
		if got.MaxAttempts != 0 {
			t.Errorf("MaxAttempts=%d, want 0", got.MaxAttempts)
		}
	})

	t.Run("corrupt blob is an error not a silent zero", func(t *testing.T) {
		if _, err := UnmarshalLedger([]byte("{not json")); err == nil {
			t.Fatal("want error on corrupt blob, got nil")
		}
	})
}
