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
