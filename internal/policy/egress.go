package policy

import "strings"

// Egress is a default-deny network allowlist for the sandbox. With no entries,
// all egress is denied (the Phase-0/1 behavior). Entries are hostnames or
// "*.suffix" wildcards, e.g. "api.anthropic.com" or "*.pypi.org".
type Egress struct {
	Allowed []string
}

// Allow reports whether host (no port) may be reached.
func (e Egress) Allow(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, pat := range e.Allowed {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == host {
			return true
		}
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:] // ".pypi.org"
			if host == pat[2:] || strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

// Empty reports whether the allowlist denies everything (no entries).
func (e Egress) Empty() bool { return len(e.Allowed) == 0 }

// DefaultEgress is a conservative allowlist: model APIs and common package
// registries, nothing else. Everything not listed is denied.
func DefaultEgress() Egress {
	return Egress{Allowed: []string{
		"api.anthropic.com",
		"api.openai.com",
		"openrouter.ai",
		"proxy.golang.org",
		"sum.golang.org",
		"registry.npmjs.org",
		"*.pypi.org",
		"files.pythonhosted.org",
		"github.com",
		"*.githubusercontent.com",
		"crates.io",
		"static.crates.io",
	}}
}

// EgressWith returns the DefaultEgress allowlist extended with operator-supplied
// hosts, appended in order after the defaults and de-duplicated (case- and
// space-insensitive on the host text). With no extra hosts it is identical to
// DefaultEgress — the conservative default is never mutated. It is the public
// primitive for composing the sandbox egress allowlist (retained for that purpose;
// the named egress-profile path builds its allowlist separately today).
//
// IMPORTANT: this allowlist governs the SANDBOX container network only — the
// hosts that model-emitted (sandboxed) code is permitted to reach. It does NOT
// gate the host-side model.Provider call. Pointing the chat adapter at a custom
// or localhost model endpoint therefore needs no egress edit; add a host here
// only when sandboxed code itself must reach that endpoint.
func EgressWith(extra ...string) Egress {
	base := DefaultEgress()
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base.Allowed)+len(extra))
	allowed := make([]string, 0, len(base.Allowed)+len(extra))
	add := func(host string) {
		key := strings.ToLower(strings.TrimSpace(host))
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		allowed = append(allowed, host)
	}
	for _, h := range base.Allowed {
		add(h)
	}
	for _, h := range extra {
		add(h)
	}
	return Egress{Allowed: allowed}
}
