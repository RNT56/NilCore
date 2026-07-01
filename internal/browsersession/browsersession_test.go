package browsersession

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/browserwire"
)

// fakeTransport records the acts it receives and replies with scripted
// observations, so the Session logic is testable with no daemon and no browser.
type fakeTransport struct {
	got   []browserwire.Act
	reply func(seq int, a browserwire.Act) browserwire.SessionResponse
}

func (f *fakeTransport) waitReady(context.Context) error { return nil }
func (f *fakeTransport) close() error                    { return nil }
func (f *fakeTransport) send(_ context.Context, req browserwire.SessionRequest) (browserwire.SessionResponse, error) {
	f.got = append(f.got, req.Act)
	if f.reply != nil {
		return f.reply(req.Seq, req.Act), nil
	}
	return browserwire.SessionResponse{Seq: req.Seq, Observation: browserwire.Observation{Version: 1}}, nil
}

func TestSecretSubstitution(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, func(name string) (string, bool) {
		if name == "login_password" {
			return "hunter2", true
		}
		return "", false
	})
	// Seed a snapshot so a ref would validate, though this act is selector-free.
	s.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Role: "textbox", Version: 1}}}

	if _, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpType, Ref: 1, Text: "pw={{secret:login_password}}"}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(ft.got) != 1 {
		t.Fatalf("expected 1 act sent, got %d", len(ft.got))
	}
	sent := ft.got[0].Text
	if sent != "pw=hunter2" {
		t.Fatalf("secret not substituted before send: %q", sent)
	}
	if strings.Contains(sent, "{{secret") {
		t.Fatalf("placeholder leaked into the sent act: %q", sent)
	}
}

func TestSecretMissingFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, func(string) (string, bool) { return "", false })
	s.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Version: 1}}}

	_, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpType, Ref: 1, Text: "{{secret:nope}}"})
	if err == nil {
		t.Fatal("expected an error for an unresolved secret")
	}
	if len(ft.got) != 0 {
		t.Fatal("an unresolved-secret act must never reach the transport (fail closed)")
	}
}

func TestSecretNilResolverFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	s.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Version: 1}}}
	if _, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpType, Ref: 1, Text: "{{secret:x}}"}); err == nil {
		t.Fatal("nil resolver + placeholder must fail closed")
	}
}

func TestStaleRefFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	s.latest = browserwire.Observation{Version: 5, Refs: []browserwire.Ref{{ID: 1, Version: 5}, {ID: 2, Version: 5}}}

	// Ref 9 is not in the current snapshot → must fail before reaching transport.
	_, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpClick, Ref: 9})
	if err == nil || !strings.Contains(err.Error(), "stale-ref") {
		t.Fatalf("expected a stale-ref guard error, got %v", err)
	}
	if len(ft.got) != 0 {
		t.Fatal("a stale-ref act must never reach the transport")
	}
}

// TestStaleRefVersionMismatchFailsClosed exercises the Cancel→Delete defense: a ref
// whose ID is still present in the latest snapshot but whose stamped Version is older
// (a re-render reused the positional id) must fail closed on the host side. A pure
// membership check would have let this through.
func TestStaleRefVersionMismatchFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	// The snapshot is at version 6 but ref 1 still carries version 5 (a node that was
	// swapped under the same id by a re-render the host has already advanced past).
	s.latest = browserwire.Observation{Version: 6, Refs: []browserwire.Ref{{ID: 1, Version: 5}}}

	_, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpClick, Ref: 1})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected a stale-ref version guard error, got %v", err)
	}
	if len(ft.got) != 0 {
		t.Fatal("a version-stale-ref act must never reach the transport")
	}
}

// TestRefVersionMatchPasses confirms a ref stamped with the current snapshot version
// validates and reaches the transport (the guard does not over-reject).
func TestRefVersionMatchPasses(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	s.latest = browserwire.Observation{Version: 6, Refs: []browserwire.Ref{{ID: 1, Version: 6}}}

	if _, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpClick, Ref: 1}); err != nil {
		t.Fatalf("a current-version ref must validate, got %v", err)
	}
	if len(ft.got) != 1 {
		t.Fatalf("expected the act to reach the transport, sent %d", len(ft.got))
	}
}

