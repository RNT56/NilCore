package render

import (
	"os"
	"strings"
	"testing"
	"time"

	"nilcore/internal/report"
	"nilcore/internal/termui"
)

// offStyle is the zero-value Style: no ANSI, the pipe/CI degrade path.
func offStyle() termui.Style { return termui.New(nil).Style() }

// onStyle returns a Style with colour ON, built hermetically by handing termui a
// pty master (a char device) so detectStyle reports a terminal — without needing
// the test process to actually be attached to one. NO_COLOR/TERM are cleared so
// the detection's other gates pass.
func onStyle(t *testing.T) termui.Style {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available for on-Style test: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	st := termui.New(ptmx).Style()
	if !termui.New(ptmx).Styled() {
		t.Skip("pty did not detect as a styled terminal on this host")
	}
	return st
}

// greenModel is a fully-passing, chain-verified model: every check passed, the
// one artifact is green with all-pass claims, so showGreen must be true.
func greenModel() *report.ReportModel {
	return &report.ReportModel{
		Run:           "run-green",
		GeneratedAt:   time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		ChainVerified: true,
		FinalPass:     true,
		Checks: []report.CheckResult{
			{Family: "verify", Task: "A", Passed: true, Output: "ok"},
			{Family: "artifact_verify", Task: "A", Passed: true},
		},
		Artifacts: []report.ArtifactView{{
			ID: "rep-1", Kind: "report", Title: "Q", Green: true,
			Claims: []report.ClaimRow{
				{ClaimID: "c1", Field: "rev", Value: "42", SourceURL: "https://example.com", Verifier: "finance.sec_fact", Status: "pass", Detail: "matched"},
			},
		}},
	}
}

// redModel has a failing claim and a failed check, but the chain is verified.
func redModel() *report.ReportModel {
	return &report.ReportModel{
		Run:           "run-red",
		GeneratedAt:   time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		ChainVerified: true,
		FinalPass:     false,
		Checks: []report.CheckResult{
			{Family: "verify", Task: "A", Passed: false, Output: "build failed"},
		},
		Artifacts: []report.ArtifactView{{
			ID: "rep-1", Kind: "matrix", Green: false,
			Claims: []report.ClaimRow{
				{ClaimID: "c1", Field: "rev", Value: "42", SourceURL: "https://example.com/x", Verifier: "finance.sec_fact", Status: "fail", Detail: "mismatch 41!=42"},
			},
		}},
		Retries: []report.RetryAttempt{
			{Task: "c1", ContinueFrom: "c1", Passed: false, Seq: 7},
			{Task: "c1", ContinueFrom: "c1", Passed: true, Seq: 9},
		},
	}
}

// brokenModel is chain-broken: ChainVerified false, FinalPass false. No green.
func brokenModel() *report.ReportModel {
	m := greenModel()
	m.Run = "run-broken"
	m.ChainVerified = false
	m.FinalPass = false
	return m
}

// TestRender is the named test (Verify line) — a suite over every acceptance
// bullet across the three renderers.
func TestRender(t *testing.T) {
	t.Run("TextOffStyleNoANSI", testTextOffStyleNoANSI)
	t.Run("TextOnStylePaintsRows", testTextOnStylePaintsRows)
	t.Run("BrokenChainBannerAllFormats", testBrokenChainBannerAllFormats)
	t.Run("MarkdownGreenOnlyWhenVerified", testMarkdownGreenOnlyWhenVerified)
	t.Run("FailedClaimFieldsAllFormats", testFailedClaimFieldsAllFormats)
	t.Run("HTMLNoScriptEscapes", testHTMLNoScriptEscapes)
	t.Run("SecretRedactionAllFormats", testSecretRedaction)
	t.Run("RetryHistoryAllFormats", testRetryHistoryAllFormats)
}

// testTextOffStyleNoANSI: an off Style yields zero escape sequences (the CI/pipe
// degrade guarantee).
func testTextOffStyleNoANSI(t *testing.T) {
	out := RenderText(redModel(), offStyle())
	if strings.Contains(out, "\033[") {
		t.Fatalf("off-Style text contains an ANSI escape:\n%s", out)
	}
	if !strings.Contains(out, redHeadline) {
		t.Fatalf("expected red headline in:\n%s", out)
	}
}

// testTextOnStylePaintsRows: an on Style wraps a passed row Success (green, code
// 32) and a failed row Danger (red, code 31).
func testTextOnStylePaintsRows(t *testing.T) {
	st := onStyle(t)
	green := RenderText(greenModel(), st)
	if !strings.Contains(green, "\033[32m") {
		t.Fatalf("on-Style green model missing Success(32) escape:\n%q", green)
	}
	red := RenderText(redModel(), st)
	if !strings.Contains(red, "\033[31m") {
		t.Fatalf("on-Style red model missing Danger(31) escape:\n%q", red)
	}
}

