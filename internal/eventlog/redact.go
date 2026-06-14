package eventlog

import (
	"regexp"
	"strings"
)

// secretRe matches common provider/CI key and token shapes embedded in any
// string (e.g. an API key the model echoed into a command). Each alternative is
// anchored to a recognizable prefix so non-secret text is not masked.
var secretRe = regexp.MustCompile(strings.Join([]string{
	`(?:sk|xoxb|xoxp|xoxa|xoxr|xoxs|xapp|ghp|gho|ghu|ghs|glpat)[-_][A-Za-z0-9_\-]{12,}`, // prefixed provider/CI tokens
	`AKIA[0-9A-Z]{16}`,                  // AWS access key id (bare, no separator)
	`ASIA[0-9A-Z]{16}`,                  // AWS temporary access key id
	`github_pat_[A-Za-z0-9_]{20,}`,      // GitHub fine-grained PAT
	`AIza[0-9A-Za-z\-_]{35}`,            // Google API key
	`-----BEGIN[ A-Z]*PRIVATE KEY-----`, // PEM private-key header
}, "|"))

// redact removes secret-looking values from a Detail map in place, before the
// event is written. Values under secret-named keys are dropped entirely; secret
// patterns embedded in any string value are masked. It recurses into nested maps
// and slices so a secret buried in structured detail is caught too (audit L5).
func redact(detail map[string]any) {
	redactMap(detail)
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if isSecretKey(k) {
			m[k] = "[redacted]"
			continue
		}
		m[k] = redactValue(v)
	}
}

func redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return secretRe.ReplaceAllString(t, "[redacted]")
	case map[string]any:
		redactMap(t)
		return t
	case []any:
		for i, e := range t {
			t[i] = redactValue(e)
		}
		return t
	default:
		return v
	}
}

func isSecretKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range []string{"key", "token", "secret", "password", "passwd", "authorization"} {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}
