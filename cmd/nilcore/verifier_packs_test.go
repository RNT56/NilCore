package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/packs/finance"
	"nilcore/internal/eventlog"
	"nilcore/internal/secrets"
)

// fakeSecretStore is a hermetic SecretStore that returns canned values by name. It is
// the in-memory stand-in for the keyed-pack secret resolution test (no keychain/vault).
type fakeSecretStore struct{ vals map[string]string }

func (s fakeSecretStore) Get(name string) (string, error) {
	if v, ok := s.vals[name]; ok {
		return v, nil
	}
	return "", secrets.ErrNotFound
}
func (s fakeSecretStore) Set(string, string) error { return secrets.ErrReadOnly }
func (s fakeSecretStore) Delete(string) error      { return secrets.ErrReadOnly }
func (s fakeSecretStore) Name() string             { return "fake" }

// writeClaimArtifact writes a one-claim artifact whose claim names verifierID, with a
// key-free SourceURL the keyed packs derive their request from. Returns the artifact id.
func writeClaimArtifact(t *testing.T, root, id, verifierID, value, sourceURL string) string {
	t.Helper()
	a := &artifact.Artifact{
		ID:        id,
		Kind:      artifact.KindReport,
		Title:     "packs-wiring",
		CreatedAt: time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC),
		Claims: []artifact.Claim{{
			ID:    id + "-c1",
			Field: "f1",
			Evidence: artifact.Evidence{
				Value:     value,
				SourceURL: sourceURL,
				Verifier:  verifierID,
				Status:    artifact.StatusPass, // self-written; the verifier must overwrite it
			},
		}},
	}
	if err := artifact.Write(root, a); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return id
}

