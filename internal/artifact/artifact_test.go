package artifact

import (
	"strings"
	"testing"
	"time"
)

// claim is a tiny constructor so the Green table stays readable.
func claim(id string, st Status) Claim {
	return Claim{ID: id, Field: id + "_field", Evidence: Evidence{Value: id + "_val", Status: st}}
}

func TestArtifactGreen(t *testing.T) {
	tests := []struct {
		name   string
		claims []Claim
		want   bool
	}{
		{"all-pass", []Claim{claim("a", StatusPass), claim("b", StatusPass)}, true},
		{"single-pass", []Claim{claim("a", StatusPass)}, true},
		{"one-fail", []Claim{claim("a", StatusPass), claim("b", StatusFail)}, false},
		{"one-stale", []Claim{claim("a", StatusPass), claim("b", StatusStale)}, false},
		{"one-unverifiable", []Claim{claim("a", StatusPass), claim("b", StatusUnverifiable)}, false},
		{"one-unverified", []Claim{claim("a", StatusPass), claim("b", StatusUnverified)}, false},
		{"empty-claims", nil, false},
		{"empty-slice", []Claim{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Artifact{Claims: tt.claims}
			if got := a.Green(); got != tt.want {
				t.Fatalf("Green() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestArtifactStatusConstants pins the wire values of the status lifecycle — they
// are persisted to disk and matched by requeue/report, so a typo must fail loudly.
func TestArtifactStatusConstants(t *testing.T) {
	cases := map[Status]string{
		StatusUnverified:   "unverified",
		StatusPass:         "pass",
		StatusFail:         "fail",
		StatusStale:        "stale",
		StatusUnverifiable: "unverifiable",
	}
	for got, want := range cases {
		if string(got) != want {
			t.Errorf("status constant = %q, want %q", got, want)
		}
	}
	kinds := map[Kind]string{
		KindReport:    "report",
		KindMatrix:    "matrix",
		KindSpec:      "spec",
		KindBenchmark: "benchmark",
		KindDossier:   "research-dossier",
	}
	for got, want := range kinds {
		if string(got) != want {
			t.Errorf("kind constant = %q, want %q", got, want)
		}
	}
}

func TestArtifactRoundTrip(t *testing.T) {
	at := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	orig := &Artifact{
		ID:        "rpt-1",
		Kind:      KindReport,
		Title:     "Q4 review",
		CreatedAt: at,
		Claims: []Claim{
			{
				ID:        "company-041-revenue",
				Field:     "revenue_fy2024",
				Statement: "FY2024 revenue per the 10-K",
				Evidence: Evidence{
					Value:            "123456789",
					SourceURL:        "https://data.sec.gov/x?a=b&c=d",
					RetrievedAt:      at,
					ExtractionMethod: "json-path",
					Verifier:         "finance.sec_fact",
					Status:           StatusPass,
					Detail:           "matched within tolerance",
				},
			},
		},
	}

	b1, err := Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(b1)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// SchemaVersion is defaulted on marshal; the round-tripped value carries it.
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	b2, err := Marshal(got)
	if err != nil {
		t.Fatalf("Marshal#2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("Marshal not byte-stable across round-trip:\nfirst:\n%s\nsecond:\n%s", b1, b2)
	}

	// HTML-significant characters must survive verbatim (escaping is off).
	if !strings.Contains(string(b1), "a=b&c=d") {
		t.Errorf("ampersand SourceURL was HTML-escaped; got:\n%s", b1)
	}
}

func TestArtifactMarshalDefaultsSchemaVersion(t *testing.T) {
	a := &Artifact{ID: "x", Kind: KindSpec, Claims: []Claim{claim("a", StatusPass)}}
	if a.SchemaVersion != 0 {
		t.Fatalf("precondition: want zero SchemaVersion")
	}
	b, err := Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Marshal must not mutate the caller's value...
	if a.SchemaVersion != 0 {
		t.Errorf("Marshal mutated caller SchemaVersion to %d", a.SchemaVersion)
	}
	// ...but the bytes must carry the defaulted version.
	if !strings.Contains(string(b), `"schema_version": 1`) {
		t.Errorf("marshaled bytes missing defaulted schema_version:\n%s", b)
	}
}

func TestArtifactMarshalNil(t *testing.T) {
	if _, err := Marshal(nil); err == nil {
		t.Fatal("Marshal(nil) should error")
	}
}

func TestArtifactUnmarshalParseError(t *testing.T) {
	if _, err := Unmarshal([]byte("{not json")); err == nil {
		t.Fatal("Unmarshal of corrupt JSON should error, not return zero-value")
	}
}

// TestArtifactGolden pins the exact serialized shape — including schema_version,
// the JSON tags, omitempty behavior, and the trailing newline — so a tag rename
// or escaping change is caught as a wire-format break.
func TestArtifactGolden(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := &Artifact{
		ID:        "g1",
		Kind:      KindMatrix,
		CreatedAt: at,
		Claims: []Claim{
			{
				ID:    "c1",
				Field: "f1",
				Evidence: Evidence{
					Value:    "v1",
					Verifier: "web.url_resolves",
					Status:   StatusPass,
				},
			},
		},
	}
	got, err := Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{
  "schema_version": 1,
  "id": "g1",
  "kind": "matrix",
  "created_at": "2026-01-02T03:04:05Z",
  "claims": [
    {
      "id": "c1",
      "field": "f1",
      "evidence": {
        "value": "v1",
        "retrieved_at": "0001-01-01T00:00:00Z",
        "verifier": "web.url_resolves",
        "status": "pass"
      }
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
