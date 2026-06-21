package board

// redact.go is the board's secret-redaction + control-byte sanitizer for the few
// TRUSTED-but-still-defensively-treated fields the scoreboard renders verbatim
// (a shard row's Status, Verifier, Detail, and the key-free SourceURL). The SourceURL is
// REQUIRED key-free by I3, but a verifier Detail tail or a smuggled query param could
// still carry a key-shaped substring, and a crafted Detail could carry control bytes that
// repaint the terminal — so both the matrix renderer (internal/report/render) and this
// board renderer pass these fields through the SAME shapes before they reach output.
//
// WHY a local copy. internal/report/render keeps its redactor unexported and the board
// cannot import it without an import-direction inversion (report depends on the board's
// event contract, not the reverse). Per the established pattern (render.go itself mirrors
// internal/eventlog/redact.go's secretRe rather than importing it), we mirror the same
// anchored secret shapes + the same key-param strip + the same control/markup escape here,
// kept in sync with internal/report/render/{render,matrix}.go.

import (
	"net/url"
	"regexp"
	"strings"
)

// secretRe matches the anchored secret shapes — kept in sync with
// internal/report/render/render.go's secretRe and internal/eventlog/redact.go. A match is
// replaced with "[redacted]" before render so a key that leaked into a SourceURL or a
// verifier Detail tail is masked in every board surface (live scoreboard + TUI).
var secretRe = regexp.MustCompile(strings.Join([]string{
	`(?:sk|xoxb|xoxp|xoxa|xoxr|xoxs|xapp|ghp|gho|ghu|ghs|glpat)[-_][A-Za-z0-9_\-]{12,}`, // prefixed provider/CI tokens
	`AKIA[0-9A-Z]{16}`,                  // AWS access key id
	`ASIA[0-9A-Z]{16}`,                  // AWS temporary access key id
	`github_pat_[A-Za-z0-9_]{20,}`,      // GitHub fine-grained PAT
	`AIza[0-9A-Za-z\-_]{35}`,            // Google API key
	`-----BEGIN[ A-Z]*PRIVATE KEY-----`, // PEM private-key header
	`api_key=[A-Za-z0-9_\-]{8,}`,        // an api_key query param (keyed-source leak shape)
	`token=[A-Za-z0-9_\-]{8,}`,          // a token query param
}, "|"))

// keyParamNames is the closed set of query-param names that carry a credential — a
// param whose (lower-cased) name is in this set is dropped from a SourceURL entirely,
// regardless of value length, closing the gap the length-gated secretRe leaves (it only
// fires on values >=8 chars). Mirrors internal/report/render/matrix.go's keyParamNames.
var keyParamNames = map[string]bool{
	"api_key":      true,
	"apikey":       true,
	"key":          true,
	"token":        true,
	"access_token": true,
	"secret":       true,
	"password":     true,
	"sig":          true,
	"signature":    true,
}

// redact masks secret-looking substrings in a board field before it is rendered (I3).
func redact(s string) string {
	if s == "" {
		return s
	}
	return secretRe.ReplaceAllString(s, "[redacted]")
}

// redactSource masks a claim's SourceURL for board output (I3): it FIRST strips every
// key-looking query param by name (unconditional, any value length) and THEN runs the
// shared secretRe over the remainder, so neither a short ?api_key=secret nor an embedded
// provider key shape survives into a scoreboard row. A locator that does not parse as a
// URL still gets the secretRe pass. Mirrors internal/report/render/matrix.go.redactSource.
func redactSource(loc string) string {
	if loc == "" {
		return loc
	}
	if u, err := url.Parse(loc); err == nil && u.RawQuery != "" {
		q := u.Query()
		stripped := false
		for name := range q {
			if keyParamNames[strings.ToLower(name)] {
				q.Del(name)
				stripped = true
			}
		}
		if stripped {
			u.RawQuery = q.Encode()
			loc = u.String()
		}
	}
	return redact(loc)
}

// sanitizeCell neutralizes a field for a single-line TERMINAL cell (I7): it collapses
// every C0/DEL control byte (incl. ESC, CR, LF, TAB) to a space so no ANSI sequence or
// carriage-return overwrite survives, and HTML-escapes the markup metacharacters so a
// crafted <script>/<img onerror=> renders inert. Applied AFTER redact so a secret is
// masked first. Mirrors internal/report/render/matrix.go.escapeCell.
func sanitizeCell(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x7f || r < 0x20 {
			b.WriteByte(' ')
			continue
		}
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// safeField runs an UNTRUSTED-but-displayed field (Status/Verifier/Detail) through the
// secret redactor THEN the control/markup sanitizer — the order the matrix renderer uses.
func safeField(s string) string { return sanitizeCell(redact(s)) }

// safeSource runs a SourceURL through the key-param strip + secret redactor THEN the
// control/markup sanitizer, so a board-rendered locator can never leak a key (I3) or
// carry a terminal-repainting control byte (I7).
func safeSource(s string) string { return sanitizeCell(redactSource(s)) }
