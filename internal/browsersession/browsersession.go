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
//     log (I3); the model only ever sees the placeholder. Every returned observation
//     is then scrubbed host-side of any typed secret value, so a secret entered into a
//     non-password field cannot reflow back to the model as plaintext on the next
//     snapshot (the in-sandbox snapshot masks only password-type inputs).
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
	"strings"

	"nilcore/internal/browserwire"
	"nilcore/internal/sandbox"
)

// SecretResolver looks up a named secret host-side (e.g. from secrets.SecretStore).
// It returns ok=false for an unknown name. The resolved value is injected into a
// type action just before it reaches the sandbox; it is never returned to the
// model or written to the log (I3).
type SecretResolver func(name string) (value string, ok bool)

// AllowlistResolver wraps inner so it resolves ONLY names on the operator-declared,
// per-task allowlist. A name absent from allow returns ok=false, so substituteSecrets
// fails closed ("refusing to type a placeholder") instead of typing it. An empty (or
// nil) allow resolves NOTHING — the fail-closed default: an operator must explicitly
// declare which secret names a browse task may type. This is the exfiltration fence:
// without it, an env-first resolver would hand the model ANY process env var the moment
// it typed the placeholder (e.g. a hostile/injected model typing {{secret:ANTHROPIC_API_KEY}}
// into a page). inner may be nil (treated as resolve-nothing).
func AllowlistResolver(allow []string, inner SecretResolver) SecretResolver {
	set := make(map[string]struct{}, len(allow))
	for _, n := range allow {
		if n = strings.TrimSpace(n); n != "" {
			set[n] = struct{}{}
		}
	}
	return func(name string) (string, bool) {
		if _, ok := set[name]; !ok {
			return "", false
		}
		if inner == nil {
			return "", false
		}
		return inner(name)
	}
}

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

	// typedSecrets is the set of RESOLVED secret VALUES this session has typed into the
	// page. Every observation returned to the model is scrubbed of any occurrence of one
	// of these values (I3): a {{secret:NAME}} the model typed via OpType reflows back as
	// plaintext on the next auto-snapshot (Observation.Text + Ref.Name/Value) if the field
	// is not a password input — the in-sandbox snapshot only masks password-type fields.
	// This host-side redaction is field-type-independent and closes that reflow.
	typedSecrets map[string]struct{}
}

// secretSentinel is the fixed replacement for a scrubbed secret value in a returned
// observation. It is the only thing the model ever sees where a typed secret would be.
const secretSentinel = "«secret»"

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
	// Host-side secret scrub (I3): redact any typed secret value that reflowed into the
	// observation before it becomes the model's view or is recorded as latest.
	obs := s.scrubObservation(resp.Observation)
	s.latest = obs
	if resp.Error != "" {
		return obs, fmt.Errorf("browser act %q failed: %s", a.Op, resp.Error)
	}
	return obs, nil
}

// validateRef rejects a ref-based act whose ref is absent from the latest snapshot,
// OR whose stamped Version differs from the latest observation's — the host-side,
// version-stamped staleness defense. The Version compare is the Cancel→Delete fix
// (ROADMAP §3 Pillar 2): a re-render that swaps an element but reuses the same
// positional id would pass a pure membership check, but its refs carry the OLD
// snapshot version, so a model still referencing them fails closed here rather than
// acting on a different node. The model must re-observe after any mutation.
func (s *Session) validateRef(a browserwire.Act) error {
	if a.Ref <= 0 {
		return nil // selector-based or non-ref act
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
		s.rememberSecret(v) // scrub this value from every future observation (I3)
		return v
	})
	if missing != "" {
		return browserwire.Act{}, fmt.Errorf("unresolved secret %q (no value in the SecretStore) — refusing to type a placeholder", missing)
	}
	a.Text = out
	return a, nil
}

// rememberSecret records a resolved secret value so scrubObservation can redact it from
// every subsequent observation. A trivially short value is ignored — scrubbing a 1–2
// char value would corrupt unrelated page text for no security gain (a real secret is
// never that short); empty values are likewise skipped.
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
// observation (URL, Text, Title, Console, and every Ref Name/Value) with secretSentinel,
// before the observation is recorded as latest and returned to the model. This is the
// host-side backstop for secret reflow (I3): it is independent of the field's input type,
// so a secret typed into a text/textarea/API-key/TOTP field — which the in-sandbox
// snapshot does NOT mask (only password inputs) — never reaches the model as plaintext.
func (s *Session) scrubObservation(o browserwire.Observation) browserwire.Observation {
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
	// URL too (I3): a {{secret:}} value that reflows into a GET-form/query-param URL
	// (e.g. ?token=<secret>) would otherwise reach the model via renderObservation's
	// "url: %s" line AND the append-only event log as plaintext.
	o.URL = scrub(o.URL)
	if len(o.Console) > 0 {
		cs := make([]string, len(o.Console))
		for i, c := range o.Console {
			cs[i] = scrub(c)
		}
		o.Console = cs
	}
	if len(o.Refs) > 0 {
		rs := make([]browserwire.Ref, len(o.Refs))
		for i, r := range o.Refs {
			r.Name = scrub(r.Name)
			r.Value = scrub(r.Value)
			rs[i] = r
		}
		o.Refs = rs
	}
	return o
}

// newID returns a short random session id used for the control-dir path.
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
