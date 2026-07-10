// egress_hard.go implements the HARD egress lifecycle for the CONTAINER sandbox
// backend (opt-in NILCORE_EGRESS_HARD; Linux-container only, CI-validated — it is
// NEVER exercised on the macOS host, which is why the setup is behind an injectable
// seam that tests fake).
//
// WHY hard mode exists: the cooperative path (AllowEgressVia) attaches the sandbox to
// a bridged NAT network and points HTTP(S)_PROXY at the allowlist proxy — a model
// command that ignores the proxy (curl --noproxy, raw sockets, /dev/tcp) still
// reaches any host. Hard mode makes the allowlist UNBYPASSABLE: a per-run `--internal`
// docker/podman network has NO default route, so a container attached only to it can
// reach ONLY the internal subnet. We run the allowlist proxy as a DUAL-HOMED GATEWAY
// container (internal net + a normal net) and attach the SANDBOX to the internal net
// only, with HTTP(S)_PROXY pointed at the gateway. Cooperative traffic → gateway →
// allowlist; a raw socket / --noproxy has no route out and fails. The sandbox keeps
// --cap-drop=ALL, so escaping the internal net needs host root / NET_ADMIN.
//
// HONEST RESIDUALS (do NOT overclaim "empty-netns equivalent"):
//  1. The internal net still has DNS (aardvark/embedded), so a DNS-tunnel exfil is
//     only MITIGATED, not proven-closed — we point the sandbox's --dns at the gateway
//     (which serves no :53) to blackhole in-sandbox resolution; proxied traffic still
//     works because the client reaches the proxy by IP.
//  2. The gateway runs `nilcore egress-gateway`, so the sandbox IMAGE must contain the
//     nilcore binary — a debian:stable-slim does not, so hard mode requires the
//     nilcore/sandbox image (images/sandbox), not the default debian.
//  3. Linux-container only; the lifecycle here is CI-validated, never host-run.
//  4. The per-run network+gateway can leak on crash — teardown is idempotent and the
//     boot reaper (reapHardEgress) reclaims label-scoped orphans.
//
// The namespace backend's empty netns remains the RECOMMENDED hard boundary; this is
// the hard option for hosts that must run the container backend.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

// hardEgressLabel tags every per-run internal network + gateway container hard mode
// creates, so the boot reaper can reclaim orphans by label after a crash.
const hardEgressLabel = "nilcore-egr"

// hardGatewayPort is the fixed in-container port the gateway's allowlist proxy binds.
const hardGatewayPort = "3128"

// setupHardEgressFn is the INJECTABLE hard-egress setup seam. Production points it at
// setupHardEgress (which shells out to the container runtime); tests replace it with a
// fake so the STRICT/hard DECISION in applyContainerEgress is unit-testable on the
// macOS host, where no real container can run.
var setupHardEgressFn = setupHardEgress

// Per-process hard-egress state. applyContainerEgress runs PER DRIVE across
// goroutines, so hardCache reuses ONE gateway per (runtime,image,allowlist) rather
// than spawning a container per drive; hardTeardowns holds each gateway's idempotent
// teardown for a clean process-exit drain, backstopped by the boot reaper on crash.
var (
	hardMu        sync.Mutex
	hardTeardowns []func()
	hardCache     = map[string]hardEgressHandle{}
)

// hardEgressHandle is the wiring one hard gateway exposes to a sandbox box.
type hardEgressHandle struct {
	network  string // the --internal network name (attach the sandbox here)
	proxyURL string // HTTP(S)_PROXY value: the gateway's internal IP:port
	dns      string // --dns value: the gateway IP (blackholes in-sandbox DNS)
}

