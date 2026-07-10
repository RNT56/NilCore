// desktop.go wires the `nilcore desktop` subcommand (Phase CU,
// docs/ROADMAP-COMPUTER-USE.md, CU-T09): from one goal it drives a persistent,
// in-sandbox virtual desktop through an observe→plan→act→verify loop. It is the
// sibling of cmd/nilcore/browse.go — same native backend, egress proxy, sandbox,
// capguard Rule-of-Two gate, and verifier — over the desktop session + the thin
// `computer` tool (Path B). The fat perception/actuation (the Set-of-Marks ladder,
// xdotool/scrot/AT-SPI) lives in the in-image nilcore-desktop driver.
//
// GATED + opt-in: the whole tier is the "general computer operator" identity step,
// so `nilcore desktop` refuses to run unless NILCORE_COMPUTER_USE is set (the
// inert-when-off discipline). The desktop image (nilcore/sandbox-desktop) carries
// Xvfb + the driver; nothing pulls it by default.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/capguard"
	"nilcore/internal/desktopagent"
	"nilcore/internal/desktopsession"
	"nilcore/internal/desktopwire"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/tools"
	"nilcore/internal/verify"
)

// computerToolRegistry builds the desktop tool surface: the stateful `computer`
// tool (no shell — a desktop agent has no arbitrary-execution path) plus optional
// read-only repo tools when -read is set.
func computerToolRegistry(ct tools.Tool, readRepo bool) *tools.Registry {
	reg := tools.NewRegistry(ct)
	if readRepo {
		reg.Register(tools.ReadTool{})
		reg.Register(tools.SearchTool{})
	}
	return reg
}

// defaultDesktopImage carries Xvfb + a WM + AT-SPI + xdotool/scrot + the
// nilcore-desktop driver (images/sandbox-desktop/Dockerfile). The operator builds/
// tags it (or overrides with -image); nothing pulls it by default.
const defaultDesktopImage = "nilcore/sandbox-desktop:latest"

type desktopFlags struct {
	goal        *string
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
	native      *bool
	model       *string
	macHost     *bool
	macProbe    *bool
	secrets     *string
}

func registerDesktopFlags(fs *flag.FlagSet) desktopFlags {
	return desktopFlags{
		goal:        fs.String("goal", "", "the desktop task, in plain language (required)"),
		dir:         fs.String("dir", ".", "working directory the sandbox is rooted at"),
		profile:     fs.String("egress-profile", "", "optional named egress preset the desktop may reach (default: deny-all — most desktop tasks are offline)"),
		check:       fs.String("check", "", "optional verifier command that governs done-ness (default: none — the model's finish ends the run)"),
		runtime:     fs.String("runtime", "podman", "container runtime: podman | docker"),
		image:       fs.String("image", defaultDesktopImage, "desktop sandbox image (Xvfb + the nilcore-desktop driver)"),
		sandboxPref: fs.String("sandbox", "container", "sandbox backend (the desktop image needs a container/microVM, not the namespace backend)"),
		logPath:     fs.String("log", "nilcore.events.jsonl", "append-only event log path"),
		config:      fs.String("config", "", "config file from `nilcore init` (default: <config-dir>/config.json)"),
		maxSteps:    fs.Int("max-steps", 50, "action budget (a hard loop bound)"),
		deadline:    fs.Duration("deadline", 20*time.Minute, "wall-clock ceiling for the whole run"),
		readRepo:    fs.Bool("read", false, "also mount read-only repo tools (adds the private-data axis to the Rule-of-Two check)"),
		native:      fs.Bool("native", false, "Path A: hand the Rung-3 pixel-grounding sub-call to Anthropic's native computer tool (NILCORE_COMPUTER_NATIVE)"),
		model:       fs.String("model", "", "the single model for this computer-use run (default: "+defaultGUIModel+", a strong GUI model; or set NILCORE_COMPUTER_MODEL)"),
		macHost:     fs.Bool("mac-host", false, "NATIVE macOS HOST CONTROL: drive your REAL Mac desktop (UNSANDBOXED — host ambient authority, I4 relaxed). Requires NILCORE_DESKTOP_HOST=1 + the nilcore-desktop-darwin driver on PATH. See docs/ROADMAP-COMPUTER-USE-DARWIN.md"),
		macProbe:    fs.Bool("mac-probe", false, "check macOS host-control readiness (Screen Recording + cliclick/Accessibility) and exit non-zero if not ready — a host-readiness gate, no goal needed"),
		secrets:     fs.String("secrets", "", "comma-separated ALLOWLIST of secret names the agent may type via {{secret:NAME}} (also NILCORE_DESKTOP_SECRETS); default empty ⇒ no secret may be typed (fail closed) — the fence against typing an arbitrary env var into a field"),
	}
}

