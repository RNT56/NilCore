package policy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// EgressProxy is a forward HTTP/HTTPS proxy that permits only hosts the Egress
// allowlist allows — the documented mechanism for sandbox egress (P2-T02). The
// sandbox runs with no direct route to the internet and HTTP(S)_PROXY pointed
// here, so a host the policy denies cannot be reached even if the model tries.
// Untrusted destinations are refused with 403 before any connection is made.
type EgressProxy struct {
	Egress Egress
}

// ServeHTTP enforces the allowlist, then tunnels (CONNECT) or forwards (plain).
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r)
	if !p.Egress.Allow(host) {
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodConnect {
		p.tunnel(w, r)
		return
	}
	p.forward(w, r)
}

// hostOnly extracts the destination hostname (no port) from a proxy request.
func hostOnly(r *http.Request) string {
	h := r.Host
	if r.Method != http.MethodConnect && r.URL.Host != "" {
		h = r.URL.Host
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

func (p *EgressProxy) tunnel(w http.ResponseWriter, r *http.Request) {
	dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		dst.Close()
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	_, _ = io.WriteString(src, "HTTP/1.1 200 Connection Established\r\n\r\n")
	go pipe(dst, src)
	go pipe(src, dst)
}

func (p *EgressProxy) forward(w http.ResponseWriter, r *http.Request) {
	out, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out.Header = r.Header.Clone()
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func pipe(dst io.WriteCloser, src io.ReadCloser) {
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

// ProxyURL is the env value the sandbox should use for HTTP(S)_PROXY when running
// the proxy at addr (host:port).
func ProxyURL(addr string) string { return fmt.Sprintf("http://%s", addr) }