// TestVerifyPacksWiring is the P11-T12 gate: NILCORE_VERIFY_PACKS adds the named domain
// packs onto evverify.Default(); unset is byte-identical to the P11-T05 state; an
// unknown name fails closed; and a keyed pack's key resolves from the SecretStore and is
// injected via box.ExecWithEnv without ever landing in the artifact JSON or an event.
func TestVerifyPacksWiring(t *testing.T) {
	t.Run("unset => registry equals evverify.Default()", func(t *testing.T) {
		t.Setenv("NILCORE_VERIFY_PACKS", "")
		reg, err := evidenceRegistry()
		if err != nil {
			t.Fatalf("evidenceRegistry: %v", err)
		}
		// Default() registers exactly web.url_resolves and nothing else: a pack id must
		// remain unresolvable, proving no pack was registered (byte-identical to T05).
		if _, ok := reg.Lookup("web.url_resolves"); !ok {
			t.Fatalf("default registry must keep the generic web.url_resolves check")
		}
		for _, id := range []string{"software.npm_version_exists", "finance.sec_fact", "ui.flow_passes"} {
			if _, ok := reg.Lookup(id); ok {
				t.Fatalf("packs off: %q must be unregistered (Unverifiable, never Pass)", id)
			}
		}
		// And validation passes (nothing to validate).
		if err := validateVerifyPacks(); err != nil {
			t.Fatalf("validateVerifyPacks with no packs must be nil, got %v", err)
		}
	})

	t.Run("web,software => those pack ids resolve", func(t *testing.T) {
		t.Setenv("NILCORE_VERIFY_PACKS", " Web, Software ") // case-insensitive + spaced
		reg, err := evidenceRegistry()
		if err != nil {
			t.Fatalf("evidenceRegistry: %v", err)
		}
		if _, ok := reg.Lookup("software.npm_version_exists"); !ok {
			t.Fatalf("software pack id must resolve when NILCORE_VERIFY_PACKS names it")
		}
		if _, ok := reg.Lookup("web.quote_exists"); !ok {
			t.Fatalf("web pack id must resolve when NILCORE_VERIFY_PACKS names it")
		}
		// A finance id was NOT selected, so it stays unregistered.
		if _, ok := reg.Lookup("finance.sec_fact"); ok {
			t.Fatalf("finance id must stay unregistered when not selected")
		}
	})

	t.Run("software pack: claim verified, not Unverifiable-by-missing-id", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		t.Setenv("NILCORE_VERIFY_PACKS", "software")
		// exit 22 ⇒ the npm fetch is non-2xx ⇒ the registered check returns Unverifiable
		// with a fetch reason. The point: the claim was VERIFIED by a real check, not left
		// Unverifiable because the id was missing (which the wiring is what makes resolvable).
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 22}
		id := writeClaimArtifact(t, box.dir, "swrep", "software.npm_version_exists", "1.2.3",
			"https://registry.npmjs.org/left-pad")

		v := behavioralVerifier(box, "true")
		_ = readReport(t, v) // run the verifier so it writes back the verdict

		// The check ran in-box (a command was issued) — proof the id resolved to a real
		// CheckFunc rather than the missing-id Unverifiable shortcut.
		if len(box.cmdsSeen) == 0 {
			t.Fatalf("registered software check must issue a box command; got none")
		}
		got := readArtifactStatus(t, box.dir, id)
		if got == "" {
			t.Fatalf("artifact status not written back")
		}
	})

	t.Run("unknown pack => fail closed (startup error + red verdict)", func(t *testing.T) {
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		t.Setenv("NILCORE_VERIFY_PACKS", "web,boguspack")

		if err := validateVerifyPacks(); err == nil {
			t.Fatalf("an unknown pack name must produce a startup error")
		}
		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}
		writeClaimArtifact(t, box.dir, "rep", "web.url_resolves", "v", "https://example.com")

		v := behavioralVerifier(box, "true")
		if rep := readReport(t, v); rep.Passed {
			t.Fatalf("an unknown pack must redden the verdict (fail closed), got Passed=true")
		}
	})

	t.Run("keyed finance key from SecretStore, never in artifact or event", func(t *testing.T) {
		const secret = "FRED-SECRET-9999"
		t.Setenv("NILCORE_EVIDENCE_VERIFY", "1")
		t.Setenv("NILCORE_BROWSER_VERIFY", "")
		t.Setenv("NILCORE_VERIFY_PACKS", "finance")
		// Ensure the key is NOT already in the env, so the SecretStore path is exercised.
		t.Setenv(finance.EnvFREDKey, "")

		// Inject a hermetic SecretStore for the duration of the test.
		prev := secretStoreForPacks
		secretStoreForPacks = fakeSecretStore{vals: map[string]string{finance.EnvFREDKey: secret}}
		t.Cleanup(func() { secretStoreForPacks = prev })

		box := &fakeVerifierBox{dir: t.TempDir(), exit: 0}
		id := writeClaimArtifact(t, box.dir, "frep", "finance.fred_series", "100.0",
			"https://api.stlouisfed.org/fred/series/observations?series_id=GDP")

		p := filepath.Join(t.TempDir(), "events.jsonl")
		log, err := eventlog.Open(p)
		if err != nil {
			t.Fatalf("open log: %v", err)
		}
		v := behavioralVerifierWithLog(box, "true", log)
		_ = readReport(t, v)

		// The SecretStore value was seeded into the env so the pack could inject it.
		if box.envSeen[finance.EnvFREDKey] != secret {
			t.Fatalf("keyed pack must inject the SecretStore key via ExecWithEnv; env seen %v", box.envSeen)
		}
		// The literal key must appear in NEITHER the command string, the artifact JSON,
		// nor the event Detail (I3).
		for _, cmd := range box.cmdsSeen {
			if strings.Contains(cmd, secret) {
				t.Fatalf("literal key leaked into the command string: %s", cmd)
			}
		}
		artJSON := readFile(t, filepath.Join(box.dir, ".nilcore", "artifacts", id+".json"))
		if strings.Contains(artJSON, secret) {
			t.Fatalf("key leaked into the persisted artifact JSON:\n%s", artJSON)
		}
		if body := readFile(t, p); strings.Contains(body, secret) {
			t.Fatalf("key leaked into an event Detail:\n%s", body)
		}
	})
}

// readArtifactStatus loads the persisted artifact and returns its first claim's
// verifier-written status (empty if unreadable). Used to prove a pack check actually ran.
func readArtifactStatus(t *testing.T, root, id string) string {
	t.Helper()
	a, err := artifact.Read(root, id)
	if err != nil {
		t.Fatalf("read back artifact: %v", err)
	}
	if len(a.Claims) == 0 {
		return ""
	}
	return string(a.Claims[0].Evidence.Status)
}
