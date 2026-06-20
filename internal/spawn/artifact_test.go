package spawn

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestResultArtifactField proves the additive Artifact field: a nil Artifact is
// byte-identical to the pre-change shape (omitempty, no "artifact" key), a
// non-nil ArtifactSummary rides verbatim through Spawn and a DAG wave, the
// cancelled/panicking paths leave it nil, ClaimStatus carries only the trusted
// surface, and spawn stays an artifact-free leaf.
func TestResultArtifactField(t *testing.T) {
	t.Run("nil_artifact_marshals_byte_identical", func(t *testing.T) {
		// The golden shape is exactly the pre-change Result with no extra key.
		// With Artifact nil and `json:",omitempty"`, the serialized bytes must
		// contain no "artifact" key at all.
		r := Result{ID: "x", Summary: "s", Branch: "b", Passed: true, State: StatePassed}
		got, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		const want = `{"ID":"x","Summary":"s","Branch":"b","Passed":true,"State":"passed","Err":null}`
		if string(got) != want {
			t.Fatalf("nil-Artifact Result not byte-identical:\n got %s\nwant %s", got, want)
		}
		if strings.Contains(string(got), "artifact") || strings.Contains(string(got), "Artifact") {
			t.Errorf("nil Artifact must not appear in JSON, got %s", got)
		}
	})

	t.Run("non_nil_preserved_through_spawn", func(t *testing.T) {
		want := &ArtifactSummary{
			ID:    "art-1",
			Kind:  "research-dossier",
			Green: true,
			Claims: []ClaimStatus{
				{ID: "company-041-revenue", Field: "revenue_fy2024", Status: "pass"},
				{ID: "company-041-margin", Field: "margin_fy2024", Status: "fail"},
			},
		}
		s := &Spawner{
			MaxConcurrent: 2,
			Run: func(_ context.Context, st Subtask) Result {
				if st.ID == "boom" {
					panic("explode")
				}
				return Result{ID: st.ID, Passed: true, Artifact: want}
			},
		}
		res := s.Spawn(context.Background(), []Subtask{
			{ID: "a"}, {ID: "boom"},
		})
		if !reflect.DeepEqual(res[0].Artifact, want) {
			t.Errorf("artifact mutated/dropped through Spawn:\n got %+v\nwant %+v", res[0].Artifact, want)
		}
		// A panicking subworker is isolated; its Result.Artifact stays nil.
		if res[1].Artifact != nil {
			t.Errorf("panicking subtask Artifact = %+v, want nil", res[1].Artifact)
		}
	})

	t.Run("non_nil_preserved_through_dag_wave", func(t *testing.T) {
		want := &ArtifactSummary{ID: "art-2", Kind: "matrix", Green: false,
			Claims: []ClaimStatus{{ID: "c1", Field: "f1", Status: "stale"}}}
		d := &DAGScheduler{
			MaxConcurrent: 2,
			RunSub: func(_ context.Context, st Subtask) Result {
				return Result{ID: st.ID, Passed: true, Artifact: want}
			},
		}
		// dependent on a passing node so the DAG actually releases a second wave.
		res := d.Run(context.Background(), []Subtask{
			{ID: "base"},
			{ID: "dep", DependsOn: []string{"base"}},
		})
		if !reflect.DeepEqual(res["dep"].Artifact, want) {
			t.Errorf("artifact mutated/dropped through DAG wave:\n got %+v\nwant %+v", res["dep"].Artifact, want)
		}
	})

	t.Run("skipped_subtask_artifact_nil", func(t *testing.T) {
		// A cancelled ctx makes Spawn record terminal Skipped Results for the
		// not-yet-launched subtasks WITHOUT calling Run — so those carry no
		// Artifact. (The dispatcher's select may still let one slot-holder run,
		// so we assert the invariant on the genuinely-skipped tasks.)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &Spawner{
			MaxConcurrent: 1,
			Run: func(_ context.Context, st Subtask) Result {
				return Result{ID: st.ID, Passed: true, Artifact: &ArtifactSummary{ID: "should-not-appear"}}
			},
		}
		res := s.Spawn(ctx, []Subtask{{ID: "a"}, {ID: "b"}})
		skipped := 0
		for _, r := range res {
			if r.State == StateSkipped {
				skipped++
				if r.Artifact != nil {
					t.Errorf("skipped subtask %s Artifact = %+v, want nil", r.ID, r.Artifact)
				}
			}
		}
		if skipped == 0 {
			t.Fatal("expected at least one skipped subtask under a cancelled ctx")
		}
	})

	t.Run("claim_status_trusted_surface_only", func(t *testing.T) {
		// Guard against a future edit smuggling Value/SourceURL onto the trusted
		// projection. ClaimStatus must expose exactly ID/Field/Status — all string.
		typ := reflect.TypeOf(ClaimStatus{})
		want := []string{"ID", "Field", "Status"}
		if typ.NumField() != len(want) {
			t.Fatalf("ClaimStatus has %d fields, want exactly %d (ID/Field/Status)", typ.NumField(), len(want))
		}
		for i, name := range want {
			f := typ.Field(i)
			if f.Name != name {
				t.Errorf("ClaimStatus field %d = %q, want %q", i, f.Name, name)
			}
			if f.Type.Kind().String() != "string" {
				t.Errorf("ClaimStatus.%s kind = %s, want string", f.Name, f.Type.Kind())
			}
		}
		for _, banned := range []string{"Value", "SourceURL", "URL", "Detail"} {
			if _, ok := typ.FieldByName(banned); ok {
				t.Errorf("ClaimStatus must never carry untrusted field %q", banned)
			}
		}
	})
}
