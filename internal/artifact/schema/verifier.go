package schema

// verifier.go adapts the pure Schema walk to the verify.Verifier contract so the
// assembler (SW-T05) can drop it into a verify.Composite as the cheapest-first
// Named[0]: a shape defect turns the whole verdict red BEFORE any per-claim sandbox
// curl runs. The adapter lives in THIS subtree (not in internal/verify) on purpose —
// verify must stay a leaf importing only sandbox, and SchemaVerifier needs artifact +
// worktreefs, so housing it here keeps verify import-free of those and avoids a cycle.
//
// SchemaVerifier reaches NO network and needs NO sandbox box: it loads the artifact
// from the worktree (symlink-safe, O_NOFOLLOW) and runs the deterministic Validate
// walk. Fail-closed: a missing file, a corrupt file, or a Kind with no registered
// Schema all report Passed:false — the artifact cannot be certified well-formed, so it
// is refused, never waved through.
//
// Trust boundary (I7): Output and the emitted event carry ONLY harness-trusted fields
// — the Code, the field/claim NAME, and the bounded harness-authored Reason. They
// NEVER echo a model-authored Value/SourceURL/Statement, so an injection phrase
// smuggled into a claim value cannot ride out in the report or the event stream. Only
// the verifier-set verdict is trusted (I2).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/verify"
	"nilcore/internal/worktreefs"
)

// EventKind is the metadata-only event name emitted through EventSink when the schema
// check runs. The report projection (SW-T06) keys off this constant to decode a
// SchemaVerifyEvent from the log; the leaf itself never imports eventlog.
const EventKind = "schema_verify"

// DefectMeta is the redacted, harness-trusted projection of a Defect carried in the
// event: the Code and the field NAME only. It deliberately omits Reason and ClaimID —
// the event is metadata about WHAT shape rule fired, not a place to replay any
// (potentially model-influenced) string — keeping the event stream strictly metadata.
type DefectMeta struct {
	Code  Code
	Field string
}

// SchemaVerifyEvent is the per-artifact, metadata-only event the SchemaVerifier emits
// through EventSink. It records the artifact identity, its Kind, the trusted defect
// metadata, and the pass/fail verdict — never a model field. SW-T06 imports this
// package to decode it.
type SchemaVerifyEvent struct {
	ArtifactID string
	Kind       artifact.Kind
	Defects    []DefectMeta
	Passed     bool
}

// SchemaVerifier adapts the Schema walk to verify.Verifier. Reg is the catalog the
// artifact's Kind is resolved through (an unregistered Kind fails closed). RelPath is
// the path to the artifact JSON inside the worktree, opened with worktreefs
// (O_NOFOLLOW) so a symlink swapped in at the target is refused rather than followed.
// EventSink, when non-nil, receives one SchemaVerifyEvent per Check — the leaf never
// imports eventlog, so the cmd layer supplies the backed sink.
type SchemaVerifier struct {
	Reg       *Registry
	RelPath   string
	EventSink func(ev any)
}

// compile-time proof that SchemaVerifier satisfies verify.Verifier — this is what lets
// the assembler drop it into verify.Composite as Named[0].
var _ verify.Verifier = (*SchemaVerifier)(nil)

// errParse marks a corrupt-artifact load so Check can report a parse failure distinctly
// from a missing file. Both still yield Passed:false; the distinction only sharpens the
// Output reason.
var errParse = errors.New("artifact parse error")

// Check loads the artifact, resolves its Kind to a Schema, runs the deterministic
// Validate walk, and reports Passed iff there are ZERO defects. It NEVER returns a Go
// error for a verification outcome: a missing file, a parse error, an unschematized
// Kind, or a structural defect are all Passed:false reports. (There is no write-back
// and no I/O beyond the single read, so Check never has a non-verification error to
// surface.) It reaches no network and needs no sandbox.
func (v *SchemaVerifier) Check(ctx context.Context) (verify.Report, error) {
	_ = ctx // no blocking work: a pure read + in-memory walk. ctx kept for the contract.

	art, err := v.load()
	if err != nil {
		// Missing or corrupt file: fail closed. An artifact that cannot be read cannot
		// be certified well-formed. We emit no event (we have no trusted identity/Kind
		// to put in it) and report the distinct reason in Output.
		reasonStr := "artifact missing"
		if errors.Is(err, errParse) {
			reasonStr = "artifact parse error"
		}
		return verify.Report{Passed: false, Output: "schema: " + reasonStr}, nil
	}

	var (
		sch *Schema
		ok  bool
	)
	if v.Reg != nil {
		sch, ok = v.Reg.Lookup(art.Kind)
	}
	if !ok {
		// Unschematized Kind ⇒ fail-closed. Validate(nil) yields exactly one
		// CodeWrongKind defect; we route through it so the Output/event shape is uniform
		// with the defect path.
		sch = nil
	}

	// A nil sch (unschematized Kind) makes Validate return [CodeWrongKind] — fail-closed.
	defects := sch.Validate(art)
	passed := len(defects) == 0

	v.emit(art, defects, passed)

	return verify.Report{Passed: passed, Output: v.render(art, defects, passed)}, nil
}

// render builds the harness-trusted Output: a header line plus one bounded
// "Code: Field — Reason" line per defect, in Validate's deterministic order. Every
// component is harness-authored (Code, field NAME, harness Reason) — NO model field
// appears, so the Output is injection-safe (I7).
func (v *SchemaVerifier) render(a *artifact.Artifact, defects []Defect, passed bool) string {
	if passed {
		return fmt.Sprintf("schema OK: kind %q, %d claim(s), 0 defect(s)", string(a.Kind), len(a.Claims))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "schema FAIL: kind %q, %d defect(s)", string(a.Kind), len(defects))
	for _, d := range defects {
		// "Code: Field — Reason". Field and Reason are harness-authored; never a model
		// Value/SourceURL/Statement.
		field := d.Field
		if field == "" {
			field = "-"
		}
		b.WriteByte('\n')
		b.WriteString(string(d.Code))
		b.WriteString(": ")
		b.WriteString(field)
		if d.Reason != "" {
			b.WriteString(" — ")
			b.WriteString(d.Reason)
		}
	}
	return b.String()
}

// emit sends the one metadata-only SchemaVerifyEvent when an EventSink is wired. The
// payload carries only the artifact identity, Kind, the redacted DefectMeta list, and
// the verdict — never a model-authored field (I7).
func (v *SchemaVerifier) emit(a *artifact.Artifact, defects []Defect, passed bool) {
	if v.EventSink == nil {
		return
	}
	metas := make([]DefectMeta, 0, len(defects))
	for _, d := range defects {
		metas = append(metas, DefectMeta{Code: d.Code, Field: d.Field})
	}
	v.EventSink(SchemaVerifyEvent{
		ArtifactID: a.ID,
		Kind:       a.Kind,
		Defects:    metas,
		Passed:     passed,
	})
}

// load reads and parses the artifact at RelPath through worktreefs (O_NOFOLLOW), so a
// symlink at the target is refused. A missing file surfaces as os.ErrNotExist; a
// corrupt body as errParse — both lead to a fail-closed Passed:false, the distinction
// sharpening the Output. No sandbox is involved: the structural check is host-side,
// worktree-confined file I/O only (the §2 I4 sanctioned exception), and reaches no
// network.
func (v *SchemaVerifier) load() (*artifact.Artifact, error) {
	f, err := worktreefs.OpenNoFollow(v.RelPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	a, err := artifact.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errParse, err)
	}
	return a, nil
}
