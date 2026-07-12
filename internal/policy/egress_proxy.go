package policy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"nilcore/internal/blastbudget"
)

// EgressProxy is a forward HTTP/HTTPS proxy that permits only hosts the Egress
// allowlist allows — the documented mechanism for sandbox egress (P2-T02).
// Untrusted destinations are refused with 403 before any connection is made.
//
// IMPORTANT — the boundary this enforces depends on the sandbox backend:
//   - namespace backend (Linux, Auto-preferred): the child runs in an EMPTY network
//     namespace — there is no route to the internet at all, so egress is a HARD
//     deny-all (this proxy is not even reachable there).
//   - container backend: the container runs with `--network bridge` (a real NAT
//     route) and HTTP(S)_PROXY pointed here, so this proxy is COOPERATIVE — it binds
//     only proxy-respecting clients. A sandboxed command that ignores the proxy
//     (`curl --noproxy '*'`, a raw socket, bash `/dev/tcp`) can still reach arbitrary
//     hosts, including cloud metadata. Treat container-backend egress allowlisting as
//     defense-in-depth, NOT a hard boundary; use the namespace backend when a hard
//     egress boundary is required. See Container.AllowEgressVia + applyContainerEgress.
//
// Both code paths additionally resolve the destination and refuse any address in
// loopback/link-local/private/multicast space, then pin the connection to that
// validated IP — so an allowlisted name (or one swapped via DNS rebinding) can
// never be used to reach localhost, the cloud-metadata endpoint, or the internal
// network (SSRF defense, audit L1).
type EgressProxy struct {
	Egress Egress

	// Blast optionally fences the distinct-egress-host axis of the blast-radius
	// budget (Phase 16, BR-T02). When non-nil, ServeHTTP charges the destination
	// host AFTER the allowlist passes but BEFORE any dial/tunnel/forward, so an
	// unattended run can never reach more than the budgeted number of distinct
	// hosts. nil = today's behaviour exactly (the leaf is a no-op on a nil
	// receiver, and an already-charged host is idempotent).
	Blast *blastbudget.Budget

	// blockIP optionally overrides the destination-IP guard (default
	// privateOrLocal). Left nil in production; tests relax it to exercise the
	// forward/tunnel paths against a loopback backend. Set before first use.
	blockIP func(net.IP) bool

	once sync.Once
	tr   *http.Transport
}

// privateOrLocal reports whether ip must never be a proxy destination: loopback,
// link-local (incl. 169.254.169.254 cloud metadata), private/ULA, unspecified,
// and any multicast. These are exactly the ranges an SSRF tries to reach.
func privateOrLocal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
}

func (p *EgressProxy) blocked(ip net.IP) bool {
	if p.blockIP != nil {
		return p.blockIP(ip)
	}
	return privateOrLocal(ip)
}

// resolve resolves host and returns its first permitted IP, erroring if the host
// has no addresses or every address is blocked. The returned IP is used to pin
// the dial so DNS cannot rebind to an internal address after the check.
func (p *EgressProxy) resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if p.blocked(ip) {
			return nil, fmt.Errorf("destination %s is a private/local address", host)
		}
		return ip, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if !p.blocked(ip) {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("destination %s resolves only to private/local addresses", host)
}

// safeDial resolves+validates addr's host and dials the pinned permitted IP, so a
// destination can never be redirected into loopback/link-local/private space.
func (p *EgressProxy) safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip, err := p.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

// transport forwards plain requests using safeDial (no nested proxy), so the same
// private-range guard applies to non-CONNECT traffic. Built once and reused.
func (p *EgressProxy) transport() *http.Transport {
	p.once.Do(func() {
		p.tr = &http.Transport{
			DialContext:           p.safeDial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ForceAttemptHTTP2:     true,
		}
	})
	return p.tr
}

// ServeHTTP enforces the allowlist, then tunnels (CONNECT) or forwards (plain).
func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r)
	if !p.Egress.Allow(host) {
		http.Error(w, "egress denied: "+host, http.StatusForbidden)
		return
	}
	// Blast-radius fence: once the host is allowlisted, charge the distinct-host
	// axis BEFORE any connection. A breach refuses with the same 403 shape as the
	// allowlist, so no dial/tunnel/forward happens. nil Blast and an
	// already-charged host are both no-ops (leaf-handled), so this is byte-
	// identical to today when unwired and idempotent for a repeated host. We read
	// only the already-parsed host — never the path/query/body (I7).
	if err := p.Blast.ChargeHost(r.Context(), host); err != nil {
		http.Error(w, "blast-radius: egress host budget exhausted", http.StatusForbidden)
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
	dst, err := p.safeDial(r.Context(), "tcp", r.Host)
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
	resp, err := p.transport().RoundTrip(out)
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

// Start binds a listener and serves the proxy in a background goroutine until ctx
// is cancelled or the returned stop func is called. It is the LIFECYCLE around
// ServeHTTP (which is only an http.Handler): a listener, a bounded goroutine, and
// a clean shutdown. It returns the bound address (host:port — feed it to ProxyURL)
// and an idempotent stop func.
//
// bindAddr selects the interface: "127.0.0.1:0" (the default when empty) is right
// when the sandbox shares the host's network namespace; a bridged container that
// must reach the host across a bridge needs an address reachable from inside it
// (e.g. "0.0.0.0:0" plus the runtime's host alias for the proxy URL) — that
// trade-off is the caller's to make, since only it knows the sandbox backend. The
// allowlist + SSRF guard in ServeHTTP still gate every request regardless of bind.
func (p *EgressProxy) Start(ctx context.Context, bindAddr string) (addr string, stop func(), err error) {
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return "", nil, fmt.Errorf("egress proxy listen on %s: %w", bindAddr, err)
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }() // returns ErrServerClosed on shutdown

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }
	return ln.Addr().String(), stop, nil
}
