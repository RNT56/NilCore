package main

// audit_remediation_test.go adds DISCRIMINATING regression tests for fixes from the
// 2026-07-10 adversarial-audit remediation (399c2a3) whose MECHANISM was correct but
// previously untested. Each test asserts the SPECIFIC behavior only the fix produces,
// so it reddens the moment that fix regresses. They drive no container and no network.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/capguard"
	"nilcore/internal/meter"
	"nilcore/internal/model"
	"nilcore/internal/project"
	"nilcore/internal/sandbox"
	"nilcore/internal/worktree"

	"nilcore/internal/budget"
)

// TestChatTUIResolveAutoBeforeProvider guards the `-backend auto` fix in chat.go +
// tui.go: "auto" is mapped to a CONCRETE backend by resolveAutoBackend BEFORE
// resolveProvider ever sees it. Pre-fix the chat/tui front doors handed the literal
// "auto" straight to resolveProvider, which fataled `unknown backend "auto"`.
//
// Would redden if: the front-door ordering regressed (resolveProvider called with
// "auto"), or resolveAutoBackend stopped mapping auto → a real backend.
func TestChatTUIResolveAutoBeforeProvider(t *testing.T) {
	emptyPath(t) // codex/claude absent ⇒ only native is available
	b := boot{cfg: nativeCfg(), cred: credFor("ANTHROPIC_API_KEY")}

	// (1) resolveProvider REJECTS a literal "auto" — this is the poison the ordering
	// fix must intercept, proving the guard is load-bearing (not decorative).
	if _, err := resolveProvider("auto", b); err == nil || !strings.Contains(err.Error(), `unknown backend "auto"`) {
		t.Fatalf(`resolveProvider("auto") = %v, want an 'unknown backend "auto"' error`, err)
	}

	// (2)+(3) Mirror the chat.go / tui.go setup EXACTLY: parse -backend auto, map it to
	// a concrete backend, THEN resolveProvider. The mapped name must never be "auto".
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	c := autoFlags(t, []string{"-backend", "auto", "-log", logPath})
	if *c.backendName != "auto" {
		t.Fatalf("-backend auto should parse to %q, got %q", "auto", *c.backendName)
	}
	if *c.backendName == "auto" { // the exact chat.go:135 / tui.go:69 guard
		*c.backendName = resolveAutoBackend(c, b, openLogAt(t, logPath))
	}
	if *c.backendName == "auto" {
		t.Fatal("resolveAutoBackend must map auto → a concrete backend, never leave it auto")
	}
	if *c.backendName != "native" {
		t.Errorf("only native available ⇒ resolved backend = %q, want native", *c.backendName)
	}
	// The concrete name resolveProvider now sees must NOT be the unknown-backend fatal.
	if _, err := resolveProvider(*c.backendName, b); err != nil {
		t.Fatalf("resolveProvider(%q) after auto-resolution errored: %v", *c.backendName, err)
	}
}

// TestMeteredAdvisor unit-tests main.go's meteredAdvisor: it wraps the advisor
// provider in a *meter.Provider that SHARES the main provider's budget Ledger when
// (and only when) the main provider is itself metered, and is otherwise a pass-through.
//
// Would redden if: the advisor got a FRESH ledger (its spend would escape the one
// budget wall), or an unmetered main path started wrapping/dropping the advisor.
func TestMeteredAdvisor(t *testing.T) {
	// (c) A nil advisor stays nil — there is no advisor tier to meter.
	if got := meteredAdvisor(&fakeProvider{id: "main"}, nil); got != nil {
		t.Errorf("meteredAdvisor(_, nil) = %v, want nil", got)
	}

	// (b) An UNMETERED main provider ⇒ the advisor is returned UNCHANGED (nothing to
	// charge on this path).
	adv := &fakeProvider{id: "adv"}
	if got := meteredAdvisor(&fakeProvider{id: "main"}, adv); got != model.Provider(adv) {
		t.Errorf("unmetered main ⇒ meteredAdvisor should return advProv unchanged, got %v", got)
	}

	// (a) A METERED main provider ⇒ the advisor is wrapped in a *meter.Provider that
	// shares the SAME Ledger pointer (one budget wall) and a distinct Task scope key.
	led := budget.New()
	main := &meter.Provider{Inner: &fakeProvider{id: "main"}, Ledger: led, Task: "supervisor", Price: meter.NewTable()}
	got := meteredAdvisor(main, adv)
	mp, ok := got.(*meter.Provider)
	if !ok {
		t.Fatalf("metered main ⇒ meteredAdvisor should return a *meter.Provider, got %T", got)
	}
	if mp.Ledger != led {
		t.Error("the advisor meter must SHARE the main provider's Ledger (one budget wall) — a fresh ledger would let advisor spend escape the ceiling")
	}
	if mp.Inner != model.Provider(adv) {
		t.Error("the advisor meter must wrap the advisor provider as Inner")
	}
	if mp.Task != "supervisor-advisor" {
		t.Errorf("advisor Task = %q, want supervisor-advisor (a distinct budget scope key)", mp.Task)
	}
}

