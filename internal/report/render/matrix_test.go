package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nilcore/internal/report"
)

// swarmModel builds a SwarmReport with two artifacts and an overlapping field set so
// the matrix has a real column union to sort. zeb's "alpha" field sorts before "rev",
// proving deterministic column order independent of trace order.
func swarmModel() *report.SwarmReport {
	base := &report.ReportModel{
		Run:           "swarm-1",
		GeneratedAt:   time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		ChainVerified: true,
		FinalPass:     false,
		ClaimTraces: []report.ClaimTrace{
			{ArtifactID: "art-1", ClaimID: "c1", Field: "rev", Value: "42",
				Source:   report.SourceRef{Locator: "https://example.com/a", Resolved: true},
				Verifier: "finance.sec_fact", Status: "pass", Detail: "ok"},
			{ArtifactID: "art-1", ClaimID: "c2", Field: "alpha", Value: "x",
				Source:   report.SourceRef{Locator: "https://example.com/b", Resolved: false},
				Verifier: "generic", Status: "fail", Detail: "no"},
			{ArtifactID: "art-2", ClaimID: "c3", Field: "rev", Value: "99",
				Source:   report.SourceRef{Locator: "https://example.com/c", Resolved: true},
				Verifier: "finance.sec_fact", Status: "stale", Detail: "old"},
		},
	}
	return &report.SwarmReport{
		Base: base,
		Swarm: report.SwarmDimension{
			Checked: 3, Passed: 1, Failed: 1, RetryPass: 0, Remaining: 2, Pass: 1,
			FinalCleanPass: false,
		},
	}
}

// TestRenderMatrix is the named test (Verify line) for the matrix renderer + the JSON
// deliverable: deterministic columns, escape of a <script> payload, redaction of an
// api_key= query param, no green cell over a non-pass status, and the JSON helper
// emitting no secret.
func TestRenderMatrix(t *testing.T) {
	t.Run("DeterministicColumns", testMatrixDeterministicColumns)
	t.Run("EscapesScript", testMatrixEscapesScript)
	t.Run("RedactsApiKey", testMatrixRedactsApiKey)
	t.Run("NoGreenOverNonPass", testMatrixNoGreenOverNonPass)
	t.Run("OffStyleNoANSI", testMatrixOffStyleNoANSI)
	t.Run("JSONEmitsNoSecret", testJSONEmitsNoSecret)
	t.Run("JSONIsRedactedProjection", testJSONIsRedactedProjection)
}

// testMatrixDeterministicColumns: the column header is the sorted field union
// ("alpha" before "rev"), regardless of the order traces appear in the model.
func testMatrixDeterministicColumns(t *testing.T) {
	out := RenderMatrix(swarmModel(), offStyle())
	ia := strings.Index(out, "alpha")
	ir := strings.Index(out, "rev")
	if ia < 0 || ir < 0 {
		t.Fatalf("matrix missing field columns:\n%s", out)
	}
	if ia > ir {
		t.Fatalf("columns not sorted (alpha must precede rev):\n%s", out)
	}
	// Determinism: the same model renders byte-identically.
	if RenderMatrix(swarmModel(), offStyle()) != out {
		t.Fatalf("matrix render is not deterministic")
	}
}

