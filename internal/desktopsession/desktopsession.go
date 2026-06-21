// Package desktopsession is the host-side handle to a persistent, in-sandbox
// virtual desktop the agent drives across many turns (Phase CU, Pillar 1). It is
// the sibling of internal/browsersession: the same one-long-lived-Exec + file-queue
// transport on the shared /work mount, the same version-stamped stale-ref guard and
// host-side {{secret}} substitution — but the daemon is `nilcore-desktop --serve`
// driving an Xvfb X11 desktop instead of a headless browser.
//
// The desktop run itself is CI-only (no X11 in unit tests); the Session logic (ref
// checks, secret substitution, act/observation marshaling, error handling) is
// unit-tested hermetically against a fake transport.
package desktopsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"

	"nilcore/internal/desktopwire"
	"nilcore/internal/sandbox"
)

// SecretResolver looks up a named secret host-side (e.g. from secrets.SecretStore).
// The resolved value is injected into a type action just before it reaches the
// sandbox; it is never returned to the model or written to the log (I3).
type SecretResolver func(name string) (value string, ok bool)

// Options configure a desktop session launch.
type Options struct {
	Driver  string         // in-sandbox driver command (default "nilcore-desktop")
	Native  bool           // hand the Rung-3 grounding sub-call to Path A (NILCORE_COMPUTER_NATIVE)
	Secrets SecretResolver // nil ⇒ any {{secret:…}} placeholder is an error (fail closed)
}

// Session is a live, host-driven desktop. NOT safe for concurrent use — the agent
// loop drives it one act at a time.
type Session struct {
	tr      transport
	secrets SecretResolver
	latest  desktopwire.Observation
	closed  bool
}

const defaultDriver = "nilcore-desktop"

var secretRe = regexp.MustCompile(`\{\{secret:([A-Za-z0-9_.-]+)\}\}`)

// Launch starts a desktop session inside box and returns a ready Session plus the
// first observation. It launches the daemon, waits for it to come up, and takes the
// first snapshot.
func Launch(ctx context.Context, box sandbox.Sandbox, opt Options) (*Session, desktopwire.Observation, error) {
	if box == nil {
		return nil, desktopwire.Observation{}, fmt.Errorf("desktopsession: no sandbox (refusing a host-side desktop)")
	}
	id, err := newID()
	if err != nil {
		return nil, desktopwire.Observation{}, err
	}
	driver := opt.Driver
	if driver == "" {
		driver = defaultDriver
	}
	tr, err := newFileTransport(ctx, box, driver, id, opt.Native)
	if err != nil {
		return nil, desktopwire.Observation{}, err
	}
	s := &Session{tr: tr, secrets: opt.Secrets}
	if err := tr.waitReady(ctx); err != nil {
		_ = tr.close()
		return nil, desktopwire.Observation{}, fmt.Errorf("desktop daemon never became ready: %w", err)
	}
	obs, err := s.Observe(ctx)
	if err != nil {
		_ = s.Close()
		return nil, desktopwire.Observation{}, err
	}
	return s, obs, nil
}

// newSession builds a Session over a given transport — the seam unit tests use.
func newSession(tr transport, secrets SecretResolver) *Session {
	return &Session{tr: tr, secrets: secrets}
}

// Observe re-snapshots without mutating the screen.
func (s *Session) Observe(ctx context.Context) (desktopwire.Observation, error) {
	return s.do(ctx, desktopwire.Act{Op: desktopwire.OpObserve})
}

// Act validates, resolves secrets in, and executes one action. A ref absent from
// the latest snapshot, or an unresolvable secret, fails closed before anything
// reaches the sandbox.
func (s *Session) Act(ctx context.Context, a desktopwire.Act) (desktopwire.Observation, error) {
	if s.closed {
		return desktopwire.Observation{}, fmt.Errorf("session is closed")
	}
	if err := s.validateRef(a); err != nil {
		return desktopwire.Observation{}, err
	}
	sub, err := s.substituteSecrets(a)
	if err != nil {
		return desktopwire.Observation{}, err
	}
	return s.do(ctx, sub)
}

// Latest returns the most recent observation.
func (s *Session) Latest() desktopwire.Observation { return s.latest }

// Close shuts the daemon down gracefully then releases the transport.
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.tr.close()
}

func (s *Session) do(ctx context.Context, a desktopwire.Act) (desktopwire.Observation, error) {
	resp, err := s.tr.send(ctx, desktopwire.SessionRequest{Act: a})
	if err != nil {
		return desktopwire.Observation{}, err
	}
	s.latest = resp.Observation
	if resp.Error != "" {
		return resp.Observation, fmt.Errorf("desktop act %q failed: %s", a.Op, resp.Error)
	}
	return resp.Observation, nil
}

// validateRef rejects a ref-based act whose ref is absent from the latest snapshot
// — the host-side half of the version-stamped staleness defense.
func (s *Session) validateRef(a desktopwire.Act) error {
	if a.Ref <= 0 {
		return nil // coordinate-based or non-ref act
	}
	for _, r := range s.latest.Refs {
		if r.ID == a.Ref {
			return nil
		}
	}
	return fmt.Errorf("ref %d is not in the current snapshot (version %d) — re-observe before acting (stale-ref guard)", a.Ref, s.latest.Version)
}

// substituteSecrets replaces {{secret:NAME}} placeholders in a type action's text
// with the real secret, on a COPY (the caller keeps the placeholder for logging),
// so the secret never enters the model context or the event log (I3).
func (s *Session) substituteSecrets(a desktopwire.Act) (desktopwire.Act, error) {
	if a.Op != desktopwire.OpType || !secretRe.MatchString(a.Text) {
		return a, nil
	}
	var missing string
	out := secretRe.ReplaceAllStringFunc(a.Text, func(m string) string {
		name := secretRe.FindStringSubmatch(m)[1]
		if s.secrets == nil {
			missing = name
			return m
		}
		v, ok := s.secrets(name)
		if !ok {
			missing = name
			return m
		}
		return v
	})
	if missing != "" {
		return desktopwire.Act{}, fmt.Errorf("unresolved secret %q (no value in the SecretStore) — refusing to type a placeholder", missing)
	}
	a.Text = out
	return a, nil
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
