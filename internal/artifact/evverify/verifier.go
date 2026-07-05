package evverify

// verifier.go is the I2 keystone of the artifact spine: ArtifactVerifier is the
// component that PRODUCES an artifact's green. It implements verify.Verifier, so it
// plugs into verify.Composite as one more NamedVerifier after the build verifier —
// a red claim turns the whole verdict red, and an artifact check can never mask a
// red build (it runs after Named[0]).
//
// What Check does, in order:
//
//  1. Loads the artifact from the worktree via worktreefs (symlink-safe, O_NOFOLLOW).
//  2. For each claim, resolves Evidence.Verifier through the Registry and runs its
//     CheckFunc INSIDE the sandbox (I4) — never a host-side request.
//  3. Applies MaxAge staleness, which can only DEMOTE a pass to stale, never be the
//     sole basis to PASS (I2): a model-authored RetrievedAt cannot green a claim.
//  4. OVERWRITES every claim's Status with the verdict, so a worker that self-wrote
//     Status=pass is replaced by a real assertion (I2), and writes the artifact back
//     atomically.
//  5. Returns one verify.Report whose Passed is true ONLY when every claim is
//     StatusPass — the same predicate as Artifact.Green().
//
// Fail-closed everywhere: a missing file, a parse error, empty claims, an
// unregistered verifier-id, a nil sandbox, or a denied host all yield Passed:false.
// An artifact that asserts nothing is never trusted-green.
//
// Trust boundary (I7): the report table carries ONLY harness-trusted fields — the
// claim id, field label, verifier-id, the verdict, and the verifier's own bounded
// detail. It NEVER echoes the model-authored Value/Statement/SourceURL verbatim, so
// an injection phrase smuggled into a claim Value cannot reach the report unfenced.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
	"nilcore/internal/verify"
	"nilcore/internal/worktreefs"
)

// ArtifactVerifier resolves and runs every claim's check, overwrites the on-disk
// statuses with the verdicts, and reports Passed iff every claim is StatusPass.
//
// RelPath is the path to the artifact JSON inside the worktree the verifier owns. It
// is resolved with worktreefs (O_NOFOLLOW) so a symlink swapped in at the target is
// refused rather than followed. Root is the worktree/confinement boundary RelPath
// lives under; writeBack passes it to worktreefs.WriteConfined so the atomic write's
// no-follow parent-dir check is bounded to the worktree — an in-worktree symlinked
// component is still refused (I4), while the host's own stable ancestors above the
// root (e.g. a macOS `/var`→`/private/var` symlink in a temp path) are trusted rather
// than wrongly rejected. Wire Root to the same worktree root whose join produced
// RelPath; if a site has no root, the directory of RelPath is a safe fallback (it is
// at/below the boundary). Box is the sandbox every check reaches through (I4);
// a nil Box fails every network claim closed to Unverifiable and makes no host-side
// call. MaxAge gates staleness (0 disables it); EventSink, when non-nil, receives one
// Detail-only event per claim and one per artifact — the leaf never imports eventlog,
// so the cmd layer supplies the backed implementation.
type ArtifactVerifier struct {
	Box       sandbox.Sandbox
	Reg       *Registry
	RelPath   string
	Root      string
	MaxAge    time.Duration
	EventSink func(ev any)
}

// compile-time proof that ArtifactVerifier satisfies the verify.Verifier contract —
// this is what lets it drop into verify.Composite as a trailing NamedVerifier.
var _ verify.Verifier = (*ArtifactVerifier)(nil)

// ClaimVerifyEvent is the per-claim, Detail-only event the verifier emits through
// EventSink. It carries ONLY harness-trusted fields plus the claim's key-free
// SourceURL (provenance, already required key-free by I3) — never the model-authored
// Value or Statement, so nothing untrusted is echoed into the event stream (I7).
type ClaimVerifyEvent struct {
	ClaimID   string
	Field     string
	Verifier  string
	Status    artifact.Status
	SourceURL string
}

