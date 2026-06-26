package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/packs"
	"nilcore/internal/artifact/packs/finance"
	"nilcore/internal/eventlog"
	"nilcore/internal/sandbox"
	"nilcore/internal/secrets"
	"nilcore/internal/verify"
	"nilcore/internal/verify/vcache"
)

// vcacheDecorate wraps base in the A9 content-hash verify cache (Phase 16, LRN-T05)
// when NILCORE_VCACHE is set and a log + worktree box are available: a chain-verified
// PASS over the EXACT same worktree content + verifier-id + toolchain is REPLAYED
// instead of re-run, skipping a redundant (expensive) `make verify` the loop would
// otherwise repeat on an unchanged integration tip. It is I2-safe by construction —
// vcache.Lookup re-runs eventlog.Verify and FAILS CLOSED to recompute on any chain
// error, and only a pass the inner verifier itself produced is ever replayed; the
// verifier stays the sole authority on "done". DEFAULT-OFF: with the env unset (or no
// log / log path / box / workdir) Decorate returns base UNCHANGED — byte-identical.
func vcacheDecorate(base verify.Verifier, box sandbox.Sandbox, verifierID string, log *eventlog.Log, logPath string) verify.Verifier {
	if os.Getenv("NILCORE_VCACHE") == "" || log == nil || logPath == "" || box == nil || box.Workdir() == "" {
		return base
	}
	return vcache.Decorate(vcache.Config{
		Inner:   base,
		Log:     log,
		LogPath: logPath,
		Hash: func(ctx context.Context) (string, error) {
			// Hash everything the verifier reads (the worktree), skipping VCS/agent state.
			return verify.ContentHashWorktree(ctx, box.Workdir(), ".git", ".nilcore")
		},
		VerifierID: verifierID,
		Toolchain:  verify.Toolchain(),
	})
}

// behavioralVerifier builds the project verifier, optionally composed with a
// headless-browser behavioral check (P9-T03) and/or an evidence-artifact check
// (P11-T05). When NILCORE_BROWSER_VERIFY is set (to the in-sandbox browser-driver
// command that navigates the running app and exits non-zero on a broken render),
// the verdict ANDs the project's own checks with a verify.BrowserVerifier — so a
// change that builds and tests green but renders broken still ships RED. When
// NILCORE_EVIDENCE_VERIFY is set AND the worktree carries one or more artifact
// files (.nilcore/artifacts/<id>.json), the verdict ALSO ANDs in an
// evverify.ArtifactVerifier per artifact, so a report/matrix/dossier whose claims
// did not each pass a runnable check ships RED (I2). The verifier stays the sole
// authority on "done" (I2); a behavioral or evidence result is an INPUT to the
// verdict, never a self-report. Unset ⇒ exactly verify.New (byte-identical).
//
// It is applied to whole-app drives (run / chat / serve / resume), not to
// individual build subagents — a behavioral check belongs at the app level, not
// per-component. (Per-subagent evidence verification is composed into env.Verifier
// separately by P11-T16.) These app-level call sites do not thread an event log, so
// the evidence checks here run with a nil EventSink; the eventlog-backed sink is
// supplied by behavioralVerifierWithLog (and reused by P11-T16's env.Verifier).
func behavioralVerifier(box sandbox.Sandbox, cmd string) verify.Verifier {
	return behavioralVerifierWithLog(box, cmd, nil)
}