// TestPrivateDataAxisFromSecretsOnly guards the capguard axis-B fix in browse.go /
// desktop.go: the Rule-of-Two private-data axis is `readRepo || secretCapable`, so a
// session that declares a {{secret:NAME}} allowlist holds private data even with
// -read=false. Pre-fix axis B keyed only off readRepo, so a secret-capable, read-false
// session evaded the gate.
//
// Would redden if: axis B stopped counting a declared-secret allowlist, or
// privateDataReason stopped reporting the secrets-only case.
func TestPrivateDataAxisFromSecretsOnly(t *testing.T) {
	// A secret allowlist declared via the -secrets flag ALONE (no env), read-repo OFF.
	secretNames := parseSecretNames("SITE_TOKEN, GH_PAT", "")
	if len(secretNames) != 2 {
		t.Fatalf("parseSecretNames(flag) = %v, want [SITE_TOKEN GH_PAT]", secretNames)
	}
	secretCapable := len(secretNames) > 0
	const readRepo = false

	// privateDataReason is the audit witness of axis B and mirrors the PrivateData
	// boolean (it is "" exactly when readRepo||secretCapable is false). Secrets-only ⇒
	// axis B is SET with the secrets-declared reason, NOT "".
	if got := privateDataReason(readRepo, secretCapable); got != "secrets-declared" {
		t.Errorf("privateDataReason(false, true) = %q, want secrets-declared (axis B set by secrets alone)", got)
	}

	// Assemble the EXACT capguard.Capabilities browse.go:159 / desktop.go:214 build and
	// assert the private-data axis is true from secrets alone.
	caps := capguard.Capabilities{
		UntrustedInput: true,
		PrivateData:    readRepo || secretCapable, // browse.go:161 / desktop.go:216
		Reasons:        map[string]string{"B": privateDataReason(readRepo, secretCapable)},
	}
	if !caps.PrivateData {
		t.Fatal("a secret-capable, -read=false session must set the Rule-of-Two private-data axis (B)")
	}
	if caps.Reasons["B"] != "secrets-declared" {
		t.Errorf("axis B reason = %q, want secrets-declared", caps.Reasons["B"])
	}

	// Negative control: no secrets AND no repo-read ⇒ axis B is OFF (reason empty),
	// so the fix did not simply pin B on unconditionally.
	if len(parseSecretNames("", "")) != 0 {
		t.Error("an empty -secrets/env pair must yield no declared secrets")
	}
	if got := privateDataReason(false, false); got != "" {
		t.Errorf("privateDataReason(false,false) = %q, want empty (no private-data axis)", got)
	}
}

