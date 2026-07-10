// egress_gateway.go wires the hidden `egress-gateway` verb: the process the HARD
// egress GATEWAY container runs (docs/ARCHITECTURE.md §Execution-model/egress). It is
// deliberately NOT advertised in `nilcore help` — it is machinery the hard-egress
// lifecycle (egress_hard.go) launches inside a dual-homed container, not an operator
// command. It binds a policy.EgressProxy (the SAME allowlist + SSRF guard the
// cooperative proxy uses) on -listen and serves it until the process is signalled.
//
// In hard mode the sandbox is attached to a `--internal` network with no route out;
// its only path off-box is this gateway (dual-homed: internal net + a normal net),
// so the allowlist becomes UNBYPASSABLE rather than merely cooperative. See
// sandbox.Container.AllowEgressViaHard and setupHardEgress.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"nilcore/internal/policy"
)

// egressGatewayMain parses -allow/-listen and serves the allowlist proxy until the
// process receives SIGINT/SIGTERM (the container stop signal). It is the entrypoint
// the gateway container invokes as `nilcore egress-gateway -allow <hosts> -listen …`.
func egressGatewayMain(args []string) {
	fs := flag.NewFlagSet("egress-gateway", flag.ExitOnError)
	allow := fs.String("allow", "", "comma-separated allowlist of hosts the sandbox may reach through this gateway")
	listen := fs.String("listen", "0.0.0.0:3128", "address:port to bind the allowlist proxy on")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runEgressGateway(ctx, splitHosts(*allow), *listen); err != nil {
		fatal(err)
	}
}

// startEgressGateway binds the allowlist proxy on listen and serves it in the
// background, returning the bound address and an idempotent stop func. Extracted as
// the testable seam of the verb: a test can bind 127.0.0.1:0, exercise the running
// proxy (deny→403, allow→past-the-allowlist), then stop it — without a process.
func startEgressGateway(ctx context.Context, allow []string, listen string) (addr string, stop func(), err error) {
	proxy := &policy.EgressProxy{Egress: policy.Egress{Allowed: allow}}
	return proxy.Start(ctx, listen)
}

// runEgressGateway starts the gateway and blocks until ctx is cancelled (the verb's
// long-running body). The allowlist + SSRF guard in policy.EgressProxy gate every
// request regardless of bind interface.
func runEgressGateway(ctx context.Context, allow []string, listen string) error {
	addr, stop, err := startEgressGateway(ctx, allow, listen)
	if err != nil {
		return fmt.Errorf("egress-gateway: %w", err)
	}
	defer stop()
	fmt.Fprintf(os.Stderr, "nilcore egress-gateway: allowlist proxy on %s (%d allowed host(s))\n", addr, len(allow))
	<-ctx.Done()
	return nil
}
