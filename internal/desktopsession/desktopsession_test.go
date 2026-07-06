package desktopsession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/desktopwire"
	"nilcore/internal/sandbox"
)

// fakeTransport drives the Session logic with no daemon.
type fakeTransport struct {
	got   []desktopwire.Act
	reply func(seq int, a desktopwire.Act) desktopwire.SessionResponse
}

func (f *fakeTransport) waitReady(context.Context) error { return nil }
func (f *fakeTransport) close() error                    { return nil }
func (f *fakeTransport) send(_ context.Context, req desktopwire.SessionRequest) (desktopwire.SessionResponse, error) {
	f.got = append(f.got, req.Act)
	if f.reply != nil {
		return f.reply(req.Seq, req.Act), nil
	}
	return desktopwire.SessionResponse{Seq: req.Seq, Observation: desktopwire.Observation{Version: 1}}, nil
}

func TestSecretSubstitution(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, func(name string) (string, bool) {
		if name == "pw" {
			return "hunter2", true
		}
		return "", false
	})
	s.latest = desktopwire.Observation{Version: 1, Refs: []desktopwire.Ref{{ID: 1, Role: "entry", Version: 1}}}
	if _, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpType, Ref: 1, Text: "x={{secret:pw}}"}); err != nil {
		t.Fatal(err)
	}
	if ft.got[0].Text != "x=hunter2" {
		t.Fatalf("secret not substituted before send: %q", ft.got[0].Text)
	}
}

// TestTypedSecretScrubbedFromObservation exercises the host-side secret-reflow backstop
// (I3): a {{secret:NAME}} typed into a NON-password desktop field must not reflow back to
// the model. After the type act resolves the secret, the driver's next observation echoes
// the plaintext in Title/Text/FocusedWindow and a Ref's Name/Value (the a11y dump populates
// Ref.Value raw); the session must scrub every occurrence to the sentinel before returning.
func TestTypedSecretScrubbedFromObservation(t *testing.T) {
	const secret = "s3cr3t-token-value"
	ft := &fakeTransport{reply: func(seq int, a desktopwire.Act) desktopwire.SessionResponse {
		return desktopwire.SessionResponse{Seq: seq, Observation: desktopwire.Observation{
			Version:       2,
			Title:         "token is " + secret,
			Text:          "your api key: " + secret + " (saved)",
			FocusedWindow: "field: " + secret,
			Refs: []desktopwire.Ref{
				{ID: 1, Role: "entry", Name: "API key", Value: secret, Version: 2},
			},
		}}
	}}
	s := newSession(ft, func(name string) (string, bool) {
		if name == "api_key" {
			return secret, true
		}
		return "", false
	})
	s.latest = desktopwire.Observation{Version: 1, Refs: []desktopwire.Ref{{ID: 1, Role: "entry", Version: 1}}}

	obs, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpType, Ref: 1, Text: "{{secret:api_key}}"})
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	// The real value was sent to the driver (substitution works)…
	if ft.got[0].Text != secret {
		t.Fatalf("secret not substituted before send: %q", ft.got[0].Text)
	}
	// …but must NOT appear anywhere in the observation returned to the model.
	if strings.Contains(obs.Title, secret) || strings.Contains(obs.Text, secret) || strings.Contains(obs.FocusedWindow, secret) {
		t.Fatalf("secret reflowed into observation title/text/window: %+v", obs)
	}
	for _, r := range obs.Refs {
		if strings.Contains(r.Name, secret) || strings.Contains(r.Value, secret) {
			t.Fatalf("secret reflowed into a ref name/value: %+v", r)
		}
	}
	if !strings.Contains(obs.Text, secretSentinel) || !strings.Contains(obs.Refs[0].Value, secretSentinel) {
		t.Fatalf("scrubbed value should be replaced by the sentinel: %+v", obs)
	}
	// Latest() must also be scrubbed (it is what the tool renders).
	if strings.Contains(s.Latest().Text, secret) {
		t.Fatalf("Latest() still carries the plaintext secret: %q", s.Latest().Text)
	}
}

func TestSecretMissingFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, func(string) (string, bool) { return "", false })
	s.latest = desktopwire.Observation{Version: 1, Refs: []desktopwire.Ref{{ID: 1, Version: 1}}}
	if _, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpType, Ref: 1, Text: "{{secret:nope}}"}); err == nil {
		t.Fatal("expected an error for an unresolved secret")
	}
	if len(ft.got) != 0 {
		t.Fatal("an unresolved-secret act must never reach the transport")
	}
}

func TestStaleRefFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	s.latest = desktopwire.Observation{Version: 5, Refs: []desktopwire.Ref{{ID: 1}, {ID: 2}}}
	_, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 9})
	if err == nil || !strings.Contains(err.Error(), "stale-ref") {
		t.Fatalf("expected a stale-ref guard error, got %v", err)
	}
	if len(ft.got) != 0 {
		t.Fatal("a stale-ref act must never reach the transport")
	}
}