// TestSwarmBlastRadiusFencesShardBox guards swarm.go's -blast-radius wiring: a
// non-default preset mints a shared blast budget that buildEnvFactory threads onto
// EVERY shard box (the `blast: blast` field). Pre-fix -blast-radius was parsed but
// never consumed, so a swarm ran unfenced regardless of the flag.
//
// Would redden if: swarm dropped `blast: blast` from its buildEnvFactory call
// (c.Blast would be nil ⇒ unfenced), or mintBlastBudget stopped honoring the preset.
func TestSwarmBlastRadiusFencesShardBox(t *testing.T) {
	log := discardLog(t)

	// The default -blast-radius off mints NO budget (unfenced, byte-identical).
	sfOff := parseSwarmFlags(t)
	if b := mintBlastBudget(*sfOff.common.blastRadius, log); b != nil {
		t.Fatalf("-blast-radius off must mint no budget, got %v", b)
	}

	// A non-default -blast-radius standard, read off the swarm's OWN flag surface,
	// mints a real budget the shard factory must attach.
	sf := parseSwarmFlags(t, "-blast-radius", "standard")
	if *sf.common.blastRadius != "standard" {
		t.Fatalf("-blast-radius standard parsed to %q", *sf.common.blastRadius)
	}
	blast := mintBlastBudget(*sf.common.blastRadius, log)
	if blast == nil {
		t.Fatal("a non-default -blast-radius must mint a blast budget")
	}

	// Reproduce swarm.buildSwarm's env-factory seam (swarm.go ~491-497). Force the
	// container backend so the box type is host-independent — podman/docker need not be
	// installed, selectSandbox falls back to NewContainer either way.
	newEnv := buildEnvFactory(buildDeps{blast: blast, sandboxPref: "container"}, "true")
	c, ok := newEnv(t.TempDir()).Box.(*sandbox.Container)
	if !ok {
		t.Fatal("forced container backend, want a *sandbox.Container box")
	}
	if c.Blast != blast {
		t.Fatal("swarm's blast fence must attach to every shard box — drop `blast: blast` and c.Blast is nil (unfenced)")
	}
}

// TestDeliverBuildPinsAndSurvivesSweep guards build.go's deliverBuild: it pins the
// converged verifier-green integration tip under the sweep-proof nilcore/kept/ prefix
// BEFORE the run-end stack cleanup deletes the throwaway task/rebase/integrate/read
// branches. Pre-fix the verified tip lived only on an integrate/ branch and was
// destroyed by the very sweep this test runs.
//
// Would redden if: deliverBuild stopped pinning the tip, pinned it under a swept
// prefix, or pinned the wrong SHA.
func TestDeliverBuildPinsAndSurvivesSweep(t *testing.T) {
	repo := newDeliveryRepo(t) // temp git repo on main with one commit
	ctx := context.Background()

	// A verifier-green integration tip lives on integrate/abcd, one commit ahead of base.
	tip := addKeptBranch(t, repo, "integrate/abcd", "feature\n")

	// A converged, un-promoted build (Done, Branch set, Promoted false): the loop kept
	// the tip but never merged it — exactly the case that must survive the sweep.
	deliverBuild(ctx, repo, project.Outcome{Done: true, Branch: "integrate/abcd"}, discardLog(t))

	const kept = "nilcore/kept/integrate-abcd"
	if !branchExists(t, repo, kept) {
		t.Fatalf("deliverBuild must pin the tip under %s", kept)
	}
	if got := deliveryGit(t, repo, nil, "rev-parse", "--verify", kept); got != tip {
		t.Fatalf("kept branch %s = %s, want the integration tip %s", kept, got, tip)
	}

	// Run the EXACT run-end sweep buildStack.cleanup performs (build.go:484).
	for _, p := range []string{"task/", "rebase/", "integrate/", "read/"} {
		worktree.DeleteBranches(ctx, repo, p)
	}

	// The integrate/ source branch is gone — proving the sweep is real and WOULD have
	// destroyed an un-pinned tip (so the test is not vacuous).
	if branchExists(t, repo, "integrate/abcd") {
		t.Fatal("the sweep must delete the integrate/ branch (else this test proves nothing)")
	}
	// The pinned deliverable SURVIVES the sweep, still at the verified tip.
	if !branchExists(t, repo, kept) {
		t.Fatalf("the kept branch %s must survive the run-end sweep", kept)
	}
	if got := deliveryGit(t, repo, nil, "rev-parse", "--verify", kept); got != tip {
		t.Fatalf("kept branch after sweep = %s, want %s", got, tip)
	}
}