// behavioralVerifierWithLog is the log-bearing form of behavioralVerifier: when a
// non-nil eventlog is supplied AND evidence verification is enabled, each
// ArtifactVerifier emits its additive artifact_verify/claim_verify events through
// the eventlog (I5 — new append-only kinds, never a mutation). behavioralVerifier
// delegates here with a nil log so the existing app-level call sites stay
// byte-identical and emit no evidence events; a future log-bearing caller (P11-T16)
// passes its run log to get the audit trail. With every evidence/browser toggle off
// this returns exactly verify.New(box, cmd) — the unset path is byte-identical.
func behavioralVerifierWithLog(box sandbox.Sandbox, cmd string, log *eventlog.Log) verify.Verifier {
	base := verify.New(box, cmd)

	var extra []verify.NamedVerifier
	if bcmd := strings.TrimSpace(os.Getenv("NILCORE_BROWSER_VERIFY")); bcmd != "" {
		extra = append(extra, verify.NamedVerifier{Name: "browser", V: verify.NewBrowser(box, bcmd)})
	}
	extra = append(extra, evidenceVerifiers(box, log)...)

	if len(extra) == 0 {
		// No behavioral/evidence checks opted in: return the bare project verifier
		// exactly as before, so the default path is byte-identical (P11-T05/P9-T03).
		return base
	}

	// Named[0] is always the build/"checks" verifier, so an evidence or browser
	// check can never mask a red build: Composite short-circuits on the first
	// failure and the build verifier runs first (I2).
	named := make([]verify.NamedVerifier, 0, 1+len(extra))
	named = append(named, verify.NamedVerifier{Name: "checks", V: base})
	named = append(named, extra...)
	return verify.Composite{Named: named}
}

// evidenceVerifiers returns one trailing NamedVerifier per artifact file present in
// the worktree, gated on NILCORE_EVIDENCE_VERIFY. It is the P11-T05 wiring seam:
//
//   - Env unset                       ⇒ nil (no evidence verifier; byte-identical).
//   - Env set, no artifact file       ⇒ nil (a green build still greens — an
//     evidence verifier is only added when there is
//     something to assert over).
//   - Env set, artifact file(s) found ⇒ one ArtifactVerifier per file, each composed
//     after the build verifier so any red claim
//     reddens the whole verdict (I2).
//
// The registry starts at evverify.Default() — only safe, generic stdlib checks; an
// unregistered verifier-id resolves to StatusUnverifiable, never Pass. When
// NILCORE_VERIFY_PACKS names one or more domain packs (web/software/finance/ui), those
// packs' RegisterAll ids are added on top (P11-T12) so a claim naming e.g.
// finance.sec_fact resolves to a real check instead of Unverifiable-by-missing-id.
// Every check reaches the network only through the box (I4); a nil box fails network
// claims closed to Unverifiable with no host-side request. MaxAge comes from
// NILCORE_EVIDENCE_MAX_AGE (0/unset ⇒ staleness disabled); it can only DEMOTE a pass to
// stale, never be the sole basis to PASS (I2).
//
// Pack selection is fail-closed: an unknown pack name (a typo in NILCORE_VERIFY_PACKS)
// makes every artifact verifier RED via the always-fail sentinel rather than silently
// dropping the requested check — so a misconfigured run never greens by ignoring a pack
// it was told to run. The explicit startup signal lives in validateVerifyPacks.
func evidenceVerifiers(box sandbox.Sandbox, log *eventlog.Log) []verify.NamedVerifier {
	if strings.TrimSpace(os.Getenv("NILCORE_EVIDENCE_VERIFY")) == "" {
		return nil
	}
	if box == nil {
		// No worktree to scan and no box to verify through. There is nothing to assert
		// over; leave the verdict to the build verifier rather than fabricate a check.
		return nil
	}

	paths := artifactFiles(box.Workdir())
	if len(paths) == 0 {
		return nil
	}

	maxAge := evidenceMaxAge()
	sink := evidenceEventSink(log)

	reg, err := evidenceRegistry()
	if err != nil {
		// Fail-closed: a bad pack list (unknown name) must not silently fall back to the
		// generic-only registry — that would green a finance/ui claim as a no-op. Redden
		// the whole evidence verdict with a single named failure carrying the reason.
		return []verify.NamedVerifier{{Name: "evidence:packs", V: failClosed{reason: err.Error()}}}
	}

	out := make([]verify.NamedVerifier, 0, len(paths))
	for _, p := range paths {
		av := &evverify.ArtifactVerifier{
			Box:       box,
			Reg:       reg,
			RelPath:   p,
			MaxAge:    maxAge,
			EventSink: sink,
		}
		out = append(out, verify.NamedVerifier{Name: "evidence:" + artifactID(p), V: av})
	}
	return out
}

