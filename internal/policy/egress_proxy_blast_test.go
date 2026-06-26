package policy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nilcore/internal/blastbudget"
)

// TestEgressProxyBlastBreachRefusesBeforeDial proves the host-fan-out fence: once
// the distinct-host budget is full, a NEW allowlisted host is refused with a 403
// at the blast check, before any dial/tunnel/forward.
func TestEgressProxyBlastBreachRefusesBeforeDial(t *testing.T) {
	b := blastbudget.New()
	b.SetHostCeiling(1)
	// Fill the single host slot with some other host.
	if err := b.ChargeHost(context.Background(), "first.example"); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	p := &EgressProxy{
		Egress:  Egress{Allowed: []string{"second.example"}},
		blockIP: func(net.IP) bool { return false },
		Blast:   b,
	}
	// A new, allowlisted host that would be the 2nd distinct ⇒ breach, no dial.
	req := httptest.NewRequest(http.MethodGet, "http://second.example/x", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("over-budget host = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "blast-radius") {
		t.Errorf("403 body = %q, want a blast-radius message", rec.Body.String())
	}
}

// TestEgressProxyBlastWithinBudgetIdempotent proves a host within budget forwards
// normally, and repeated requests to the SAME host never re-consume budget.
func TestEgressProxyBlastWithinBudgetIdempotent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	host, _, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	b := blastbudget.New()
	b.SetHostCeiling(2)
	p := &EgressProxy{
		Egress:  Egress{Allowed: []string{host}},
		blockIP: func(net.IP) bool { return false },
		Blast:   b,
	}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, backend.URL+"/x", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d to an in-budget host = %d, want 200", i, rec.Code)
		}
	}
	if u := b.Used(""); u.Hosts != 1 {
		t.Errorf("distinct hosts charged = %d, want 1 (idempotent over repeats)", u.Hosts)
	}
}

// TestEgressProxyNilBlastByteIdentical proves the nil-Blast path is unchanged: an
// allowlisted host forwards exactly as before, with no fence in the way.
func TestEgressProxyNilBlastByteIdentical(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	p := &EgressProxy{Egress: Egress{Allowed: []string{host}}, blockIP: func(net.IP) bool { return false }} // Blast nil
	req := httptest.NewRequest(http.MethodGet, backend.URL+"/x", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-Blast forward = %d, want 200", rec.Code)
	}
}
