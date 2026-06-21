// browse.go wires the `nilcore browse` subcommand (Phase 14, docs/ROADMAP-BROWSER-USE.md):
// from one goal it drives a persistent, in-sandbox browser through an
// observe→plan→act→verify loop. It is purely WIRING over leaves that already
// exist: the persistent session (internal/browsersession) over the pure-Go CDP
// driver, the stateful browse tool + bounded loop (internal/browseragent), the
// trusted plan-then-verify system prompt (internal/browseragent/plan), the
// Rule-of-Two capability gate (internal/capguard), the named browse egress
// preset, and the SAME native backend, egress proxy, sandbox, and verifier the
// run/serve paths use.
//
// Four properties are load-bearing:
//   - The browser runs INSIDE the sandbox (I4): the session launches the in-image
//     nilcore-browser daemon via box.Exec and drives it over a file-queue on the
//     shared /work mount — never a host-side browser.
//   - Secrets stay host-side (I3): a {{secret:NAME}} placeholder is resolved from
//     the SecretStore at type time and never enters the model context or the log.
//   - The Rule of Two is enforced in code (capguard), not the prompt: untrusted
//     web + private data + open egress never run together unattended; the human
//     gate decides when all three are unavoidable, and a headless run fails closed.
//   - The browse loop never decides "done" — the verifier does (I2). Page content
//     is fenced as untrusted data (I7) by the browse tool.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/artifact/packs/ui"
	"nilcore/internal/backend"
	"nilcore/internal/browseragent"
	"nilcore/internal/browseragent/plan"
	"nilcore/internal/browsersession"
	"nilcore/internal/capguard"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// defaultBrowseImage is the sandbox image `nilcore browse` uses by default: the
// project's browser image, which carries a pinned headless Chromium and the
// operator-trusted nilcore-browser driver on $PATH (images/sandbox/Dockerfile).
// The default build image has neither, so browse points at the browser image and
// the operator builds/tags it (or overrides with -image).
const defaultBrowseImage = "nilcore/sandbox:latest"

type browseFlags struct {
	goal        *string
	url         *string
	dir         *string
	profile     *string
	check       *string
	runtime     *string
	image       *string
	sandboxPref *string
	logPath     *string
	config      *string
	maxSteps    *int
	deadline    *time.Duration
	readRepo    *bool
	extract     *string
	model       *string
}

func registerBrowseFlags(fs *flag.FlagSet) browseFlags {
	return browseFlags{
		goal:        fs.String("goal", "", "the browsing task, in plain language (required)"),
		url:         fs.String("url", "", "initial URL to open (optional; the agent can also navigate)"),
		dir:         fs.String("dir", ".", "working directory the sandbox is rooted at"),
		profile:     fs.String("egress-profile", "browse", "named egress preset the browser may reach (browse|web-research|docs|finance); widened further by a project-local .nilcore/egress.json"),
		check:       fs.String("check", "", "optional verifier command that governs done-ness (default: none — the model's finish ends the run; use -check or evidence packs for machine governance)"),
		runtime:     fs.String("runtime", "podman", "container runtime: podman | docker"),
		image:       fs.String("image", defaultBrowseImage, "sandbox image carrying chromium + the nilcore-browser driver"),
		sandboxPref: fs.String("sandbox", "container", "sandbox backend (egress allowlist needs container; namespace has no allowlist proxy)"),
		logPath:     fs.String("log", "nilcore.events.jsonl", "append-only event log path"),
		config:      fs.String("config", "", "config file from `nilcore init` (default: <config-dir>/config.json)"),
		maxSteps:    fs.Int("max-steps", 40, "browse action budget (a hard loop bound)"),
		deadline:    fs.Duration("deadline", 15*time.Minute, "wall-clock ceiling for the whole run"),
		readRepo:    fs.Bool("read", false, "also mount read-only repo tools (adds the private-data axis to the Rule-of-Two check)"),
		extract:     fs.String("extract", "", "extraction mode: record findings as a verifier-gated artifact at this id (e.g. -extract release-facts); the harness re-derives every finding before the run is done (I2)"),
		model:       fs.String("model", "", "the single model for this browse run (default: "+defaultGUIModel+", a strong GUI model; or set NILCORE_BROWSE_MODEL)"),
	}
}

// validExtractID reuses the artifact id rule so a bad -extract value fails at the
// front door, not at write time.
func validExtractID(id string) bool {
	return id != "" && id != "." && id != ".." &&
		!strings.ContainsAny(id, "/\\") && !strings.HasPrefix(id, ".")
}