// ArtifactVerifyEvent is the per-artifact summary event: the counts of each verdict
// and the final green. It is metadata only.
type ArtifactVerifyEvent struct {
	ArtifactID   string
	Kind         artifact.Kind
	Green        bool
	Pass         int
	Fail         int
	Stale        int
	Unverifiable int
}

// errParse marks a corrupt-artifact load so Check can report a fail-closed parse
// failure distinctly from a missing file. Both still yield Passed:false; the
// distinction only sharpens the Output reason.
var errParse = errors.New("artifact parse error")

// Check loads the artifact, runs every claim's bound check in the box, overwrites
// the on-disk statuses, and returns the aggregate verdict. It never returns a Go
// error for a verification outcome: a missing file, parse error, empty claims, or a
// failing claim are all Passed:false reports. A Go error is reserved for an
// unexpected I/O failure writing the verdicts back.
func (v *ArtifactVerifier) Check(ctx context.Context) (verify.Report, error) {
	art, err := v.load()
	if err != nil {
		// Missing file or parse error: fail closed. The artifact cannot be
		// trusted-green if it cannot even be read.
		reason := "artifact missing"
		if errors.Is(err, errParse) {
			reason = "artifact parse error"
		}
		return verify.Report{Passed: false, Output: "evidence: " + reason}, nil
	}
	if len(art.Claims) == 0 {
		// An artifact that asserts nothing is never trusted-green (fail-closed).
		v.emitArtifact(art, false, counts{})
		return verify.Report{Passed: false, Output: "evidence: artifact has no claims (fail-closed)"}, nil
	}

	var cnt counts
	rows := make([]string, 0, len(art.Claims))
	for i := range art.Claims {
		st, det := v.verifyClaim(ctx, art.Claims[i])
		// Overwrite the model's self-written status with the real verdict (I2) and
		// record the harness-trusted detail tail.
		art.Claims[i].Evidence.Status = st
		art.Claims[i].Evidence.Detail = detail(det)
		cnt.add(st)
		rows = append(rows, v.row(art.Claims[i]))
		v.emitClaim(art.Claims[i])
	}

	green := art.Green()
	v.emitArtifact(art, green, cnt)

	// Persist the verdicts back atomically so downstream readers (typed worker
	// results, requeue, report) see the real statuses, not the worker's self-claim.
	if err := v.writeBack(art); err != nil {
		return verify.Report{}, fmt.Errorf("evidence: write back verdicts: %w", err)
	}

	out := v.summary(art, cnt) + "\n" + strings.Join(rows, "\n")
	return verify.Report{Passed: green, Output: out}, nil
}

// verifyClaim runs one claim's bound check and applies the staleness gate. The gate
// can only DEMOTE: a StatusPass whose source is too old becomes StatusStale, but no
// timestamp can ever turn a non-pass into a pass (I2). Resolve already maps a nil
// box, an unregistered id, or a denied host to Unverifiable, so this layer never
// reaches the network host-side.
func (v *ArtifactVerifier) verifyClaim(ctx context.Context, c artifact.Claim) (artifact.Status, string) {
	if v.Reg == nil {
		return artifact.StatusUnverifiable, "no registry bound"
	}
	st, det := v.Reg.Resolve(ctx, v.Box, c)
	if st == artifact.StatusPass && v.MaxAge > 0 {
		// Freshness can only demote. We never PASS a claim solely because its
		// RetrievedAt is recent; here a verifier ALREADY asserted the value true and
		// we additionally require the provenance to be fresh. A zero RetrievedAt (no
		// freshness evidence) on a value that demands freshness is treated as stale.
		ra := c.Evidence.RetrievedAt
		if ra.IsZero() || time.Since(ra) > v.MaxAge {
			reason := "stale: retrieved_at older than max-age"
			if ra.IsZero() {
				reason = "stale: no retrieved_at provenance"
			}
			if det != "" {
				reason = reason + "; " + det
			}
			return artifact.StatusStale, reason
		}
	}
	return st, det
}

