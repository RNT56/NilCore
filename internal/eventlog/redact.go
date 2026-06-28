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

// inlineSecretRe catches a credential assigned to a named field INSIDE a free-form
// string — most importantly a model-authored shell command stored under "cmd"
// (e.g. `export DB_PASSWORD=hunter2`, `--token=ghs_...`, `Authorization: Bearer x`).
// A pattern-only allowlist (secretRe) cannot know every provider shape; this is the
// belt-and-suspenders for the highest-risk free-text path (I3: no secrets in the log,
// ever). It keeps the key name and masks the value. Conservative by design — over-
// redaction in the audit trail is preferred to a leaked credential.
// The optional scheme group ((?:bearer|basic|token) ) keeps an auth scheme word
// visible while masking only the credential after it, so `Authorization: Bearer XYZ`
// becomes `Authorization: Bearer [redacted]` (not `Authorization: [redacted] XYZ`).
var inlineSecretRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|client[_-]?secret|authorization|auth[_-]?token|bearer)([ \t]*[=:][ \t]*|[ \t]+)((?:bearer|basic|token)[ \t]+)?("?)([^\s"']{4,})`)

// flagSecretRe catches a credential passed as a CLI flag value (e.g. `mysql -p s3cr3t`,
// `curl --header 'Authorization: x'` is covered above; this covers `-p`, `--password`,
// `--token`, `--secret`, `--api-key`, `--access-key`).
var flagSecretRe = regexp.MustCompile(`(?i)((?:^|\s)(?:-p|--password|--passwd|--token|--secret|--api-key|--access-key|--auth-token)(?:[= ]))(\S{4,})`)

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
		s := secretRe.ReplaceAllString(t, "[redacted]")
		s = inlineSecretRe.ReplaceAllString(s, "${1}${2}${3}${4}[redacted]")
		s = flagSecretRe.ReplaceAllString(s, "${1}[redacted]")
		return s
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