// artifactFiles returns the absolute paths of every .nilcore/artifacts/*.json file
// in the worktree, sorted for a stable verifier order. It is a host-side READ of the
// worktree the app verifier owns purely to discover which artifacts exist; the actual
// load is done inside evverify via worktreefs (O_NOFOLLOW), so a symlink swapped in at
// a target path is still refused there. A missing/empty directory yields no paths
// (evidence verification is then a no-op — the green-build path stays green).
func artifactFiles(root string) []string {
	if root == "" {
		return nil
	}
	dir := filepath.Join(root, ".nilcore", "artifacts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths
}

// artifactID recovers the artifact id from its file path for the NamedVerifier label
// (a human-readable failure prefix only — never a trust input).
func artifactID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

// evidenceMaxAge reads the optional staleness window from NILCORE_EVIDENCE_MAX_AGE
// (a Go duration, e.g. "24h"). Unset/blank/invalid ⇒ 0, which disables staleness
// (MaxAge can only DEMOTE a verified pass to stale, never PASS on a timestamp — I2).
func evidenceMaxAge() time.Duration {
	raw := strings.TrimSpace(os.Getenv("NILCORE_EVIDENCE_MAX_AGE"))
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// evidenceEventSink adapts the evverify EventSink callback to the append-only event
// log (I5). It emits two additive kinds, Detail-only and metadata-only:
//
//   - claim_verify    {claim_id, field, verifier, status, source_url}
//   - artifact_verify {id, kind, green, pass, fail, stale, unverifiable}
//
// Both carry ONLY harness-trusted fields plus the claim's key-free SourceURL (I3 —
// provenance is required key-free; the model-authored Value/Statement are never
// echoed, I7). The eventlog redaction path still runs over every Detail, so a secret
// that somehow reached a field is scrubbed. A nil log ⇒ nil sink ⇒ no events emit and
// the verifier behaves byte-identically (the unset/log-less app path).
func evidenceEventSink(log *eventlog.Log) func(ev any) {
	if log == nil {
		return nil
	}
	return func(ev any) {
		switch e := ev.(type) {
		case evverify.ClaimVerifyEvent:
			log.Append(eventlog.Event{Kind: "claim_verify", Detail: map[string]any{
				"claim_id":   e.ClaimID,
				"field":      e.Field,
				"verifier":   e.Verifier,
				"status":     string(e.Status),
				"source_url": e.SourceURL,
			}})
		case evverify.ArtifactVerifyEvent:
			log.Append(eventlog.Event{Kind: "artifact_verify", Detail: map[string]any{
				"id":           e.ArtifactID,
				"kind":         string(e.Kind),
				"green":        e.Green,
				"pass":         e.Pass,
				"fail":         e.Fail,
				"stale":        e.Stale,
				"unverifiable": e.Unverifiable,
			}})
		}
	}
}

// verifyPacks parses the opt-in NILCORE_VERIFY_PACKS / -verify-packs list into the
// pack names to register on top of evverify.Default(). Names are comma-separated and
// (per packs.Select) case-insensitive + space-trimmed; an empty/blank list returns nil,
// the byte-identical default where the registry equals evverify.Default() and any
// pack-claim resolves Unverifiable rather than Pass.
func verifyPacks() []string {
	raw := strings.TrimSpace(os.Getenv("NILCORE_VERIFY_PACKS"))
	if raw == "" {
		return nil
	}
	var names []string
	for _, part := range strings.Split(raw, ",") {
		if n := strings.TrimSpace(part); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// evidenceRegistry builds the verifier registry the evidence verifiers run against:
// evverify.Default() (generic stdlib checks only) plus exactly the packs named in
// NILCORE_VERIFY_PACKS. With no packs opted in it returns Default() unchanged — the
// byte-identical P11-T05 state. packs.Select is ATOMIC: an unknown pack name aborts
// before any registration, so the returned error means NOTHING was registered and the
// caller fails the verdict closed rather than running a half-populated registry.
//
// Before returning, keyed packs' API keys are seeded from the SecretStore into the
// process environment by NAME (env-first, then SecretStore — mirroring the credential
// resolver at main.go). The pack itself references the key by $NAME and injects the
// VALUE via box.ExecWithEnv for a single invocation; the literal key never enters the
// command string, the persisted Evidence.SourceURL, or any event Detail (I3).
func evidenceRegistry() (*evverify.Registry, error) {
	reg := evverify.Default()
	names := verifyPacks()
	if len(names) == 0 {
		return reg, nil
	}
	if err := packs.Select(names, reg); err != nil {
		return nil, err
	}
	seedKeyedPackSecrets(names)
	return reg, nil
}

// validateVerifyPacks is the explicit startup signal that the opted-in pack list is
// resolvable: it returns a non-nil error for an unknown pack name so a misconfigured
// run can fail loudly at boot instead of only reddening at verify time. It is a pure
// validation (a throwaway registry), safe to call before any verification. Empty list
// (packs off) ⇒ nil.
func validateVerifyPacks() error {
	names := verifyPacks()
	if len(names) == 0 {
		return nil
	}
	if err := packs.Select(names, evverify.New()); err != nil {
		return fmt.Errorf("NILCORE_VERIFY_PACKS: %w", err)
	}
	return nil
}

// keyedPackEnv maps each pack name to the SecretStore-resolvable env var NAMES its
// keyed checks reference. Only the NAME lives here (and in the pack leaf); the VALUE is
// resolved from the SecretStore at wiring time and injected per-invocation by the pack
// via box.ExecWithEnv. Keyless packs have no entry.
var keyedPackEnv = map[string][]string{
	packs.NameFinance: {finance.EnvFREDKey, finance.EnvMarketKey},
}

// anyKeyedPack reports whether any selected pack has keyed checks (an entry in
// keyedPackEnv). It gates the SecretStore lookup so a keyless selection never probes the
// host store.
func anyKeyedPack(names []string) bool {
	for _, raw := range names {
		if _, ok := keyedPackEnv[strings.ToLower(strings.TrimSpace(raw))]; ok {
			return true
		}
	}
	return false
}

// secretStoreForPacks is the SecretStore the keyed-pack key resolution reads from. It is
// a package var so tests can inject a hermetic fake; when nil and a keyed pack is opted
// in, seedKeyedPackSecrets falls back to the host store (secrets.Detect) so the default
// boot path resolves keys without a main.go edit. It is consulted ONLY when a keyed pack
// is actually selected, so the packs-off path performs no SecretStore lookup and stays
// byte-identical.
var secretStoreForPacks secrets.SecretStore

// seedKeyedPackSecrets resolves each opted-in keyed pack's API keys env-first, then from
// the SecretStore, and seeds any value found (and not already present) into the process
// environment by NAME. This is the SecretStore → box.ExecWithEnv hop required by I3: the
// pack reads the NAME at run time and routes the VALUE through ExecWithEnv, so the key
// never lands in the command string, the artifact JSON, or an event Detail. A missing
// secret leaves the env untouched (a keyed check with no key supplied then resolves
// Unverifiable, never Pass). The host store is detected lazily and ONLY when a keyed pack
// was selected, so the default packs-off path never probes the keychain.
func seedKeyedPackSecrets(names []string) {
	// Only packs with keyed checks need a store; skip the lookup (and the keychain probe)
	// entirely when none of the selected packs is keyed.
	if !anyKeyedPack(names) {
		return
	}
	store := secretStoreForPacks
	if store == nil {
		store = secrets.Detect()
	}
	if store == nil {
		return
	}
	for _, raw := range names {
		envNames, ok := keyedPackEnv[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			continue
		}
		for _, name := range envNames {
			if strings.TrimSpace(os.Getenv(name)) != "" {
				continue // env-first: an operator-set value wins, no SecretStore read
			}
			if v, err := store.Get(name); err == nil && v != "" {
				_ = os.Setenv(name, v)
			}
		}
	}
}

// failClosed is a verify.Verifier that always reports RED with a fixed reason. It is the
// fail-closed sentinel for a wiring error (e.g. an unknown pack name): rather than run a
// silently-degraded registry, the evidence verdict carries one named failure so the run
// reds and the operator sees why.
type failClosed struct{ reason string }

func (f failClosed) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: false, Output: f.reason}, nil
}