// getHardEgress returns the process's gateway for (runtime,image,egress), setting one
// up on first use. Success is cached (and its teardown registered); a FAILURE is NOT
// cached, so a transient runtime error can recover on a later drive. ok=false means
// setup failed and the caller must FAIL CLOSED (leave the box deny-all).
func getHardEgress(runtime, image string, egress policy.Egress) (hardEgressHandle, bool) {
	key := hardCacheKey(runtime, image, egress)
	hardMu.Lock()
	defer hardMu.Unlock()
	if h, ok := hardCache[key]; ok {
		return h, true
	}
	network, proxyURL, teardown, err := setupHardEgressFn(runtime, egress, image)
	if err != nil {
		return hardEgressHandle{}, false
	}
	h := hardEgressHandle{network: network, proxyURL: proxyURL, dns: hostFromProxyURL(proxyURL)}
	hardCache[key] = h
	if teardown != nil {
		hardTeardowns = append(hardTeardowns, teardown)
	}
	return h, true
}

// hardCacheKey is an order-independent key for a hard gateway: same runtime+image and
// same allowlist ⇒ same gateway reused across drives.
func hardCacheKey(runtime, image string, egress policy.Egress) string {
	hosts := append([]string(nil), egress.Allowed...)
	sort.Strings(hosts)
	return runtime + "|" + image + "|" + strings.Join(hosts, ",")
}

// hostFromProxyURL extracts the host (the gateway IP) from a "http://ip:port" proxy
// URL, so the same IP can pin the sandbox's --dns. "" if unparseable.
func hostFromProxyURL(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// stopHardEgress drains every registered gateway teardown (idempotent per entry) — the
// clean process-exit path. Safe to call when nothing was set up (a no-op), so the
// default (opt-out) path is unaffected.
func stopHardEgress() {
	hardMu.Lock()
	fns := hardTeardowns
	hardTeardowns = nil
	hardCache = map[string]hardEgressHandle{}
	hardMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// setupHardEgress is the production hard-egress lifecycle: create a per-run
// `--internal` network (no default route), start the allowlist proxy as a DUAL-HOMED
// gateway container (internal net + a normal net), wait for readiness, and return the
// internal network name, the proxy URL the sandbox should use (the gateway's internal
// IP:port — an IP, so the sandbox needs NO DNS to reach the proxy), and an idempotent
// teardown. Linux-container only; CI-validated (the injectable seam fakes it in unit
// tests). image MUST contain the nilcore binary (residual #2).
func setupHardEgress(runtime string, egress policy.Egress, image string) (network, gatewayProxyURL string, teardown func(), err error) {
	suffix, err := hardRandSuffix()
	if err != nil {
		return "", "", nil, fmt.Errorf("hard egress: rand: %w", err)
	}
	network = "nilcore-egr-net-" + suffix
	gwName := "nilcore-egr-gw-" + suffix
	// Idempotent teardown built up-front so a partial setup (net created, gateway
	// failed) still cleans up on the error paths below.
	teardown = hardTeardownFunc(runtime, network, gwName)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 1. The internal network: NO default route, so a container attached only to it
	//    can reach ONLY the internal subnet (never the host / internet directly).
	if out, e := runtimeCmd(ctx, runtime, "network", "create", "--internal", "--label", hardEgressLabel, network); e != nil {
		teardown()
		return "", "", nil, fmt.Errorf("hard egress: create internal net: %w: %s", e, strings.TrimSpace(out))
	}

	// 2. The gateway: dual-homed (internal net + a normal net so it can reach the
	//    allowed hosts), running the allowlist proxy on :3128.
	runArgs := []string{
		"run", "-d", "--label", hardEgressLabel,
		"--network", network, "--network", defaultNetworkName(runtime),
		"--name", gwName, image,
		"nilcore", "egress-gateway", "-allow", strings.Join(egress.Allowed, ","), "-listen", "0.0.0.0:" + hardGatewayPort,
	}
	if out, e := runtimeCmd(ctx, runtime, runArgs...); e != nil {
		teardown()
		return "", "", nil, fmt.Errorf("hard egress: start gateway: %w: %s", e, strings.TrimSpace(out))
	}

	// 3. Resolve the gateway's IP ON THE INTERNAL NET — the sandbox uses it for both
	//    HTTP(S)_PROXY (no DNS needed to reach the proxy) and --dns (blackholing
	//    in-sandbox resolution, since the gateway serves no :53).
	ip, e := gatewayInternalIP(ctx, runtime, gwName, network)
	if e != nil {
		teardown()
		return "", "", nil, fmt.Errorf("hard egress: gateway ip: %w", e)
	}

	// 4. Readiness: wait until the gateway logs that its proxy is listening (bounded).
	if e := waitGatewayReady(ctx, runtime, gwName, image); e != nil {
		teardown()
		return "", "", nil, fmt.Errorf("hard egress: gateway not ready: %w", e)
	}

	return network, policy.ProxyURL(ip + ":" + hardGatewayPort), teardown, nil
}

// hardTeardownFunc returns an idempotent teardown that force-removes the gateway
// container then the internal network. Best-effort: a missing container/network is
// not an error worth surfacing (the goal is reclamation, not verification).
func hardTeardownFunc(runtime, network, gwName string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = runtimeCmd(ctx, runtime, "rm", "-f", gwName)
			_, _ = runtimeCmd(ctx, runtime, "network", "rm", network)
		})
	}
}