func TestDriverErrorSurfacedWithObservation(t *testing.T) {
	ft := &fakeTransport{reply: func(seq int, a browserwire.Act) browserwire.SessionResponse {
		return browserwire.SessionResponse{Seq: seq, Error: "selector matched no visible element",
			Observation: browserwire.Observation{Version: 2, URL: "http://x.test/after"}}
	}}
	s := newSession(ft, nil)
	s.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Version: 1}}}

	obs, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpClick, Ref: 1})
	if err == nil {
		t.Fatal("a driver-reported error must surface to the caller")
	}
	// The post-failure observation is still recorded so the agent can see the state.
	if obs.URL != "http://x.test/after" || s.Latest().URL != "http://x.test/after" {
		t.Fatalf("post-failure observation not recorded: %+v", obs)
	}
}

// TestTypedSecretScrubbedFromObservation exercises the host-side secret-reflow backstop
// (I3): a {{secret:NAME}} typed into a NON-password field (which the in-sandbox snapshot
// does not mask) must not reflow back to the model. After the type act resolves the
// secret, the driver's next observation echoes the plaintext in Text and a Ref's
// Name/Value; the session must scrub every occurrence to the sentinel before returning.
func TestTypedSecretScrubbedFromObservation(t *testing.T) {
	const secret = "s3cr3t-token-value"
	ft := &fakeTransport{reply: func(seq int, a browserwire.Act) browserwire.SessionResponse {
		// The driver reflows the typed value into a plain text field's value + page text
		// (a text/API-key input the snapshot does NOT treat as secret).
		return browserwire.SessionResponse{Seq: seq, Observation: browserwire.Observation{
			Version: 2, URL: "http://x.test/",
			Title: "token is " + secret,
			Text:  "your api key: " + secret + " (saved)",
			Refs: []browserwire.Ref{
				{ID: 0, Role: "textbox", Name: "API key", Value: secret, Version: 2},
			},
		}}
	}}
	s := newSession(ft, func(name string) (string, bool) {
		if name == "api_key" {
			return secret, true
		}
		return "", false
	})
	s.latest = browserwire.Observation{Version: 1, Refs: []browserwire.Ref{{ID: 1, Role: "textbox", Version: 1}}}

	obs, err := s.Act(context.Background(), browserwire.Act{Op: browserwire.OpType, Ref: 1, Text: "{{secret:api_key}}"})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	// The real value was sent to the driver (substitution works)…
	if ft.got[0].Text != secret {
		t.Fatalf("secret not substituted before send: %q", ft.got[0].Text)
	}
	// …but must NOT appear anywhere in the observation returned to the model.
	if strings.Contains(obs.Title, secret) || strings.Contains(obs.Text, secret) {
		t.Fatalf("secret reflowed into observation text/title: %+v", obs)
	}
	for _, r := range obs.Refs {
		if strings.Contains(r.Name, secret) || strings.Contains(r.Value, secret) {
			t.Fatalf("secret reflowed into a ref name/value: %+v", r)
		}
	}
	if !strings.Contains(obs.Text, "«secret»") || !strings.Contains(obs.Refs[0].Value, "«secret»") {
		t.Fatalf("scrubbed value should be replaced by the sentinel: %+v", obs)
	}
	// Latest() must also be scrubbed (it is what the tool renders).
	if strings.Contains(s.Latest().Text, secret) {
		t.Fatalf("Latest() still carries the plaintext secret: %q", s.Latest().Text)
	}
}

func TestObserveUpdatesLatest(t *testing.T) {
	ft := &fakeTransport{reply: func(seq int, a browserwire.Act) browserwire.SessionResponse {
		return browserwire.SessionResponse{Seq: seq, Observation: browserwire.Observation{
			Version: 3, URL: "http://x.test/", Refs: []browserwire.Ref{{ID: 0, Role: "button", Name: "Go"}}}}
	}}
	s := newSession(ft, nil)
	obs, err := s.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Version != 3 || s.Latest().Version != 3 {
		t.Fatalf("Observe did not record latest: %+v", obs)
	}
	if ft.got[0].Op != browserwire.OpObserve {
		t.Fatalf("Observe sent %q, want observe", ft.got[0].Op)
	}
}