// row renders one claim as a single harness-trusted table line. It echoes ONLY the
// claim id, field label, verdict, verifier-id, and the verifier's own detail tail —
// never the model-authored Value/Statement/SourceURL, so an injection phrase in a
// claim Value cannot appear unfenced in the report (I7).
func (v *ArtifactVerifier) row(c artifact.Claim) string {
	label := strings.ToUpper(string(c.Evidence.Status))
	line := fmt.Sprintf("%-12s %s (%s)", label, c.ID, c.Field)
	if vid := strings.TrimSpace(c.Evidence.Verifier); vid != "" {
		line += " via " + vid
	}
	if c.Evidence.Detail != "" {
		line += " — " + c.Evidence.Detail
	}
	return line
}

// summary renders the per-artifact verdict header from the trusted counts only.
func (v *ArtifactVerifier) summary(a *artifact.Artifact, c counts) string {
	verdict := "RED"
	if a.Green() {
		verdict = "GREEN"
	}
	return fmt.Sprintf("evidence %s: %d claim(s) — pass=%d fail=%d stale=%d unverifiable=%d",
		verdict, len(a.Claims), c.pass, c.fail, c.stale, c.unverifiable)
}

// load reads and parses the artifact at RelPath through worktreefs (O_NOFOLLOW), so
// a symlink at the target is refused. A missing file is reported as os.ErrNotExist; a
// corrupt body as errParse — both lead to a fail-closed Passed:false, but the
// distinction sharpens the Output. We resolve the file independently of Box so the
// nil-box fail-closed path still loads its claims to mark them Unverifiable.
func (v *ArtifactVerifier) load() (*artifact.Artifact, error) {
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

// writeBack persists the verdict-overwritten artifact atomically and symlink-safely
// to the SAME confined path it was read from. WriteConfined keeps the temp+rename +
// O_NOFOLLOW discipline; the path was already confined when load() opened it. The
// no-follow parent-dir check is bounded to Root (the worktree boundary RelPath lives
// under): an in-worktree symlinked component is still refused, while a legitimately
// symlinked host ancestor above Root (the macOS `/var` temp-dir case) is trusted. When
// Root is empty we fall back to RelPath's own directory — still a correct boundary,
// since that dir is at/below the worktree root.
func (v *ArtifactVerifier) writeBack(a *artifact.Artifact) error {
	data, err := artifact.Marshal(a)
	if err != nil {
		return err
	}
	root := v.Root
	if strings.TrimSpace(root) == "" {
		root = filepath.Dir(v.RelPath)
	}
	return worktreefs.WriteConfined(root, v.RelPath, data, 0)
}

// counts tallies the per-status verdicts for the artifact summary + event.
type counts struct{ pass, fail, stale, unverifiable int }

func (c *counts) add(st artifact.Status) {
	switch st {
	case artifact.StatusPass:
		c.pass++
	case artifact.StatusFail:
		c.fail++
	case artifact.StatusStale:
		c.stale++
	default:
		// unverified should never survive a verify pass; bucket it with unverifiable
		// so a non-pass is never silently dropped from the count.
		c.unverifiable++
	}
}

// emitClaim sends one Detail-only per-claim event when an EventSink is wired. The
// payload carries only harness-trusted fields plus the key-free SourceURL (I3/I7).
func (v *ArtifactVerifier) emitClaim(c artifact.Claim) {
	if v.EventSink == nil {
		return
	}
	v.EventSink(ClaimVerifyEvent{
		ClaimID:   c.ID,
		Field:     c.Field,
		Verifier:  c.Evidence.Verifier,
		Status:    c.Evidence.Status,
		SourceURL: c.Evidence.SourceURL,
	})
}

// emitArtifact sends the per-artifact summary event when an EventSink is wired.
func (v *ArtifactVerifier) emitArtifact(a *artifact.Artifact, green bool, c counts) {
	if v.EventSink == nil {
		return
	}
	v.EventSink(ArtifactVerifyEvent{
		ArtifactID:   a.ID,
		Kind:         a.Kind,
		Green:        green,
		Pass:         c.pass,
		Fail:         c.fail,
		Stale:        c.stale,
		Unverifiable: c.unverifiable,
	})
}
