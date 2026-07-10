package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestEgressGatewayServesAllowlist proves the hidden egress-gateway verb's testable
// core: it binds the allowlist proxy on the given address, refuses a denied host
// with a 403 before any dial, lets an allowlisted host PAST the allowlist (the -allow
// entry took effect — a denied host would 403, this one reaches the SSRF guard), and
// shuts down cleanly on ctx cancel. Hermetic: no host is ever dialed off-box (the
// allowed host is a loopback literal the SSRF guard blocks fast).
func TestEgressGatewayServesAllowlist(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Allow a loopback literal: the allowlist passes it, but the proxy's SSRF guard
	// then refuses loopback — a fast, network-free failure that is DISTINCT from the
	// "egress denied" 403, so it proves the -allow entry was applied.
	addr, stop, err := startEgressGateway(ctx, []string{"127.0.0.1"}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start gateway: %v", err)
	}

	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   5 * time.Second,
	}

	// Denied host ⇒ 403 with an "egress denied" body (rejected before any dial/DNS).
	resp, err := client.Get("http://denied.example/")
	if err != nil {
		t.Fatalf("denied request errored at transport: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied host status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "egress denied") {
		t.Errorf("denied host body = %q, want an 'egress denied' message", string(body))
	}

	// Allowed host ⇒ PAST the allowlist. It is not the 403 "egress denied" refusal;
	// the SSRF guard then blocks loopback with a 502, proving the allowlist let it in.
	resp2, err := client.Get("http://127.0.0.1:9/")
	if err != nil {
		t.Fatalf("allowed request errored at transport: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if strings.Contains(string(body2), "egress denied") {
		t.Errorf("allowlisted host was refused by the allowlist: %q", string(body2))
	}
	if resp2.StatusCode == http.StatusForbidden && !strings.Contains(string(body2), "private/local") {
		t.Errorf("allowlisted host got an unexpected 403: %q", string(body2))
	}

	// Shutdown: cancel ctx (the container-stop analog) ⇒ the listener closes and a
	// fresh proxied request can no longer be served.
	cancel()
	stop()
	deadline := time.Now().Add(2 * time.Second)
	down := false
	for time.Now().Before(deadline) {
		c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 200 * time.Millisecond}
		if _, err := c.Get("http://denied.example/"); err != nil {
			down = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !down {
		t.Error("gateway still serving after ctx cancel + stop; want the listener closed")
	}
}
