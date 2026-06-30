// Package scmhook is an inbound SCM/CI webhook listener that turns a signed
// GitHub event (a labeled issue, or a failing CI run) into a trigger.Signal and
// routes it through the existing reversible-auto-start / irreversible-gate
// machinery (internal/trigger). It adds a new Signal SOURCE, not a new mechanism.
//
// Security:
//   - Every request is authenticated by an HMAC-SHA256 signature over the raw body
//     (GitHub's X-Hub-Signature-256), compared in constant time against a secret
//     held by the operator's SecretStore (invariant I3). An unsigned or
//     mismatched request is rejected (401) and never produces a Signal.
//   - The payload is UNTRUSTED data (invariant I7): the issue title/CI name is
//     embedded inside a harness-authored goal frame ("Address GitHub issue #N: …")
//     rather than used verbatim as the controlling instruction. The downstream loop
//     fences any payload-derived context with guard.Wrap.
//
// Stdlib only (invariant I6): crypto/hmac + crypto/sha256 + net/http.
package scmhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"nilcore/internal/eventlog"
	"nilcore/internal/trigger"
)

const maxBodyBytes = 2 << 20 // 2 MiB cap on a webhook body

// maxSeenDeliveries bounds the replay-defense cache so a long-lived listener cannot
// grow it without limit; once full the oldest-seen id is evicted (a simple FIFO ring).
// A forge re-delivers the same id on retry within seconds, so a small window suffices.
const maxSeenDeliveries = 4096

// Handler verifies and routes inbound SCM webhooks.
type Handler struct {
	// Secret is the HMAC secret shared with the forge's webhook config. It comes
	// from the SecretStore and is never logged.
	Secret string
	// TriggerLabel, when set, restricts issue events to issues carrying this label
	// (e.g. "nilcore"). Empty means any labeled issue qualifies.
	TriggerLabel string
	// Handle routes an accepted Signal (typically trigger.Trigger.Handle). Required.
	Handle func(ctx context.Context, sig trigger.Signal) (bool, error)
	// Log records metadata-only audit events (invariant I5). Optional.
	Log *eventlog.Log

	// seen is the bounded set of already-processed X-GitHub-Delivery ids, the replay
	// defense: a correctly-signed body re-POSTed later (a captured-and-replayed
	// delivery) re-passes the HMAC check but is dropped here so it does not re-emit
	// the same Signal / re-launch the same auto-run. order is the FIFO eviction ring.
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// seenBefore records the delivery id and reports whether it was already processed.
// An empty id (a forge that omits X-GitHub-Delivery) is never treated as seen, so the
// pre-existing behaviour is preserved for callers without a delivery header.
func (h *Handler) seenBefore(id string) bool {
	if id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = make(map[string]struct{}, maxSeenDeliveries)
	}
	if _, ok := h.seen[id]; ok {
		return true
	}
	if len(h.order) >= maxSeenDeliveries {
		oldest := h.order[0]
		h.order = h.order[1:]
		delete(h.seen, oldest)
	}
	h.seen[id] = struct{}{}
	h.order = append(h.order, id)
	return false
}

func (h *Handler) log(kind string, detail map[string]any) {
	if h.Log != nil {
		h.Log.Append(eventlog.Event{Kind: kind, Detail: detail})
	}
}

// ServeHTTP authenticates the request, maps a supported event to a trigger.Signal,
// and routes it. It returns 401 on a bad signature, 202 when a Signal was routed,
// and 204 for a well-formed but non-actionable event.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	if !validSignature(h.Secret, r.Header.Get("X-Hub-Signature-256"), body) {
		h.log("webhook_rejected", map[string]any{"reason": "bad-signature", "event": r.Header.Get("X-GitHub-Event")})
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	// Replay defense (I7-adjacent): a captured (body, signature) pair re-POSTed later
	// re-passes the HMAC check, so drop a delivery id we have already processed as a
	// no-op 200 BEFORE mapping/routing it — otherwise a replayed "issue labeled" could
	// re-launch the same auto-run. Dropped before mapEvent so it never produces a Signal.
	if delivery := r.Header.Get("X-GitHub-Delivery"); h.seenBefore(delivery) {
		h.log("webhook_replay_dropped", map[string]any{"event": r.Header.Get("X-GitHub-Event")})
		w.WriteHeader(http.StatusOK)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	sig, ok := h.mapEvent(event, body)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h.log("webhook_accepted", map[string]any{"event": event, "source": sig.Source})
	if h.Handle != nil {
		if _, err := h.Handle(r.Context(), sig); err != nil {
			// The Signal was authenticated and routed; a downstream error is logged
			// but the webhook delivery is still acknowledged so the forge does not
			// hammer redeliveries.
			h.log("webhook_handle_error", map[string]any{"event": event, "err": err.Error()})
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// validSignature constant-time compares the GitHub HMAC-SHA256 signature header
// ("sha256=<hex>") against HMAC(secret, body). An empty secret or header fails.
func validSignature(secret, header string, body []byte) bool {
	if secret == "" {
		return false
	}
	want, ok := strings.CutPrefix(header, "sha256=")
	if !ok || want == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sum := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sum), []byte(want))
}

// githubPayload is the small slice of the webhook body the mapper reads. Unknown
// fields are ignored; the body is parsed as data, never executed (invariant I7).
type githubPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue"`
	WorkflowRun struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
	} `json:"workflow_run"`
}

// mapEvent translates a supported, actionable event into a trigger.Signal. It
// returns ok=false for events that should be ignored (no-op 204).
func (h *Handler) mapEvent(event string, body []byte) (trigger.Signal, bool) {
	var p githubPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return trigger.Signal{}, false
	}
	switch event {
	case "issues":
		if p.Action != "labeled" && p.Action != "opened" {
			return trigger.Signal{}, false
		}
		if h.TriggerLabel != "" && !hasLabel(p.Issue.Labels, h.TriggerLabel) {
			return trigger.Signal{}, false
		}
		// The attacker-controllable title is embedded as data inside a fixed,
		// harness-authored instruction frame (I7); the agent is told to read the
		// issue, not to obey its text.
		goal := fmt.Sprintf("Address GitHub issue #%d (%q): read the issue, reproduce, and implement a verified fix.", p.Issue.Number, oneLine(p.Issue.Title))
		return trigger.Signal{Source: "issue", Goal: goal}, true
	case "workflow_run":
		if p.Action != "completed" || p.WorkflowRun.Conclusion != "failure" {
			return trigger.Signal{}, false
		}
		goal := fmt.Sprintf("CI workflow %q failed on branch %q: diagnose the failure and implement a verified fix.", oneLine(p.WorkflowRun.Name), oneLine(p.WorkflowRun.HeadBranch))
		return trigger.Signal{Source: "ci", Goal: goal}, true
	default:
		return trigger.Signal{}, false
	}
}

func hasLabel(labels []struct {
	Name string `json:"name"`
}, want string) bool {
	for _, l := range labels {
		if l.Name == want {
			return true
		}
	}
	return false
}

// oneLine collapses newlines so a hostile title cannot inject extra framing lines
// into the goal string.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
