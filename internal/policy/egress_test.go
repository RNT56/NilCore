package policy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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

	// Allow the backend's host (127.0.0.1).
	p := &EgressProxy{Egress: Egress{Allowed: []string{"127.0.0.1"}}}
	req := httptest.NewRequest(http.MethodGet, backend.URL+"/x", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("forward allowed = %d %q", rec.Code, rec.Body.String())
	}
}
