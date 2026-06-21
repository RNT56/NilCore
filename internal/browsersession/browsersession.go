// Package browsersession is the host-side handle to a persistent, in-sandbox
// browser the agent drives across many turns (Phase 14, Pillar 1). NilCore's
// sandbox runs each box.Exec as a fresh, isolated container, so there is no
// host-held CDP connection and no process that survives between calls; instead the
// whole browser session is ONE long-lived `nilcore-browser --serve` Exec (the
// daemon) and the host exchanges one Act ⇄ one Observation with it over a
// FILE-QUEUE on the shared /work mount (cmd/tools/nilcore-browser/serve.go).
//
// The Session adds the host-side guarantees that must NOT live in the sandbox:
//   - version-stamped ref validation — an Act that references an element from a
//     stale snapshot fails closed rather than acting on a re-rendered node;
//   - {{secret:NAME}} substitution — a typed secret is resolved from the host
//     SecretStore at send time and NEVER placed in the model context or the event
//     log (I3); the model only ever sees the placeholder.
//
// The browser run itself is CI-only (no Chromium in unit tests); the Session logic
// (ref checks, secret substitution, act/observation marshaling, error handling) is
// unit-tested hermetically against a fake transport.
package browsersession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"

	"nilcore/internal/browserwire"
	"nilcore/internal/sandbox"
)

// SecretResolver looks up a named secret host-side (e.g. from secrets.SecretStore).
// It returns ok=false for an unknown name. The resolved value is injected into a
// type action just before it reaches the sandbox; it is never returned to the
// model or written to the log (I3).
type SecretResolver func(name string) (value string, ok bool)

// Options configure a session launch.
type Options struct {
	Driver     string         // in-sandbox driver command (default "nilcore-browser")
	InitialURL string         // first navigation (optional)
	Secrets    SecretResolver // nil ⇒ any {{secret:…}} placeholder is an error (fail closed)
}

// Session is a live, host-driven browser. It is NOT safe for concurrent use — the
// agent loop drives it one act at a time.
type Session struct {
	tr      transport
	secrets SecretResolver
	latest  browserwire.Observation
	closed  bool
}

const defaultDriver = "nilcore-browser"

// secretRe matches a {{secret:NAME}} placeholder; NAME is a conservative identifier
// so a placeholder can never smuggle structure.
var secretRe = regexp.MustCompile(`\{\{secret:([A-Za-z0-9_.-]+)\}\}`)

// Launch starts a browser session inside box and returns a ready Session plus the
// first observation. It launches the daemon, waits for it to come up, navigates to
// any InitialURL, and takes the first snapshot.
func Launch(ctx context.Context, box sandbox.Sandbox, opt Options) (*Session, browserwire.Observation, error) {
	if box == nil {
		return nil, browserwire.Observation{}, fmt.Errorf("browsersession: no sandbox (refusing a host-side browser)")
	}
	id, err := newID()
	if err != nil {
		return nil, browserwire.Observation{}, err
	}
	driver := opt.Driver
	if driver == "" {
		driver = defaultDriver
	}
	tr, err := newFileTransport(ctx, box, driver, id, opt.InitialURL)
	if err != nil {
		return nil, browserwire.Observation{}, err
	}
	s := &Session{tr: tr, secrets: opt.Secrets}
	if err := tr.waitReady(ctx); err != nil {
		_ = tr.close()
		return nil, browserwire.Observation{}, fmt.Errorf("browser daemon never became ready: %w", err)
	}
	obs, err := s.Observe(ctx)
	if err != nil {
		_ = s.Close()
		return nil, browserwire.Observation{}, err
	}
	return s, obs, nil
}

// newSession builds a Session over a given transport — the seam unit tests use to
// drive the Session logic without a real daemon.
func newSession(tr transport, secrets SecretResolver) *Session {
	return &Session{tr: tr, secrets: secrets}
}

// Observe re-snapshots without mutating the page.
func (s *Session) Observe(ctx context.Context) (browserwire.Observation, error) {
	return s.do(ctx, browserwire.Act{Op: browserwire.OpObserve})
}

// Act validates, resolves secrets in, and executes one action, returning the
// resulting observation. A ref that is not in the latest snapshot, or an
// unresolvable secret placeholder, fails closed before anything reaches the
// sandbox.
func (s *Session) Act(ctx context.Context, a browserwire.Act) (browserwire.Observation, error) {
	if s.closed {
		return browserwire.Observation{}, fmt.Errorf("session is closed")
	}
	if err := s.validateRef(a); err != nil {
		return browserwire.Observation{}, err
	}
	sub, err := s.substituteSecrets(a)
	if err != nil {
		return browserwire.Observation{}, err
	}
	return s.do(ctx, sub)
}

// Latest returns the most recent observation (the model's current view).
func (s *Session) Latest() browserwire.Observation { return s.latest }

// Close shuts the daemon down gracefully (a close act) then releases the transport.
func (s *Session) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.tr.close()
}

// do sends one act over the transport and records the resulting observation as the
// new latest view. A driver-reported Error is surfaced to the caller (fail closed)
// but the observation (the post-failure page state) is still recorded so the agent
// can see what the failure left behind.
func (s *Session) do(ctx context.Context, a browserwire.Act) (browserwire.Observation, error) {
	resp, err := s.tr.send(ctx, browserwire.SessionRequest{Act: a})
	if err != nil {
		return browserwire.Observation{}, err
	}
	s.latest = resp.Observation
	if resp.Error != "" {
		return resp.Observation, fmt.Errorf("browser act %q failed: %s", a.Op, resp.Error)
	}
	return resp.Observation, nil
}

// validateRef rejects a ref-based act whose ref is absent from the latest snapshot
// (the page changed) — the host-side half of the version-stamped staleness defense.
func (s *Session) validateRef(a browserwire.Act) error {
	if a.Ref <= 0 {
		return nil // selector-based or non-ref act
	}
	for _, r := range s.latest.Refs {
		if r.ID == a.Ref {
			return nil
		}
	}
	return fmt.Errorf("ref %d is not in the current snapshot (version %d) — re-observe before acting (stale-ref guard)", a.Ref, s.latest.Version)
}

// substituteSecrets replaces {{secret:NAME}} placeholders in a type action's text
// with the real secret from the resolver. The substitution is on a COPY — the
// caller keeps the placeholder form for logging — so the secret never enters the
// model context or the event log (I3). An unresolved/absent secret is an error.
func (s *Session) substituteSecrets(a browserwire.Act) (browserwire.Act, error) {
	if a.Op != browserwire.OpType || !secretRe.MatchString(a.Text) {
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
		return browserwire.Act{}, fmt.Errorf("unresolved secret %q (no value in the SecretStore) — refusing to type a placeholder", missing)
	}
	a.Text = out
	return a, nil
}

// newID returns a short random session id used for the control-dir path.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