// TestStaleRefVersionMismatchFailsClosed mirrors the browser tier's swap defense: a ref
// whose ID is still present in the latest snapshot but whose stamped Version is older (a
// re-render reused the positional CV/AT-SPI id) must fail closed host-side. A pure
// membership check would have actuated a DIFFERENT element on the real desktop.
func TestStaleRefVersionMismatchFailsClosed(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	// Snapshot is at version 6 but ref 1 still carries version 5.
	s.latest = desktopwire.Observation{Version: 6, Refs: []desktopwire.Ref{{ID: 1, Version: 5}}}
	_, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 1})
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
	s.latest = desktopwire.Observation{Version: 6, Refs: []desktopwire.Ref{{ID: 1, Version: 6}}}
	if _, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Ref: 1}); err != nil {
		t.Fatalf("a current-version ref must validate, got %v", err)
	}
	if len(ft.got) != 1 {
		t.Fatalf("expected the act to reach the transport, sent %d", len(ft.got))
	}
}

func TestCoordinateActSkipsRefValidation(t *testing.T) {
	ft := &fakeTransport{}
	s := newSession(ft, nil)
	s.latest = desktopwire.Observation{Version: 1} // no refs
	// A Rung-3 coordinate click has Ref==0, so it must NOT be rejected by the ref guard.
	if _, err := s.Act(context.Background(), desktopwire.Act{Op: desktopwire.OpClick, Coordinate: []int{10, 20}}); err != nil {
		t.Fatalf("coordinate act should pass the ref guard: %v", err)
	}
	if len(ft.got) != 1 {
		t.Fatal("coordinate act should reach the transport")
	}
}

// ── file-queue round-trip against a fake in-process daemon ──

type fakeDaemonBox struct{ dir string }

func (f *fakeDaemonBox) Workdir() string { return f.dir }
func (f *fakeDaemonBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return f.Exec(ctx, cmd)
}
func (f *fakeDaemonBox) Exec(ctx context.Context, cmd string) (sandbox.Result, error) {
	control := ""
	toks := strings.Fields(cmd)
	for i, tk := range toks {
		if tk == "--control" && i+1 < len(toks) {
			control = filepath.Join(f.dir, strings.Trim(toks[i+1], "'"))
		}
	}
	if control == "" {
		return sandbox.Result{ExitCode: 2}, nil
	}
	_ = os.MkdirAll(control, 0o700)
	_ = atomicWrite(filepath.Join(control, readyMarker), []byte("1"))
	seq := 1
	for {
		if ctx.Err() != nil {
			return sandbox.Result{}, nil
		}
		reqPath := filepath.Join(control, reqPrefix+digit(seq)+jsonSuffix)
		data, err := os.ReadFile(reqPath)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		var req desktopwire.SessionRequest
		_ = json.Unmarshal(data, &req)
		if req.Act.Op == desktopwire.OpClose {
			b, _ := json.Marshal(desktopwire.SessionResponse{Seq: seq})
			_ = atomicWrite(filepath.Join(control, respPrefix+digit(seq)+jsonSuffix), b)
			_ = os.Remove(reqPath)
			return sandbox.Result{}, nil
		}
		obs := desktopwire.Observation{Version: uint64(seq), Rung: desktopwire.RungATSPI,
			FocusedWindow: "app-" + req.Act.Op, Refs: []desktopwire.Ref{{ID: 1, Role: "push button", Name: "Go", Version: uint64(seq)}}}
		b, _ := json.Marshal(desktopwire.SessionResponse{Seq: seq, Observation: obs})
		_ = atomicWrite(filepath.Join(control, respPrefix+digit(seq)+jsonSuffix), b)
		_ = os.Remove(reqPath)
		seq++
	}
}

func digit(n int) string { return string(rune('0' + n)) } // single-digit seqs suffice

func TestFileTransportRoundTrip(t *testing.T) {
	box := &fakeDaemonBox{dir: t.TempDir()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, first, err := Launch(ctx, box, Options{})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer s.Close()
	if first.Version == 0 || len(first.Refs) == 0 || first.Rung != desktopwire.RungATSPI {
		t.Fatalf("first observation wrong: %+v", first)
	}
	if _, err := s.Act(ctx, desktopwire.Act{Op: desktopwire.OpClick, Ref: 1}); err != nil {
		t.Fatalf("click ref 1: %v", err)
	}
	if _, err := s.Act(ctx, desktopwire.Act{Op: desktopwire.OpClick, Ref: 7}); err == nil {
		t.Fatal("stale ref 7 must fail closed")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLaunchNilBoxRefuses(t *testing.T) {
	if _, _, err := Launch(context.Background(), nil, Options{}); err == nil {
		t.Fatal("nil sandbox must refuse")
	}
}