func browseMain(args []string) {
	fs := flag.NewFlagSet("browse", flag.ExitOnError)
	bf := registerBrowseFlags(fs)
	_ = fs.Parse(args)

	if strings.TrimSpace(*bf.goal) == "" {
		fmt.Fprintln(os.Stderr, "error: -goal is required\nrun 'nilcore browse -h' for usage")
		os.Exit(2)
	}

	b := loadBoot(*bf.config)
	log := openLog(*bf.logPath)
	defer log.Close()

	// Browser use runs on a SINGLE model set for the feature (parity with desktop):
	// -model, then NILCORE_BROWSE_MODEL, then a strong GUI default (Opus 4.8).
	prov, err := resolveNativeSpec(guiModelSpec(*bf.model, os.Getenv("NILCORE_BROWSE_MODEL")), b)
	if err != nil {
		fatal(err)
	}

	absDir, err := filepath.Abs(*bf.dir)
	if err != nil {
		fatal(fmt.Errorf("resolving -dir: %w", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *bf.deadline)
	defer cancel()

	// Egress: the browse preset (or another named profile) is the proxy allowlist,
	// unioned with a project-local .nilcore/egress.json. Fail-closed on an unknown
	// name. browse REQUIRES a container backend for the allowlist proxy.
	prof, perr := resolveEgressProfile(b.cfg, *bf.profile)
	if perr != nil {
		fatal(perr)
	}
	emitEgressProfile(log, prof, egressBackendLabel(*bf.sandboxPref))
	egress, proxyAddr, stopProxy, _ := startEgressProxy(ctx, prof.Tree.Allowed)
	defer stopProxy()

	box := selectSandbox(*bf.sandboxPref, *bf.runtime, *bf.image, absDir)
	applyContainerEgress(box, egress, proxyAddr, *bf.runtime)
	if _, ok := box.(*sandbox.Container); !ok {
		fmt.Fprintln(os.Stderr, "nilcore browse: WARNING — a non-container sandbox has no egress allowlist proxy; the browser will be unable to reach any host. Use -sandbox container.")
	}

	// Rule of Two (capguard): a browse agent always ingests untrusted input (A).
	// Private data (B) is on only when repo read tools are mounted. Open egress (C)
	// is derived from the resolved allowlist (a wildcard or a broad list). All three
	// at once requires the human gate; headless with no gate fails closed.
	approver := policy.NewConsoleApprover(os.Stdin, os.Stdout)
	caps := capguard.Capabilities{
		UntrustedInput: true,
		PrivateData:    *bf.readRepo,
		EgressHosts:    egress.Allowed,
		Reasons: map[string]string{
			"A": "browse-agent",
			"B": ternary(*bf.readRepo, "repo-read-mounted", ""),
			"C": "profile:" + *bf.profile,
		},
	}
	dec := capguard.Evaluate(caps, true)
	log.Append(eventlog.Event{Kind: "capguard", Detail: map[string]any{
		"verdict": string(dec.Verdict), "axes": dec.Axes, "detail": dec.Detail,
	}})
	switch dec.Verdict {
	case capguard.Refuse:
		fatal(fmt.Errorf("browse refused by the Rule of Two: %s", dec.Detail))
	case capguard.GateRequired:
		fmt.Fprintf(os.Stderr, "nilcore browse: %s\n", dec.Detail)
		if !approver.Approve("run a browse session combining untrusted web input, private data, and open egress (" + dec.Detail + ")") {
			fatal(fmt.Errorf("browse denied at the human gate (Rule of Two)"))
		}
	}

	// Secret resolver: {{secret:NAME}} is resolved env-first then SecretStore (I3),
	// host-side, and never reaches the model context or the log.
	secrets := func(name string) (string, bool) {
		v := strings.TrimSpace(b.cred(name))
		return v, v != ""
	}

	driver := strings.TrimSpace(os.Getenv("NILCORE_BROWSER"))
	if driver == "" {
		driver = "nilcore-browser"
	}

	sess, first, err := browsersession.Launch(ctx, box, browsersession.Options{
		Driver:     driver,
		InitialURL: *bf.url,
		Secrets:    secrets,
	})
	if err != nil {
		fatal(fmt.Errorf("browse: launching session: %w", err))
	}
	defer sess.Close()
	if first.URL != "" {
		fmt.Fprintf(os.Stderr, "nilcore browse: session up at %s (%d elements)\n", first.URL, len(first.Refs))
	}

	extractID := strings.TrimSpace(*bf.extract)
	if extractID != "" && !validExtractID(extractID) {
		fatal(fmt.Errorf("-extract id %q must be a single safe path component (no separators, no leading dot)", extractID))
	}

	// The tool surface: the stateful browse tool (no shell — a browse agent has no
	// arbitrary-execution path, structurally). In extraction mode, record_finding
	// writes verifier-gated findings. Optionally read-only repo tools.
	bt := &browseragent.BrowseTool{Sess: sess, MaxSteps: *bf.maxSteps, EventSink: browseEventSink(log)}
	reg := tools.NewRegistry(bt)
	if extractID != "" {
		reg.Register(&browseragent.FindingTool{Root: absDir, ArtifactID: extractID, Title: *bf.goal, Sess: sess})
	}
	if *bf.readRepo {
		reg.Register(tools.ReadTool{})
		reg.Register(tools.SearchTool{})
	}

	// The verifier governs done-ness (I2):
	//   -extract ⇒ a lazy ArtifactVerifier re-derives every recorded finding in-box
	//             (ui.value_present): the run is done only when each finding confirmed.
	//   -check   ⇒ the project/behavioral verifier governs.
	//   neither  ⇒ a pure browse/research run has no machine-checkable artifact, so
	//             done is the model's finish over a pass verifier (read-only-mode parity).
	var v verify.Verifier = verify.Pass{}
	switch {
	case extractID != "":
		v = lazyBrowseExtractVerifier{box: box, log: log}
	case strings.TrimSpace(*bf.check) != "":
		v = behavioralVerifierWithLog(box, *bf.check, log)
	}

	n := &backend.Native{
		Model:        prov,
		Box:          box,
		Verifier:     v,
		Log:          log,
		Tools:        reg,
		DisableShell: true, // a browse agent has no shell — the no-arbitrary-execution guarantee is structural
		MaxSteps:     *bf.maxSteps + 8,
		System:       plan.SystemPrompt(*bf.goal, extractID != ""),
	}

	out, err := n.Run(ctx, backend.Task{ID: "browse-" + shortID(), Dir: absDir, Goal: *bf.goal})
	if err != nil {
		fatal(fmt.Errorf("browse: %w", err))
	}
	fmt.Println(strings.TrimSpace(out.Summary))
}

// ternary is a tiny helper for the capguard reason map.
func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// browseEventSink adapts the browse tool's trajectory Steps to the append-only
// event log (I5) as metadata-only browse_step events — the op, the page URL
// (provenance, key-free), and counts; never the untrusted page body (I7). nil log
// ⇒ nil sink ⇒ no events (byte-identical).
func browseEventSink(log *eventlog.Log) browseragent.EventSink {
	if log == nil {
		return nil
	}
	return func(s browseragent.Step) {
		log.Append(eventlog.Event{Kind: "browse_step", Detail: map[string]any{
			"n": s.N, "op": s.Op, "url": s.URL, "refs": s.Refs,
			"version": s.Version, "stagnant": s.Stagnant, "error": s.Errored,
		}})
	}
}

// lazyBrowseExtractVerifier governs an extraction run (I2): at Check time it
// discovers the artifact the agent wrote DURING the run (it does not exist at
// construction time), binds the ui pack's ui.value_present check, and re-derives
// every finding in-box. No findings recorded ⇒ RED (an extract run that asserts
// nothing is not done). The verifier — never the agent's report — decides done.
type lazyBrowseExtractVerifier struct {
	box sandbox.Sandbox
	log *eventlog.Log
}

func (l lazyBrowseExtractVerifier) Check(ctx context.Context) (verify.Report, error) {
	extras := browseExtractVerifiers(l.box, l.log)
	if len(extras) == 0 {
		return verify.Report{Passed: false, Output: "no findings recorded — call record_finding for each datum the goal asks for"}, nil
	}
	return verify.Composite{Named: extras}.Check(ctx)
}

// browseExtractVerifiers builds one ArtifactVerifier per artifact file the run
// wrote, against a registry of generic stdlib checks + the ui pack (so a claim
// naming ui.value_present resolves to the real re-derivation check). It reuses the
// app-level artifactFiles discovery and evidence event sink — the same machinery
// the evidence-verify path uses — so a browse finding is judged exactly like every
// other NilCore artifact claim.
func browseExtractVerifiers(box sandbox.Sandbox, log *eventlog.Log) []verify.NamedVerifier {
	if box == nil {
		return nil
	}
	paths := artifactFiles(box.Workdir())
	if len(paths) == 0 {
		return nil
	}
	reg := evverify.Default()
	ui.RegisterAll(reg)
	sink := evidenceEventSink(log)
	out := make([]verify.NamedVerifier, 0, len(paths))
	for _, p := range paths {
		out = append(out, verify.NamedVerifier{
			Name: "extract:" + artifactID(p),
			V:    &evverify.ArtifactVerifier{Box: box, Reg: reg, RelPath: p, EventSink: sink},
		})
	}
	return out
}