// testMatrixEscapesScript: a <script> payload in a claim Value is neutralized — the
// literal "<script>" must not survive into the cell (I7).
func testMatrixEscapesScript(t *testing.T) {
	m := swarmModel()
	m.Base.ClaimTraces[0].Value = `<script>alert(1)</script>`
	out := RenderMatrix(m, offStyle())
	if strings.Contains(out, "<script>") {
		t.Fatalf("matrix contains a live <script> payload:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("matrix did not escape the script payload:\n%s", out)
	}
}

// testMatrixRedactsApiKey: an api_key= query param in a SourceURL is stripped from the
// rendered footnote (I3) — neither a short nor a long value leaks.
func testMatrixRedactsApiKey(t *testing.T) {
	m := swarmModel()
	m.Base.ClaimTraces[0].Source.Locator = "https://api.example.com/v1?api_key=secret&q=rev"
	out := RenderMatrix(m, offStyle())
	if strings.Contains(out, "secret") {
		t.Fatalf("matrix leaked the api_key value:\n%s", out)
	}
	if strings.Contains(out, "api_key") {
		t.Fatalf("matrix leaked the api_key param name:\n%s", out)
	}
	// The non-secret part of the URL survives so the source is still cited.
	if !strings.Contains(out, "api.example.com") {
		t.Fatalf("matrix dropped the whole source instead of just the key:\n%s", out)
	}
}

// testMatrixNoGreenOverNonPass: with an ON style, a non-pass cell (fail/stale) is
// NEVER wrapped in the Success(32) escape — only a pass cell is green (I2).
func testMatrixNoGreenOverNonPass(t *testing.T) {
	st := onStyle(t)
	out := RenderMatrix(swarmModel(), st)
	// Find the failing cell text and assert no Success escape immediately wraps it.
	// Simpler structural assertion: a pass cell exists (green present) and the fail
	// status text "FAIL" is present wrapped in Danger(31), never Success(32).
	if !strings.Contains(out, "\033[32m") {
		t.Fatalf("expected a Success(32) escape for the pass cell:\n%q", out)
	}
	// The fail row's cell must be Danger-painted; assert the substring "FAIL" never
	// directly follows a Success open code.
	for _, frag := range strings.Split(out, "\033[32m") {
		// frag is the text painted green up to the next reset; it must not be a FAIL
		// or STALE cell.
		head := frag
		if i := strings.Index(head, "\033[0m"); i >= 0 {
			head = head[:i]
		}
		if strings.Contains(head, "FAIL") || strings.Contains(head, "STALE") {
			t.Fatalf("a non-pass status was painted green:\n%q", out)
		}
	}
}

// testMatrixOffStyleNoANSI: an off Style yields zero escape sequences (the CI/pipe
// degrade guarantee, I6).
func testMatrixOffStyleNoANSI(t *testing.T) {
	out := RenderMatrix(swarmModel(), offStyle())
	if strings.Contains(out, "\033[") {
		t.Fatalf("off-Style matrix contains an ANSI escape:\n%s", out)
	}
}

// testJSONEmitsNoSecret: the json deliverable over a ?api_key=secret SourceURL emits
// NO secret — the redacted projection strips the key param before marshaling (I3).
func testJSONEmitsNoSecret(t *testing.T) {
	m := swarmModel()
	m.Base.ClaimTraces[0].Source.Locator = "https://api.example.com/v1?api_key=secret"
	m.Base.ClaimTraces[1].Value = "ghp_abcdefghijklmnop1234567890"
	data, err := MarshalRedacted(m)
	if err != nil {
		t.Fatalf("MarshalRedacted: %v", err)
	}
	js := string(data)
	if strings.Contains(js, "api_key=secret") || strings.Contains(js, "\"secret\"") || strings.Contains(js, "api_key") {
		t.Fatalf("json leaked the api_key:\n%s", js)
	}
	if strings.Contains(js, "ghp_abcdefghijklmnop1234567890") {
		t.Fatalf("json leaked a github token:\n%s", js)
	}
	// Sanity: it is still valid JSON and carries the redacted source.
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("json deliverable is not valid JSON: %v\n%s", err, js)
	}
}

// testJSONIsRedactedProjection: MarshalRedacted emits the projection's shape (run,
// chain_verified, swarm counts, claims) — NOT the raw ReportModel — and never the raw
// SourceURL field name, proving it is a distinct redacted type.
func testJSONIsRedactedProjection(t *testing.T) {
	data, err := MarshalRedacted(swarmModel())
	if err != nil {
		t.Fatalf("MarshalRedacted: %v", err)
	}
	js := string(data)
	for _, want := range []string{`"run"`, `"chain_verified"`, `"swarm"`, `"claims"`, `"status"`} {
		if !strings.Contains(js, want) {
			t.Errorf("json projection missing %s:\n%s", want, js)
		}
	}
	// The raw model's exported field names (Go-cased) must NOT appear — the JSON is the
	// redacted projection, not json.Marshal(ReportModel).
	for _, bad := range []string{"SourceURL", "ClaimTraces", "GeneratedAt"} {
		if strings.Contains(js, bad) {
			t.Errorf("json leaked a raw-model field name %q (marshaled the raw model?):\n%s", bad, js)
		}
	}
}