func desktopMain(args []string) {
	fs := flag.NewFlagSet("desktop", flag.ExitOnError)
	df := registerDesktopFlags(fs)
	_ = fs.Parse(args)

	// --mac-probe is a standalone host-readiness check (CU-MAC-T07): no goal, no gate
	// (it's read-only onboarding). It shells to the darwin driver's --probe mode and
	// mirrors its exit status, so it works as a CI/pre-flight gate.
	if *df.macProbe {
		driver := strings.TrimSpace(os.Getenv("NILCORE_DESKTOP_DRIVER"))
		if driver == "" {
			driver = "nilcore-desktop-darwin"
		}
		cmd := exec.Command(driver, "--probe") //nolint:gosec // operator-trusted driver name
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "nilcore desktop: host not ready (or %q not on PATH — build it: go build -o %s ./cmd/tools/nilcore-desktop-darwin)\n", driver, driver)
			os.Exit(1)
		}
		return
	}

	// GATE: the tier is inert unless explicitly opted into (the identity step).
	if strings.TrimSpace(os.Getenv("NILCORE_COMPUTER_USE")) == "" {
		fmt.Fprintln(os.Stderr, "nilcore desktop: refusing to run — desktop computer use is a gated capability.\nSet NILCORE_COMPUTER_USE=1 to opt in (see docs/ROADMAP-COMPUTER-USE.md §0).")
		os.Exit(2)
	}
	if strings.TrimSpace(*df.goal) == "" {
		fmt.Fprintln(os.Stderr, "error: -goal is required\nrun 'nilcore desktop -h' for usage")
		os.Exit(2)
	}

	b := loadBoot(*df.config)
	log := openLog(*df.logPath)
	defer log.Close()

	// Computer use runs on a SINGLE model set for the feature (CU-T11): the -model
	// flag, then NILCORE_COMPUTER_MODEL, then a strong GUI default (Opus 4.8) — not
	// the general executor config, since GUI grounding wants a capable model.
	prov, err := resolveNativeSpec(guiModelSpec(*df.model, os.Getenv("NILCORE_COMPUTER_MODEL")), b)
	if err != nil {
		fatal(err)
	}
	absDir, err := filepath.Abs(*df.dir)
	if err != nil {
		fatal(fmt.Errorf("resolving -dir: %w", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *df.deadline)
	defer cancel()

	native := *df.native || strings.TrimSpace(os.Getenv("NILCORE_COMPUTER_NATIVE")) != ""
	approver := policy.NewConsoleApprover(os.Stdin, os.Stdout)

	// Operator-declared secret-name allowlist (fail-closed default empty): the only names
	// the agent may resolve via {{secret:NAME}}. Wrapping the env-first resolver in
	// AllowlistResolver is the exfil fence — an unlisted name (e.g. {{secret:ANTHROPIC_API_KEY}})
	// resolves to not-found, so substituteSecrets refuses to type it. It also feeds the
	// Rule-of-Two axis B in the contained-mode capguard below. Applies to BOTH the host and
	// contained paths.
	secretNames := parseSecretNames(*df.secrets, os.Getenv("NILCORE_DESKTOP_SECRETS"))
	secretCapable := len(secretNames) > 0
	secrets := desktopsession.AllowlistResolver(secretNames, func(name string) (string, bool) {
		v := strings.TrimSpace(b.cred(name))
		return v, v != ""
	})

	var (
		box   sandbox.Sandbox
		sess  *desktopsession.Session
		first desktopwire.Observation
		v     verify.Verifier = verify.Pass{}
	)

	if *df.macHost {
		// ── Native macOS HOST CONTROL (CU-MAC) — the louder-gated, UNSANDBOXED tier ──
		// I4 is relaxed: this drives the user's REAL desktop. It requires the separate
		// NILCORE_DESKTOP_HOST opt-in so a sandboxed run can never silently become host
		// control, and forces an unconditional human gate.
		if strings.TrimSpace(os.Getenv("NILCORE_DESKTOP_HOST")) != "1" {
			fatal(fmt.Errorf("--mac-host drives your REAL Mac desktop UNSANDBOXED (host ambient authority). Set NILCORE_DESKTOP_HOST=1 to confirm you accept this. See docs/ROADMAP-COMPUTER-USE-DARWIN.md §0"))
		}
		fmt.Fprintln(os.Stderr, "nilcore desktop: ⚠️  HOST CONTROL — the agent will drive your REAL macOS desktop (NOT sandboxed). Grant Accessibility + Screen Recording to your terminal, and watch it. Press Ctrl-C to abort.")
		fmt.Fprintln(os.Stderr, "nilcore desktop:    Safety: `nilcore desktop --mac-probe` checks readiness first; set NILCORE_DESKTOP_ALLOW_APPS=\"App1,App2\" to pin acting to those apps; `touch ~/.nilcore/desktop/STOP` (or $NILCORE_DESKTOP_STOP) halts all actuation instantly.")
		log.Append(eventlog.Event{Kind: "capguard", Detail: map[string]any{"verdict": "host-gate", "axes": []string{"A", "B", "C"}, "detail": "macOS host control — unsandboxed ambient authority"}})
		if !approver.Approve("drive your REAL macOS desktop (UNSANDBOXED host control) toward: " + *df.goal) {
			fatal(fmt.Errorf("host control denied at the gate"))
		}
		if strings.TrimSpace(*df.check) != "" {
			fmt.Fprintln(os.Stderr, "nilcore desktop: -check is ignored in --mac-host (no sandbox to verify in).")
		}
		hostDriver := strings.TrimSpace(os.Getenv("NILCORE_DESKTOP_DRIVER"))
		if hostDriver == "" {
			hostDriver = "nilcore-desktop-darwin"
		}
		sess, first, err = desktopsession.LaunchHost(ctx, desktopsession.HostOptions{Driver: hostDriver, Native: native, Secrets: secrets})
		if err != nil {
			fatal(fmt.Errorf("desktop: launching host session (is %q on PATH? build it: go build -o %s ./cmd/tools/nilcore-desktop-darwin): %w", hostDriver, hostDriver, err))
		}
	} else {
		// ── Contained desktop (Linux container/microVM) — the I4-compliant default ──
		prof, perr := resolveEgressProfile(b.cfg, *df.profile)
		if perr != nil {
			fatal(perr)
		}
		emitEgressProfile(log, prof, egressBackendLabel(*df.sandboxPref))
		egress, proxyAddr, stopProxy, _ := startEgressProxy(ctx, prof.Tree.Allowed, nil, proxyBindAddr(*df.sandboxPref, *df.runtime))
		defer stopProxy()

		box = selectSandbox(*df.sandboxPref, *df.runtime, *df.image, absDir)
		applyContainerEgress(box, egress, proxyAddr, *df.runtime)
		if _, ok := box.(*sandbox.Container); !ok {
			fmt.Fprintln(os.Stderr, "nilcore desktop: WARNING — the desktop image needs a container (or microVM) sandbox; the namespace backend has no X11 desktop. Use -sandbox container.")
		}

		// Private data (axis B) is on when repo read tools are mounted OR a secret allowlist
		// is declared — a session that can type a site credential holds private data even
		// with -read=false, so it must count axis B (otherwise it would evade the gate).
		caps := capguard.Capabilities{
			UntrustedInput: true,
			PrivateData:    *df.readRepo || secretCapable,
			EgressHosts:    egress.Allowed,
			Reasons: map[string]string{
				"A": "desktop-agent",
				"B": privateDataReason(*df.readRepo, secretCapable),
				"C": ternary(*df.profile != "", "profile:"+*df.profile, ""),
			},
		}
		dec := capguard.Evaluate(caps, true)
		log.Append(eventlog.Event{Kind: "capguard", Detail: map[string]any{"verdict": string(dec.Verdict), "axes": dec.Axes, "detail": dec.Detail}})
		switch dec.Verdict {
		case capguard.Refuse:
			fatal(fmt.Errorf("desktop refused by the Rule of Two: %s", dec.Detail))
		case capguard.GateRequired:
			fmt.Fprintf(os.Stderr, "nilcore desktop: %s\n", dec.Detail)
			if !approver.Approve("run a desktop session combining untrusted screen input, private data, and open egress (" + dec.Detail + ")") {
				fatal(fmt.Errorf("desktop denied at the human gate (Rule of Two)"))
			}
		}

		driver := strings.TrimSpace(os.Getenv("NILCORE_DESKTOP_DRIVER"))
		if driver == "" {
			driver = "nilcore-desktop"
		}
		sess, first, err = desktopsession.Launch(ctx, box, desktopsession.Options{Driver: driver, Native: native, Secrets: secrets})
		if err != nil {
			fatal(fmt.Errorf("desktop: launching session: %w", err))
		}
		if strings.TrimSpace(*df.check) != "" {
			v = behavioralVerifierWithLog(box, *df.check, log)
		}
	}
	defer sess.Close()
	fmt.Fprintf(os.Stderr, "nilcore desktop: session up (window %q, %d elements, rung %d)\n", first.FocusedWindow, len(first.Refs), first.Rung)

	// Path B (default) advertises our generic `computer` tool; Path A (-native /
	// NILCORE_COMPUTER_NATIVE) advertises Anthropic's native `computer` beta tool,
	// translating its actions to the SAME driver. Both share the governed body.
	// The console approver (also the Rule-of-Two / host-control session gate above) is the
	// per-action gate. In CONTAINED mode it routes a delete/pay/accept-terms click, or an
	// Enter-to-submit on such a dialog, through the human gate (deny-default headless),
	// classified from the accessible target — symmetric with the browse tier.
	// In HOST-CONTROL mode (--mac-host) that name-based classifier is BLIND: the CV-only
	// observation gives refs empty Name/Value and never sets FocusedWindow/Title, so no
	// click/type/key would match. GateAllMutations therefore routes EVERY mutating action
	// on the REAL desktop through the gate, so a destructive host action cannot slip past.
	var computer tools.Tool
	if native {
		computer = &desktopagent.NativeComputerTool{Sess: sess, MaxSteps: *df.maxSteps, EventSink: desktopEventSink(log), Approver: approver, GateAllMutations: *df.macHost}
		fmt.Fprintln(os.Stderr, "nilcore desktop: Path A (native Anthropic computer tool) enabled — pixel-mode, vendor-locked to Anthropic for this run.")
	} else {
		computer = &desktopagent.ComputerTool{Sess: sess, MaxSteps: *df.maxSteps, EventSink: desktopEventSink(log), Approver: approver, GateAllMutations: *df.macHost}
	}
	if *df.macHost {
		fmt.Fprintln(os.Stderr, "nilcore desktop: host-control per-action gate ARMED — every click/type/key/drag on your REAL desktop must be approved.")
	}
	reg := computerToolRegistry(computer, *df.readRepo)

	n := &backend.Native{
		Model:        prov,
		Box:          box,
		Verifier:     v,
		Log:          log,
		Tools:        reg,
		DisableShell: true, // a desktop agent has no shell — structural
		MaxSteps:     *df.maxSteps + 8,
		System:       desktopagent.SystemPrompt(*df.goal),
	}
	out, err := n.Run(ctx, backend.Task{ID: "desktop-" + shortID(), Dir: absDir, Goal: *df.goal})
	if err != nil {
		fatal(fmt.Errorf("desktop: %w", err))
	}
	fmt.Println(strings.TrimSpace(out.Summary))
}

// desktopEventSink adapts the computer tool's trajectory Steps to metadata-only
// desktop_step events (op/window/rung/refs — never the untrusted screen body, I7).
func desktopEventSink(log *eventlog.Log) desktopagent.EventSink {
	if log == nil {
		return nil
	}
	return func(s desktopagent.Step) {
		log.Append(eventlog.Event{Kind: "desktop_step", Detail: map[string]any{
			"n": s.N, "op": s.Op, "window": s.Window, "rung": s.Rung, "refs": s.Refs,
			"version": s.Version, "stagnant": s.Stagnant, "error": s.Errored,
		}})
	}
}
