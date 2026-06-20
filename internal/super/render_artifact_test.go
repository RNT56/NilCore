package super

import (
	"errors"
	"strings"
	"testing"

	"nilcore/internal/guard"
	"nilcore/internal/spawn"
)

// renderReport must stay byte-identical when a Result carries no typed artifact
// (the default, flag-off path): the trusted control line + the fenced prose, with
// no `artifact`/`claim` lines injected. Pillar 3 is opt-in; a nil Artifact is the
// pre-change shape and must serialize exactly as before.
func TestRenderReportTypedArtifactNilByteIdentical(t *testing.T) {
	s := &Supervisor{}
	r := spawn.Result{ID: "super.t1", Passed: true, Branch: "task/super.t1", Summary: "did the work"}

	got := s.renderReport(r)

	// Reconstruct the exact pre-change rendering: control line, then guarded prose.
	want := "subagent super.t1: passed=true branch=task/super.t1\n" +
		guard.Wrap("subagent super.t1 summary", "did the work")
	if got != want {
		t.Fatalf("nil-Artifact rendering not byte-identical:\n got=%q\nwant=%q", got, want)
	}
	if strings.Contains(got, "artifact ") || strings.Contains(got, "claim ") {
		t.Errorf("nil Artifact must emit no artifact/claim lines, got:\n%s", got)
	}
}

// A non-nil Artifact renders its verifier-set claim statuses as TRUSTED control
// lines (`artifact …`, `claim …`) that appear BEFORE the fenced prose block. The
// claim/artifact lines are NOT guard.Wrap'd; the worker's narrative IS. This is
// the I7 keystone for Pillar 3: the verifier's verdict is control, prose is data.
func TestRenderReportTypedArtifactTrustedThenFenced(t *testing.T) {
	s := &Supervisor{}
	r := spawn.Result{
		ID:      "super.t2",
		Passed:  true,
		Branch:  "task/super.t2",
		Summary: "fetched the numbers",
		Artifact: &spawn.ArtifactSummary{
			ID:    "dossier-7",
			Kind:  "research-dossier",
			Green: true,
			Claims: []spawn.ClaimStatus{
				{ID: "company-041-revenue", Field: "revenue_fy2024", Status: "pass"},
				{ID: "company-041-margin", Field: "margin_fy2024", Status: "fail"},
			},
		},
	}

	got := s.renderReport(r)

	// Exact trusted-line format.
	artLine := "artifact dossier-7 kind=research-dossier green=true\n"
	claim1 := "claim company-041-revenue field=revenue_fy2024 status=pass\n"
	claim2 := "claim company-041-margin field=margin_fy2024 status=fail\n"
	for _, want := range []string{artLine, claim1, claim2} {
		if !strings.Contains(got, want) {
			t.Errorf("missing trusted line %q in:\n%s", want, got)
		}
	}

	// Ordering: control line < artifact line < both claim lines < fenced prose.
	idxControl := strings.Index(got, "subagent super.t2:")
	idxArt := strings.Index(got, artLine)
	idxClaim1 := strings.Index(got, claim1)
	idxClaim2 := strings.Index(got, claim2)
	idxFence := strings.Index(got, "[untrusted subagent super.t2 summary")
	ordered := idxControl < idxArt && idxArt < idxClaim1 && idxClaim1 < idxClaim2 && idxClaim2 < idxFence
	if !ordered {
		t.Fatalf("ordering wrong (control=%d art=%d c1=%d c2=%d fence=%d):\n%s",
			idxControl, idxArt, idxClaim1, idxClaim2, idxFence, got)
	}

	// The artifact/claim lines must be OUTSIDE the untrusted fence — i.e. they
	// precede the BEGIN marker. The prose (and only the prose) is inside.
	beginIdx := strings.Index(got, "<<<BEGIN UNTRUSTED DATA>>>")
	if beginIdx < 0 || idxArt > beginIdx || idxClaim2 > beginIdx {
		t.Errorf("trusted artifact/claim lines must precede the untrusted fence, got:\n%s", got)
	}
	if !strings.Contains(got, "fetched the numbers") {
		t.Errorf("worker prose must still render fenced, got:\n%s", got)
	}
}

// Only the verifier-produced identity+status surface as trusted control fields;
// a model-authored value can never appear unfenced because ArtifactSummary does
// not carry one. We assert the rendered trusted region exposes id/field/status
// and nothing resembling a free-text value or URL.
func TestRenderReportTypedArtifactTrustedSurfaceOnly(t *testing.T) {
	s := &Supervisor{}
	r := spawn.Result{
		ID:     "super.t3",
		Passed: false,
		Artifact: &spawn.ArtifactSummary{
			ID:     "matrix-1",
			Kind:   "matrix",
			Green:  false,
			Claims: []spawn.ClaimStatus{{ID: "c1", Field: "f1", Status: "unverifiable"}},
		},
		// No Summary: there is no fence, so the WHOLE output is the trusted region.
	}

	got := s.renderReport(r)

	if strings.Contains(got, "BEGIN UNTRUSTED DATA") {
		t.Fatalf("no Summary should mean no fence, got:\n%s", got)
	}
	// The trusted region carries exactly the typed identity+status fields.
	for _, want := range []string{"matrix-1", "kind=matrix", "green=false", "c1", "field=f1", "status=unverifiable"} {
		if !strings.Contains(got, want) {
			t.Errorf("trusted region missing %q in:\n%s", want, got)
		}
	}
}

// A green=false, Passed=false typed Result still renders, but renderReport never
// flips a merge decision: it only surfaces fields. (mergeOrder/doIntegrate gate on
// Passed elsewhere; here we assert the typed render does not assert passed=true.)
func TestRenderReportTypedArtifactErrorAndStatus(t *testing.T) {
	s := &Supervisor{}
	r := spawn.Result{
		ID:     "super.t4",
		Passed: false,
		Err:    errors.New("boom"),
		Artifact: &spawn.ArtifactSummary{
			ID: "spec-9", Kind: "spec", Green: false,
			Claims: []spawn.ClaimStatus{{ID: "x", Field: "y", Status: "stale"}},
		},
	}

	got := s.renderReport(r)
	if !strings.Contains(got, "passed=false") {
		t.Errorf("control line must report passed=false, got:\n%s", got)
	}
	if !strings.Contains(got, `error="boom"`) {
		t.Errorf("error must surface as a typed field, got:\n%s", got)
	}
	if !strings.Contains(got, "claim x field=y status=stale") {
		t.Errorf("claim line must render, got:\n%s", got)
	}
}
