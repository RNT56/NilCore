package main

// Pillar-5 research egress profiles (P11-T28) — the SINGLE audited toggle that
// widens the sandbox's default-deny tree from nothing to a sanctioned, named host
// set. A profile is opted in three ways, in precedence order: the -egress-profile
// flag, the NILCORE_EGRESS_PROFILE env var, then the persisted config
// (cfg.Web.Profile / cfg.Web.ProfileFile from `nilcore init`). With none set the
// path is byte-identical: resolveEgressProfile returns an empty policy.Egress, no
// sources, no event — exactly the deny-all the front doors had before.
//
// The security direction is precise and one-way: a profile WIDENS the tree, but
// roster.EgressFor still intersects each role's own allowlist against that tree
// (narrow-only, R9), so a deny-all role stays --network none under any profile.
// The resolved tree feeds resolveWeb (the chat/serve web_fetch path) and build.go's
// per-role tree; the rest of the machinery is untouched.
//
// I3: the resolved tree holds hostnames ONLY. The metadata-only egress_profile
// event carries {profile,file,host_count,sources,backend} — never a host with a
// query string, never a key. Keyed sources resolve their key value from the
// SecretStore and inject it via box.ExecWithEnv at the verifier-pack layer, never
// here.

import (
	"fmt"
	"os"

	"nilcore/internal/egressprofile"
	"nilcore/internal/eventlog"
	"nilcore/internal/onboard"
	"nilcore/internal/policy"
)

// egressProfileEnv is the env var that opts a research egress profile in without a
// flag (parity with NILCORE_WEB_ALLOW). It names a built-in preset from
// egressprofile.Names(); an unknown name is a loud fail-closed error.
const egressProfileEnv = "NILCORE_EGRESS_PROFILE"

// egressProfile is the resolved outcome of the flag/env/config precedence: the
// widened tree allowlist plus the metadata for the egress_profile event. A zero
// egressProfile (Tree empty, no Profile/File, no Sources) is the byte-identical
// default — On() reports false and the front doors keep their deny-all behavior.
type egressProfile struct {
	Tree    policy.Egress // the resolved widen-tree (preset hosts ∪ project-file hosts)
	Profile string        // the selected preset name ("" ⇒ none)
	File    string        // the project-local allowlist path ("" ⇒ none)
	Sources []string      // provenance: "profile:<name>", "file:<path>" (for the event)
}

// On reports whether a profile (or project-local file) was opted in. When false
// the resolved tree is empty and every downstream path is byte-identical.
func (p egressProfile) On() bool { return p.Profile != "" || p.File != "" }

// resolveEgressProfile applies the flag > env > config precedence for the named
// preset, pairs it with the persisted project-local allowlist file (config only —
// the file path is not a flag), and unions them into one widen-tree via
// egressprofile.Resolve. An unknown profile name (flag or env) or an unparseable
// project-local file is a FAIL-CLOSED error: the caller must keep deny-all, never
// fail open. Nothing opted in ⇒ a zero egressProfile and nil error (the default).
func resolveEgressProfile(cfg onboard.Config, flagProfile string) (egressProfile, error) {
	// Precedence: an explicit flag wins, else the env var, else the persisted config.
	profile := flagProfile
	if profile == "" {
		profile = os.Getenv(egressProfileEnv)
	}
	if profile == "" {
		profile = cfg.Web.Profile
	}
	file := cfg.Web.ProfileFile // the project-local file path is config-only

	if profile == "" && file == "" {
		return egressProfile{}, nil // byte-identical default: no widen
	}

	tree, sources, err := egressprofile.Resolve(profile, file)
	if err != nil {
		// Unknown preset name or unparseable file: fail closed so the caller keeps
		// deny-all rather than silently widening (or fail-opening) the sandbox.
		return egressProfile{}, fmt.Errorf("resolving egress profile: %w", err)
	}
	return egressProfile{Tree: tree, Profile: profile, File: file, Sources: sources}, nil
}

// emitEgressProfile records the single metadata-only egress_profile event so the
// Pillar-6 report can show the audited widen. It is emitted ONLY when a profile is
// opted in (On() ⇒ true); the default path emits nothing (byte-identical). The
// Detail carries no hostnames-with-query-strings and no keys (I3): only the preset
// name, the file path, the host COUNT, the provenance sources, and which sandbox
// backend the profile actually applies to. backend distinguishes the *Container
// path (the profile widens the proxy tree) from the namespace path (no proxy path —
// the box stays hard deny-all and the profile is surfaced as inert).
func emitEgressProfile(log *eventlog.Log, p egressProfile, backend string) {
	if log == nil || !p.On() {
		return
	}
	log.Append(eventlog.Event{
		Kind: "egress_profile",
		Detail: map[string]any{
			"profile":    p.Profile,
			"file":       p.File,
			"host_count": len(p.Tree.Allowed),
			"sources":    p.Sources,
			"backend":    backend,
		},
	})
}

// egressBackendLabel names the sandbox backend a resolved profile actually applies
// to, for the egress_profile event and the namespace-degrade diagnostic. The
// namespace backend has no proxy egress path (CLONE_NEWNET with no interfaces), so
// a profile there is inert: applyContainerEgress no-ops and the box stays hard
// deny-all. We surface that LOUDLY rather than silently appear to widen.
func egressBackendLabel(sandboxPref string) string {
	if sandboxPref == "namespace" {
		return "namespace"
	}
	return "container"
}

// warnNamespaceEgress prints the loud stderr diagnostic when a profile is opted in
// but the chosen sandbox backend is the namespace backend, which has no proxy path:
// the profile cannot take effect and the box stays deny-all. Paired with the
// backend=namespace field on the egress_profile event so the audit trail records
// the inert widen.
func warnNamespaceEgress(p egressProfile, sandboxPref string) {
	if !p.On() || egressBackendLabel(sandboxPref) != "namespace" {
		return
	}
	fmt.Fprintf(os.Stderr,
		"nilcore: egress profile %q has no effect under the namespace sandbox backend "+
			"(no proxy egress path) — the sandbox stays deny-all; use -sandbox container/auto to widen\n",
		p.Profile)
}
