package policy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestEgressAllow(t *testing.T) {
	e := Egress{Allowed: []string{"api.anthropic.com", "*.pypi.org"}}
	for _, h := range []string{"api.anthropic.com", "pypi.org", "files.pypi.org", "a.b.pypi.org", "API.Anthropic.com"} {
		if !e.Allow(h) {
			t.Errorf("Allow(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"evil.com", "anthropic.com", "evilpypi.org", "notpypi.org.evil.com", "", "api.openai.com"} {
		if e.Allow(h) {
			t.Errorf("Allow(%q) = true, want false", h)
		}
	}
}

func TestEgressEmptyDeniesAll(t *testing.T) {
	var e Egress
	if !e.Empty() {
		t.Error("zero Egress should be Empty")
	}
	if e.Allow("github.com") {
		t.Error("empty allowlist must deny everything")
	}
}

func TestDefaultEgress(t *testing.T) {
	e := DefaultEgress()
	if !e.Allow("api.anthropic.com") || !e.Allow("proxy.golang.org") {
		t.Error("default should allow model API + Go proxy")
	}
	if e.Allow("evil.example.com") {
		t.Error("default should deny unknown hosts")
	}
}

func TestEgressProxyDenies(t *testing.T) {
	p := &EgressProxy{Egress: Egress{Allowed: []string{"allowed.com"}}}

	req := httptest.NewRequest(http.MethodConnect, "http://denied.com:443", nil)
	req.Host = "denied.com:443"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("CONNECT denied host = %d, want 403", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "http://denied.com/x", nil)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("GET denied host = %d, want 403", rec2.Code)
	}
}

func TestEgressProxyForwardsAllowed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	// Allow the backend's host (127.0.0.1). The backend is on loopback, which the
	// SSRF guard blocks by default, so relax the IP guard for this happy-path test.
	p := &EgressProxy{Egress: Egress{Allowed: []string{"127.0.0.1"}}, blockIP: func(net.IP) bool { return false }}
	req := httptest.NewRequest(http.MethodGet, backend.URL+"/x", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("forward allowed = %d %q", rec.Code, rec.Body.String())
	}
}

// TestEgressProxyRefusesPrivateDestination proves the SSRF guard: even an
// allowlisted host that resolves to loopback/private space is refused at dial
// time (502), so the proxy can never be used to reach localhost, the cloud
// metadata endpoint, or the internal network (audit L1).
func TestEgressProxyRefusesPrivateDestination(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "SHOULD NOT REACH")
	}))
	defer backend.Close()

	// Default guard (no blockIP override): allowlist permits the host, but the
	// destination is loopback, so the dial must be refused.
	p := &EgressProxy{Egress: Egress{Allowed: []string{"127.0.0.1"}}}
	req := httptest.NewRequest(http.MethodGet, backend.URL+"/x", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("proxy reached a loopback destination: SSRF guard ineffective (body=%q)", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("loopback forward = %d, want 502", rec.Code)
	}
}

// TestEgressProxyStartServesAndShutsDown proves the LIFECYCLE: Start binds a real
// listener and serves; a client pointed at the proxy reaches an allowlisted host
// and is refused an off-list one with 403; and stop() shuts the listener down so a
// follow-up dial fails. This is the half ServeHTTP alone did not provide.
func TestEgressProxyStartServesAndShutsDown(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()

	// Allow the backend host; relax the SSRF guard since the backend is on loopback.
	p := &EgressProxy{Egress: Egress{Allowed: []string{"127.0.0.1"}}, blockIP: func(net.IP) bool { return false }}
	addr, stop, err := p.Start(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	proxyURL, _ := url.Parse(ProxyURL(addr))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Allowlisted host: reaches the backend through the listening proxy.
	resp, err := client.Get(backend.URL + "/x")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("allowlisted GET = %d %q, want 200 ok", resp.StatusCode, body)
	}

	// Off-list host: 403 before any connection.
	resp2, err := client.Get("http://denied.example.com/x")
	if err != nil {
		t.Fatalf("GET denied through proxy: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("off-list GET = %d, want 403", resp2.StatusCode)
	}

	// Shutdown: a fresh dial to the (now closed) listener must fail.
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, derr := net.DialTimeout("tcp", addr, 50*time.Millisecond); derr != nil {
			break // listener is down
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy listener still accepting after stop()")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrivateOrLocal(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1",
		"169.254.169.254", "0.0.0.0", "::1", "fe80::1", "fc00::1", "224.0.0.1"}
	for _, s := range blocked {
		if ip := net.ParseIP(s); ip == nil || !privateOrLocal(ip) {
			t.Errorf("privateOrLocal(%s) = false, want true", s)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range public {
		if ip := net.ParseIP(s); ip == nil || privateOrLocal(ip) {
			t.Errorf("privateOrLocal(%s) = true, want false", s)
		}
	}
}

func TestResolveRejectsPrivateLiteral(t *testing.T) {
	p := &EgressProxy{}
	if _, err := p.resolve(context.Background(), "169.254.169.254"); err == nil {
		t.Error("resolve must reject the cloud-metadata address")
	}
	if ip, err := p.resolve(context.Background(), "8.8.8.8"); err != nil || ip == nil {
		t.Errorf("resolve(8.8.8.8) = %v, %v; want a public IP", ip, err)
	}
}
