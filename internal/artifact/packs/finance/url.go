package finance

import (
	"fmt"
	"net/url"
	"strings"
)

// queryParam extracts a single query parameter from a model-declared source_url. The
// model declares WHICH series/symbol it wants via the key-free public URL; the keyed
// checks then DERIVE the real request URL from a key-free base plus that parameter and
// the injected key — they never trust the model's full URL for the keyed reach.
func queryParam(raw, name string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid source_url: %w", err)
	}
	return u.Query().Get(name), nil
}

// lastPathSegment returns the final non-empty path segment of source_url (e.g. the
// SYMBOL in /api/v3/quote/AAPL). Empty if the URL has no path.
func lastPathSegment(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

// safeIdent constrains a value that will be interpolated into a sandbox command string
// (a series id or ticker symbol). It admits only letters, digits, '.', '_', and '-' —
// enough for real identifiers, but no quote/space/shell metacharacter that could break
// out of the single-quoted command (defense-in-depth alongside the env-injection of
// the actual secret). An empty value is rejected.
func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
