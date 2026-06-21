package browseragent

import (
	"context"
	"encoding/json"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/browserwire"
)

func TestRecordFinding(t *testing.T) {
	root := t.TempDir()
	fs := &fakeSession{latest: browserwire.Observation{URL: "http://src.test/current"}}
	ft := &FindingTool{Root: root, ArtifactID: "facts", Title: "test goal", Sess: fs}
	ctx := context.Background()

	// Explicit url.
	if _, err := ft.Run(ctx, ".", json.RawMessage(`{"field":"version","value":"1.2.3","url":"http://a.test/rel"}`)); err != nil {
		t.Fatalf("record (explicit url): %v", err)
	}
	// Defaulted to the current page.
	if _, err := ft.Run(ctx, ".", json.RawMessage(`{"field":"name","value":"NilCore"}`)); err != nil {
		t.Fatalf("record (current page): %v", err)
	}

	a, err := artifact.Read(root, "facts")
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if a.Kind != artifact.KindDossier || len(a.Claims) != 2 {
		t.Fatalf("artifact kind=%q claims=%d, want dossier/2", a.Kind, len(a.Claims))
	}
	c0 := a.Claims[0]
	if c0.Field != "version" || c0.Evidence.Value != "1.2.3" || c0.Evidence.SourceURL != "http://a.test/rel" {
		t.Fatalf("claim0 = %+v", c0)
	}
	if c0.Evidence.Verifier != "ui.value_present" {
		t.Fatalf("claim0 verifier = %q, want ui.value_present", c0.Evidence.Verifier)
	}
	if c0.Evidence.Status != artifact.StatusUnverified {
		t.Fatalf("claim0 status = %q, want unverified (the verifier sets the real status, I2)", c0.Evidence.Status)
	}
	if a.Claims[1].Evidence.SourceURL != "http://src.test/current" {
		t.Fatalf("claim1 should default to the current page, got %q", a.Claims[1].Evidence.SourceURL)
	}
	// Stable, distinct claim ids.
	if a.Claims[0].ID == a.Claims[1].ID {
		t.Fatalf("claim ids must be distinct: %q == %q", a.Claims[0].ID, a.Claims[1].ID)
	}
}

func TestRecordFindingFailsClosed(t *testing.T) {
	ft := &FindingTool{Root: t.TempDir(), ArtifactID: "x", Sess: &fakeSession{}}
	ctx := context.Background()
	// Missing value.
	if _, err := ft.Run(ctx, ".", json.RawMessage(`{"field":"f"}`)); err == nil {
		t.Fatal("missing value must error")
	}
	// No url and no current page.
	if _, err := ft.Run(ctx, ".", json.RawMessage(`{"field":"f","value":"v"}`)); err == nil {
		t.Fatal("no url and no current page must error")
	}
}