// testBrokenChainBannerAllFormats: a broken chain shows the RED banner and NO
// green final-pass headline in text, HTML, and markdown.
func testBrokenChainBannerAllFormats(t *testing.T) {
	m := brokenModel()
	outs := map[string]string{
		"text": RenderText(m, offStyle()),
		"html": RenderHTML(m),
		"md":   RenderMarkdown(m),
	}
	for name, out := range outs {
		if !strings.Contains(out, brokenChainBanner) {
			t.Errorf("%s: missing broken-chain banner:\n%s", name, out)
		}
		if strings.Contains(out, greenHeadline) {
			t.Errorf("%s: GREEN headline printed over a broken chain:\n%s", name, out)
		}
	}
}

// testMarkdownGreenOnlyWhenVerified: the markdown GREEN headline appears iff
// ChainVerified && all-pass — proving it is a verifier projection, not a
// citations-emitter (NON-GOAL guard).
func testMarkdownGreenOnlyWhenVerified(t *testing.T) {
	if got := RenderMarkdown(greenModel()); !strings.Contains(got, greenHeadline) {
		t.Fatalf("verified all-pass model should show GREEN headline:\n%s", got)
	}
	// Chain verified but a claim is non-pass ⇒ no green.
	if got := RenderMarkdown(redModel()); strings.Contains(got, greenHeadline) {
		t.Fatalf("model with a failing claim must NOT show GREEN headline:\n%s", got)
	}
	// Chain broken ⇒ no green even though claims would pass.
	if got := RenderMarkdown(brokenModel()); strings.Contains(got, greenHeadline) {
		t.Fatalf("broken-chain model must NOT show GREEN headline:\n%s", got)
	}
}

// testFailedClaimFieldsAllFormats: a failed ClaimRow renders ClaimID, Field,
// Value, SourceURL, Verifier and Status in all three formats.
func testFailedClaimFieldsAllFormats(t *testing.T) {
	m := redModel()
	r := m.Artifacts[0].Claims[0]
	for name, out := range map[string]string{
		"text": RenderText(m, offStyle()),
		"html": RenderHTML(m),
		"md":   RenderMarkdown(m),
	} {
		for _, want := range []string{r.ClaimID, r.Field, r.Value, r.SourceURL, r.Verifier, "FAIL"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: failed claim row missing %q:\n%s", name, want, out)
			}
		}
	}
}

// testHTMLNoScriptEscapes: the HTML has no <script>/external asset and a script
// payload in a Value is escaped (rendered inert).
func testHTMLNoScriptEscapes(t *testing.T) {
	m := redModel()
	m.Artifacts[0].Claims[0].Value = `<script>alert(1)</script>`
	out := RenderHTML(m)
	if strings.Contains(out, "<script>") {
		t.Fatalf("HTML contains a live <script> tag:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("HTML did not escape the script payload:\n%s", out)
	}
	for _, bad := range []string{"http://", "//cdn", "src=\"http", "@import", "url("} {
		if strings.Contains(out, bad) && bad != "http://" { // SourceURLs may legitimately contain http; only assert no asset refs
			t.Errorf("HTML references an external asset (%q):\n%s", bad, out)
		}
	}
}

// testSecretRedaction: a key shape seeded in a SourceURL and a Detail tail is
// masked in all three formats.
func testSecretRedaction(t *testing.T) {
	m := redModel()
	m.Artifacts[0].Claims[0].SourceURL = "https://api.example.com/v1?api_key=AKIAIOSFODNN7EXAMPLE"
	m.Artifacts[0].Claims[0].Detail = "fetched with ghp_abcdefghijklmnop1234567890"
	for name, out := range map[string]string{
		"text": RenderText(m, offStyle()),
		"html": RenderHTML(m),
		"md":   RenderMarkdown(m),
	} {
		if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("%s: leaked AWS key:\n%s", name, out)
		}
		if strings.Contains(out, "ghp_abcdefghijklmnop1234567890") {
			t.Errorf("%s: leaked GitHub token:\n%s", name, out)
		}
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("%s: expected a [redacted] marker:\n%s", name, out)
		}
	}
}

// testRetryHistoryAllFormats: each RetryAttempt renders as an ordered chain in
// all three formats.
func testRetryHistoryAllFormats(t *testing.T) {
	m := redModel()
	for name, out := range map[string]string{
		"text": RenderText(m, offStyle()),
		"html": RenderHTML(m),
		"md":   RenderMarkdown(m),
	} {
		if !strings.Contains(out, "Retry history") {
			t.Errorf("%s: missing retry history section:\n%s", name, out)
		}
		iFailed := strings.Index(out, "FAILED")
		iResolved := strings.Index(out, "RESOLVED")
		if iFailed < 0 || iResolved < 0 {
			t.Errorf("%s: retry outcomes missing:\n%s", name, out)
			continue
		}
		// Seq 7 (FAILED) is ordered before seq 9 (RESOLVED) in the model.
		if iFailed > iResolved {
			t.Errorf("%s: retry attempts not in Seq order (FAILED before RESOLVED):\n%s", name, out)
		}
	}
}
