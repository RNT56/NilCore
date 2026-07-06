// Package desktopsession is the host-side handle to a persistent, in-sandbox
// virtual desktop the agent drives across many turns (Phase CU, Pillar 1). It is
// the sibling of internal/browsersession: the same one-long-lived-Exec + file-queue
// transport on the shared /work mount, the same version-stamped stale-ref guard,
// host-side {{secret}} substitution, and host-side secret-reflow scrub (a secret typed
// into a non-password field is redacted from every subsequent observation, I3) — but the
// daemon is `nilcore-desktop --serve` driving an Xvfb X11 desktop instead of a headless
// browser.
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
	"strings"

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

	// typedSecrets is the set of RESOLVED secret VALUES this session has typed onto the
	// desktop. Every observation returned to the model is scrubbed of any occurrence of
	// one of these values (I3): a {{secret:NAME}} the model typed via OpType reflows back
	// as plaintext on the next auto-snapshot (Observation.Text/Title + Ref.Name/Value —
	// cmd/tools/nilcore-desktop/a11y.go populates Ref.Value raw) if the field is not a
	// password input; the in-sandbox snapshot masks nothing here. This host-side redaction
	// is field-type-independent and closes that reflow — the sibling of
	// browsersession.Session.typedSecrets.
	typedSecrets map[string]struct{}
}

// secretSentinel is the fixed replacement for a scrubbed secret value in a returned
// observation. It is the only thing the model ever sees where a typed secret would be.
const secretSentinel = "«secret»"

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
	// Host-side secret scrub (I3): redact any typed secret value that reflowed into the
	// observation before it becomes the model's view (latest) or is returned. The driver's
	// a11y dump populates Ref.Value/Name and Text/Title raw, so a secret typed into a
	// non-password field would otherwise reach the model as plaintext.
	obs := s.scrubObservation(resp.Observation)
	s.latest = obs
	if resp.Error != "" {
		return obs, fmt.Errorf("desktop act %q failed: %s", a.Op, resp.Error)
	}
	return obs, nil
}

// validateRef rejects a ref-based act whose ref is absent from the latest snapshot,
// OR whose stamped Version differs from the latest observation's — the host-side,
// version-stamped staleness defense (mirrors browsersession.validateRef exactly). Both
// desktop drivers rebuild positional CV/AT-SPI ref ids from scratch every observe, so a
// re-render that swaps the element behind a reused id would pass a pure membership check;
// its refs carry the OLD snapshot version, so a model still referencing them fails closed
// here rather than actuating a DIFFERENT element on the real desktop (a swap attack). The
// model must re-observe after any mutation.
func (s *Session) validateRef(a desktopwire.Act) error {
	if a.Ref <= 0 {
		return nil // coordinate-based or non-ref act
	}
	for _, r := range s.latest.Refs {
		if r.ID == a.Ref {
			if r.Version != s.latest.Version {
				return fmt.Errorf("ref %d is stale (stamped version %d, current snapshot version %d) — re-observe before acting (stale-ref guard)", a.Ref, r.Version, s.latest.Version)
			}
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
		s.rememberSecret(v) // scrub this value from every future observation (I3)
		return v
	})
	if missing != "" {
		return desktopwire.Act{}, fmt.Errorf("unresolved secret %q (no value in the SecretStore) — refusing to type a placeholder", missing)
	}
	a.Text = out
	return a, nil
}

// rememberSecret records a resolved secret value so scrubObservation can redact it from
// every subsequent observation. A trivially short value is ignored — scrubbing a 1–2
// char value would corrupt unrelated screen text for no security gain (a real secret is
// never that short); empty values are likewise skipped. Mirrors browsersession.rememberSecret.
func (s *Session) rememberSecret(v string) {
	if len(v) < 3 {
		return
	}
	if s.typedSecrets == nil {
		s.typedSecrets = make(map[string]struct{})
	}
	s.typedSecrets[v] = struct{}{}
}

// scrubObservation replaces every occurrence of a previously-typed secret value in the
// observation (Text, Title, Console, and every Ref Name/Value) with secretSentinel,
// before the observation is recorded as latest and returned to the model. This is the
// host-side backstop for secret reflow (I3): it is independent of the field's input type,
// so a secret typed into a text/API-key/TOTP field — which the driver's a11y dump does
// NOT mask — never reaches the model as plaintext. Mirrors browsersession.scrubObservation.
func (s *Session) scrubObservation(o desktopwire.Observation) desktopwire.Observation {
	if len(s.typedSecrets) == 0 {
		return o
	}
	scrub := func(in string) string {
		if in == "" {
			return in
		}
		out := in
		for v := range s.typedSecrets {
			if strings.Contains(out, v) {
				out = strings.ReplaceAll(out, v, secretSentinel)
			}
		}
		return out
	}
	o.Text = scrub(o.Text)
	o.Title = scrub(o.Title)
	o.FocusedWindow = scrub(o.FocusedWindow)
	if len(o.Console) > 0 {
		cs := make([]string, len(o.Console))
		for i, c := range o.Console {
			cs[i] = scrub(c)
		}
		o.Console = cs
	}
	if len(o.Refs) > 0 {
		rs := make([]desktopwire.Ref, len(o.Refs))
		for i, r := range o.Refs {
			r.Name = scrub(r.Name)
			r.Value = scrub(r.Value)
			rs[i] = r
		}
		o.Refs = rs
	}
	return o
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
