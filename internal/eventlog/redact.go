package eventlog

import (
	"regexp"
	"strings"
)

// secretRe matches common provider/CI key and token shapes embedded in any
// string (e.g. an API key the model echoed into a command).
var secretRe = regexp.MustCompile(`(sk|xoxb|xoxp|xapp|ghp|gho|glpat|AKIA)[-_][A-Za-z0-9_\-]{12,}`)

// redact removes secret-looking values from a Detail map in place, before the
// event is written. Values under secret-named keys are dropped entirely; secret
// patterns embedded in any string value are masked.
func redact(detail map[string]any) {
	for k, v := range detail {
		if isSecretKey(k) {
			detail[k] = "[redacted]"
			continue
		}
		if s, ok := v.(string); ok {
			detail[k] = secretRe.ReplaceAllString(s, "[redacted]")
		}
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
