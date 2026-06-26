package self

import (
	"strings"
	"testing"

	"nilcore/eval"
)

// TestLoadWellFormed proves the frozen suite is non-empty and every case is
// well-formed (unique non-empty name, non-empty goal) — the data the flywheel
// relies on is sound.
func TestLoadWellFormed(t *testing.T) {
	s, h, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Version == "" {
		t.Error("suite version is empty")
	}
	if len(s.Cases) == 0 {
		t.Fatal("frozen suite is empty")
	}
	if h == "" {
		t.Fatal("suite hash is empty")
	}
	seen := map[string]bool{}
	for i, c := range s.Cases {
		if strings.TrimSpace(c.Name) == "" {
			t.Errorf("case %d has empty name", i)
		}
		if strings.TrimSpace(c.Goal) == "" {
			t.Errorf("case %q has empty goal", c.Name)
		}
		if seen[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
	}
}

// TestHashStableAcrossCalls is the golden test: the frozen identity is byte-stable
// across repeated Loads/Hashes, so the suite's pinned identity in the event log
// never drifts with the feature wired off.
func TestHashStableAcrossCalls(t *testing.T) {
	_, h1, err := Load()
	if err != nil {
		t.Fatalf("Load #1: %v", err)
	}
	_, h2, err := Load()
	if err != nil {
		t.Fatalf("Load #2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash drifted across calls: %q vs %q", h1, h2)
	}

	// And Suite.Hash agrees with the hash Load returns.
	s, hLoad, err := Load()
	if err != nil {
		t.Fatalf("Load #3: %v", err)
	}
	hMethod, err := s.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if hMethod != hLoad {
		t.Errorf("Suite.Hash %q != Load hash %q", hMethod, hLoad)
	}
}

// TestGoldenFrozenHash pins the exact content hash of the shipped suite. If this
// fails, the eval set changed: that is the C6 tamper signal — update the constant
// ONLY when the change to frozenCases/frozenVersion is intentional.
func TestGoldenFrozenHash(t *testing.T) {
	_, h, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if h != goldenHash {
		t.Errorf("frozen suite hash = %q, want %q\n"+
			"the self-eval set changed — if intentional, update goldenHash; "+
			"otherwise this is the C6 eval-set tamper guard firing", h, goldenHash)
	}
}

// TestMutationChangesHash proves the tamper guard: any edit to a case (here a
// copy with one mutated goal) yields a different hash. We mutate a defensive copy
// — never the frozen original — so this test cannot corrupt the suite.
func TestMutationChangesHash(t *testing.T) {
	base, baseHash, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Mutate one case's goal in an isolated copy.
	mutCases := make([]eval.Case, len(base.Cases))
	copy(mutCases, base.Cases)
	mutCases[0].Goal = mutCases[0].Goal + " (tampered)"
	mutated := Suite{Version: base.Version, Cases: mutCases}
	mutHash, err := mutated.Hash()
	if err != nil {
		t.Fatalf("Hash mutated: %v", err)
	}
	if mutHash == baseHash {
		t.Error("mutating a case goal did not change the suite hash")
	}

	// Reordering cases also changes identity (the suite is an ordered set).
	if len(base.Cases) >= 2 {
		reCases := make([]eval.Case, len(base.Cases))
		copy(reCases, base.Cases)
		reCases[0], reCases[1] = reCases[1], reCases[0]
		reHash, err := (Suite{Version: base.Version, Cases: reCases}).Hash()
		if err != nil {
			t.Fatalf("Hash reordered: %v", err)
		}
		if reHash == baseHash {
			t.Error("reordering cases did not change the suite hash")
		}
	}

	// Changing the version label also changes identity.
	verHash, err := (Suite{Version: base.Version + "x", Cases: base.Cases}).Hash()
	if err != nil {
		t.Fatalf("Hash reversioned: %v", err)
	}
	if verHash == baseHash {
		t.Error("changing the version label did not change the suite hash")
	}

	// And the original suite is untouched by all of the above.
	_, afterHash, err := Load()
	if err != nil {
		t.Fatalf("Load after mutation: %v", err)
	}
	if afterHash != baseHash {
		t.Errorf("frozen suite was mutated by a caller: %q != %q", afterHash, baseHash)
	}
}

// goldenHash is the pinned SHA-256 (hex) of the frozen suite. Kept as a literal
// const so any change to the eval set is loud: TestGoldenFrozenHash asserts the
// computed hash equals this value, and a mismatch is the C6 tamper signal. Update
// it ONLY when the change to frozenCases/frozenVersion is intentional.
const goldenHash = "8a63edb27f5a556d689b307246203d12d48755f96386f9ad16f67b2b95327143"
