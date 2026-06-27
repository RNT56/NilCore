package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/scmhook"
	"nilcore/internal/trigger"
)

// startWebhookListener mounts the SCM/CI webhook intake (P9-T04) alongside the
// serve channel loop. A signed GitHub webhook (HMAC-verified with
// NILCORE_WEBHOOK_SECRET — I3) becomes a trigger.Signal routed through the
// existing reversible-auto-start / irreversible-gate machinery.
//
// It is HEADLESS: there is no human at the listener, so irreversible self-starts
// deny-default (I3) — only reversible work auto-starts. Self-started tasks run
// serially (one orchestrator, mutex-guarded) through the SAME verified path as
// `nilcore -goal`. The listener is bound to ctx and shut down on serve exit. A
// missing secret disables the intake (fail-closed) rather than accepting unsigned
// requests.
func startWebhookListener(ctx context.Context, addr string, c commonFlags, b boot, log *eventlog.Log, dir, secret string) {
	if secret == "" {
		fmt.Fprintln(os.Stderr, "nilcore serve: --webhook set but NILCORE_WEBHOOK_SECRET is empty; webhook intake disabled (fail-closed)")
		return
	}
	// Share the run's blast-radius budget across its sandbox/egress (BR-T02/T03); the
	// gate stays a hardcoded headless deny (I3). nil when off ⇒ unfenced, byte-identical.
	orch := buildRunOrchestrator(c, b, log, dir, mintBlastBudget(*c.blastRadius, log))
	var mu sync.Mutex // serialize self-started runs on the single orchestrator
	trig := &trigger.Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false }, // headless: irreversible deny-defaults (I3)
		Start: func(ctx context.Context, goal string) error {
			mu.Lock()
			defer mu.Unlock()
			out, err := runViaKernel(ctx, orch, backend.Task{ID: fmt.Sprintf("hook-%d", time.Now().UnixNano()), Goal: goal})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "webhook self-started: verified=%v — %s\n", out.Verified, out.Summary)
			return nil
		},
		Log: log,
	}
	h := &scmhook.Handler{
		Secret:       secret,
		TriggerLabel: os.Getenv("NILCORE_WEBHOOK_LABEL"),
		Handle:       trig.Handle,
		Log:          log,
	}
	mux := http.NewServeMux()
	mux.Handle("/webhook", h)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "nilcore serve: webhook listener: %v\n", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "nilcore serve: webhook intake on http://%s/webhook (label=%q)\n", addr, os.Getenv("NILCORE_WEBHOOK_LABEL"))
}
