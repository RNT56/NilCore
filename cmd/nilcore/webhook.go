package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/blastbudget"
	"nilcore/internal/eventlog"
	"nilcore/internal/memory"
	"nilcore/internal/scmhook"
	"nilcore/internal/trigger"
)

// Self-start rate-fence defaults (denial-of-wallet). The listener is headless and
// fed by untrusted, replayable-signed deliveries, so the count of self-starts is
// bounded even though the run mutex already serializes them. Both are overridable
// (NILCORE_WEBHOOK_MAX_PER_DAY / NILCORE_WEBHOOK_MIN_INTERVAL) and fail SAFE — a
// missing or bad value clamps to the bounded default, never to unbounded.
const (
	defaultWebhookMaxPerDay   = 20
	defaultWebhookMinInterval = time.Minute
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
//
// Denial-of-wallet fence: a valid HMAC only proves a forge relayed the delivery, not
// that a human authorized a run, so two bounds cap the COST/content a stream of
// signed-but-unauthorized deliveries can incur — a per-day self-start cap plus a
// cooldown (trigger.RateLimiter, on TOP of the serial mutex), and a fail-closed label
// default: with NILCORE_WEBHOOK_LABEL unset a bare "opened" issue does NOT self-start
// (see scmhook.mapEvent), so a drive-by opened issue on a public repo cannot trigger.
func startWebhookListener(ctx context.Context, addr string, c commonFlags, b boot, log *eventlog.Log, dir, secret string, mem *memory.Memory, ckpt *agent.Checkpoint, blast *blastbudget.Budget) {
	if secret == "" {
		fmt.Fprintln(os.Stderr, "nilcore serve: --webhook set but NILCORE_WEBHOOK_SECRET is empty; webhook intake disabled (fail-closed)")
		return
	}
	// Reuse the SERVE process's store, memory, checkpointer and blast-radius budget
	// (shared across the run's sandbox/egress, BR-T02/T03; nil when off ⇒ unfenced,
	// byte-identical). buildRunOrchestrator calls setupPersistence, which opens a second
	// MaxOpenConns(1) *sql.DB on the nilcore.db serve already owns — its own contract
	// forbids serve from calling it. That second handle re-pointed log.UseStore and the
	// experience/lessons hooks at a different connection, was never Closed, and exposed
	// SQLITE_BUSY under a concurrent serve-thread + webhook-run write. This listener runs
	// UNDER serve (which already opened one), so buildRunOrchestratorWith takes the
	// handles serve already holds. The gate stays a hardcoded headless deny (I3).
	orch := buildRunOrchestratorWith(c, b, log, dir, blast, mem, ckpt)
	var mu sync.Mutex // serialize self-started runs on the single orchestrator
	trig := &trigger.Trigger{
		Enabled: true,
		Gate:    func(string) bool { return false }, // headless: irreversible deny-defaults (I3)
		// Bound the NUMBER of self-starts (the mutex only serializes them): a per-day cap
		// + cooldown so untrusted signed deliveries cannot spin up unbounded runs (I5-audited).
		Limiter: &trigger.RateLimiter{MaxPerDay: webhookMaxPerDay(), MinInterval: webhookMinInterval()},
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

// webhookMaxPerDay reads the self-start daily cap from NILCORE_WEBHOOK_MAX_PER_DAY.
// Unset/blank ⇒ defaultWebhookMaxPerDay; a non-positive or unparseable value clamps
// to the default (fail-safe to a bound, never to unbounded — a broken value must not
// disable the denial-of-wallet fence).
func webhookMaxPerDay() int {
	raw := strings.TrimSpace(os.Getenv("NILCORE_WEBHOOK_MAX_PER_DAY"))
	if raw == "" {
		return defaultWebhookMaxPerDay
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultWebhookMaxPerDay
	}
	return n
}

// webhookMinInterval reads the cooldown between self-starts from
// NILCORE_WEBHOOK_MIN_INTERVAL (a Go duration, e.g. "60s"). Unset/blank ⇒
// defaultWebhookMinInterval; a negative or unparseable value clamps to the default.
func webhookMinInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("NILCORE_WEBHOOK_MIN_INTERVAL"))
	if raw == "" {
		return defaultWebhookMinInterval
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return defaultWebhookMinInterval
	}
	return d
}