// gatewayInternalIP inspects the gateway's IP on the internal network.
func gatewayInternalIP(ctx context.Context, runtime, gwName, network string) (string, error) {
	tmpl := fmt.Sprintf(`{{ (index .NetworkSettings.Networks %q).IPAddress }}`, network)
	out, err := runtimeCmd(ctx, runtime, "inspect", "-f", tmpl, gwName)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("gateway has no IP on %s", network)
	}
	return ip, nil
}

// waitGatewayReady polls the gateway's logs until the proxy reports it is listening,
// bailing fast if the container has already exited (a bad image / missing nilcore
// binary — residual #2). Bounded so a wedged setup fails closed rather than hangs.
func waitGatewayReady(ctx context.Context, runtime, gwName, image string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if out, _ := runtimeCmd(ctx, runtime, "logs", gwName); strings.Contains(out, "allowlist proxy on") {
			return nil
		}
		if st, _ := runtimeCmd(ctx, runtime, "inspect", "-f", "{{.State.Running}}", gwName); strings.TrimSpace(st) == "false" {
			return fmt.Errorf("gateway container exited before becoming ready (is the nilcore binary present in image %q?)", image)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for gateway readiness")
}

// defaultNetworkName is the runtime's normal (routable) network the gateway is also
// attached to so it can reach the allowed hosts.
func defaultNetworkName(runtime string) string {
	if runtime == "docker" {
		return "bridge"
	}
	return "podman" // podman's default rootless network
}

// hardRandSuffix returns a short random hex suffix so concurrent runs never collide on
// a network/container name.
func hardRandSuffix() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// runtimeCmd runs `<runtime> <args...>` and returns its combined output. Every hard-
// egress runtime interaction goes through here so the lifecycle is one small,
// auditable shell-out surface (I6 — no new module; we reuse the container runtime).
func runtimeCmd(ctx context.Context, runtime string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, runtime, args...).CombinedOutput()
	return string(out), err
}

// reapHardEgress reclaims orphaned hard-egress gateways + internal networks left by a
// crashed prior process, matched by the nilcore-egr label. It is best-effort and
// NON-BLOCKING (runs in a goroutine): pure housekeeping, never a correctness gate, and
// must not delay serve boot. Idempotent — nothing to reap is a no-op. Callers gate it
// on the hard-mode opt-in so a default (opt-out) boot spawns no runtime processes.
func reapHardEgress(runtime string, log *eventlog.Log) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Gateways run detached, so force-remove any label-tagged containers (running or
		// stopped) FIRST — a running container pins its network from `network prune`.
		if ids, err := runtimeCmd(ctx, runtime, "ps", "-aq", "--filter", "label="+hardEgressLabel); err == nil {
			for _, id := range strings.Fields(ids) {
				_, _ = runtimeCmd(ctx, runtime, "rm", "-f", id)
			}
		}
		// Then prune the now-unused internal networks by label.
		if out, err := runtimeCmd(ctx, runtime, "network", "prune", "-f", "--filter", "label="+hardEgressLabel); err != nil && log != nil {
			log.Append(eventlog.Event{Kind: "maint_error", Detail: map[string]any{"op": "hard_egress_reap", "error": err.Error(), "out": strings.TrimSpace(out)}})
		}
	}()
}
