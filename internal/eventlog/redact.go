package eventlog

import (
	"math"
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
// The separator alternative tolerates an OPTIONAL intervening quote (["']?) before the
// `=`/`:`, so a credential buried as a JSON string-in-a-string — e.g. a model-echoed
// blob `{"api_key": "live_..."}` stored under a free-text field — is masked too. Without
// it the closing quote after the key name sits between the key and the `:`, and neither
// separator alternative matched (the credential leaked through).
var inlineSecretRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|client[_-]?secret|authorization|auth[_-]?token|bearer)(["']?[ \t]*[=:][ \t]*|[ \t]+)((?:bearer|basic|token)[ \t]+)?("?)([^\s"']{4,})`)

// flagSecretRe catches a credential passed as a CLI flag value (e.g. `mysql -p s3cr3t`,
// `curl --header 'Authorization: x'` is covered above; this covers `-p`, `--password`,
// `--token`, `--secret`, `--api-key`, `--access-key`).
var flagSecretRe = regexp.MustCompile(`(?i)((?:^|\s)(?:-p|--password|--passwd|--token|--secret|--api-key|--access-key|--auth-token)(?:[= ]))(\S{4,})`)

// tokenCandidateRe isolates the maximal token-shaped runs in a string — a contiguous
// stretch of the character set credentials actually use (letters, digits, and the
// url-/base64-safe punctuation `_ - + / = .`). The prefix/inline/flag rules above only
// catch KNOWN shapes; a bare high-entropy secret with no recognizable prefix (e.g. a
// random 40-char hex/base64 token the model pasted into a free-text field) slips
// through them. maskHighEntropyTokens scores each candidate and masks only the ones
// that look random, so ordinary prose and long identifiers survive.
var tokenCandidateRe = regexp.MustCompile(`[A-Za-z0-9_\-+/=.]{24,}`)

// hasLetter/hasDigit gate the entropy test cheaply: a credential mixes classes, while
// a long all-digit number (a timestamp, an id) or an all-lowercase word does not and
// must not be masked.
var (
	hasLetterRe = regexp.MustCompile(`[A-Za-z]`)
	hasDigitRe  = regexp.MustCompile(`[0-9]`)
)

// minTokenEntropyBits is the Shannon-entropy floor (in bits/char) above which a
// mixed-class 24+char run is treated as a random secret rather than structured text.
// Random base64/base62 credentials sit around 4.7–5.0 bits/char; version strings and
// hyphenated identifiers (e.g. "go1.25.0-darwin-arm64-release-build") sit near 4.2 and
// carry separators (screened by the low-separator gate below). The 4.0 floor PLUS the
// low-separator gate keeps false positives off real audit content while masking bare
// high-diversity credentials the prefix rules cannot know.
//
// Deliberately NOT masked by this last-resort belt: pure-HEX tokens. A 16-symbol (hex)
// alphabet caps at log2(16)=4.0 bits/char, so hex lands at/below this floor — and that
// is on purpose. Context-free hex is indistinguishable from, and vastly outnumbered by,
// the legitimate content hashes, hash-chain refs, and cache keys (e.g. the verify-cache
// key — a sha256) that fill this audit log; masking those would corrupt the trail and
// break hash-keyed lookups. A hex value that IS a secret is still caught when it carries
// context: an sk-/ghp_/AKIA-style prefix, or a `key=`/`token=`/`secret=` assignment, are
// masked by secretRe/inlineSecretRe/flagSecretRe upstream — those, not entropy, are the
// right signal for a keyed hex credential.
const minTokenEntropyBits = 4.0

// maxTokenSepFraction is the ceiling on the share of separator characters
// (`. - / + = _`) a candidate may contain and still be treated as a random secret.
// Real tokens are almost entirely alphanumeric (fraction ~0); a hyphen-/dot-delimited
// identifier like a version or a path segment carries a meaningful fraction and is
// therefore preserved. This is the discriminator that keeps structured, high-diversity
// strings out of the mask.
const maxTokenSepFraction = 0.12

// maskHighEntropyTokens replaces each maximal token-shaped run that looks like a
// random credential with "[redacted]", leaving surrounding text (and structured
// non-random tokens) intact. It is the last-resort belt for the highest-risk path:
// a bare secret with no known prefix (I3). It is deliberately conservative — a run
// must be long, mix character classes, AND clear the entropy floor to be masked, so
// prose, hashes-of-record we WANT to keep readable in the audit trail (the log's own
// chain hashes live in dedicated fields, not free text), and long identifiers are
// preserved.
func maskHighEntropyTokens(s string) string {
	return tokenCandidateRe.ReplaceAllStringFunc(s, func(tok string) string {
		if looksHighEntropySecret(tok) {
			return "[redacted]"
		}
		return tok
	})
}

// looksHighEntropySecret reports whether tok is a long, mixed-class, high-entropy,
// separator-sparse run — the signature of a random credential rather than structured
// text. All must hold: it contains BOTH a letter and a digit (rules out plain numbers
// and plain words), it is NOT mostly separator-delimited (rules out versions/paths),
// and its per-character Shannon entropy clears minTokenEntropyBits.
func looksHighEntropySecret(tok string) bool {
	if !hasLetterRe.MatchString(tok) || !hasDigitRe.MatchString(tok) {
		return false
	}
	if separatorFraction(tok) > maxTokenSepFraction {
		return false
	}
	return shannonBitsPerChar(tok) >= minTokenEntropyBits
}

// separatorFraction returns the share of tok that is url-/base64-safe punctuation
// (`. - / + = _`). Random credentials are almost pure alphanumeric; structured
// identifiers are punctuated.
func separatorFraction(tok string) float64 {
	if tok == "" {
		return 0
	}
	sep := 0
	for i := 0; i < len(tok); i++ {
		switch tok[i] {
		case '.', '-', '/', '+', '=', '_':
			sep++
		}
	}
	return float64(sep) / float64(len(tok))
}

// shannonBitsPerChar returns the Shannon entropy (bits per character) of s. A random
// token approaches log2(alphabet); repetitive or structured text stays low.
func shannonBitsPerChar(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	bits := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := c / n
		bits -= p * math.Log2(p)
	}
	return bits
}

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
		// Last-resort: mask a bare high-entropy token that none of the prefix rules
		// above recognized (I3). Runs AFTER them so an already-"[redacted]" span is
		// left alone (it is short and not token-shaped).
		s = maskHighEntropyTokens(s)
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
