package telegram

import (
	"errors"
	"strings"
	"testing"
)

// TestRedactErrStripsToken proves redactErr strips the bot token from an error before it
// is returned. Telegram carries the token in the request URL PATH, so a *url.Error from
// the HTTP layer echoes it in Error() (Go redacts only URL userinfo, not the path). If
// the token rode a call error into a log it would breach I3 (no secrets in the trail).
func TestRedactErrStripsToken(t *testing.T) {
	const token = "123456:ABC-DEF_ghIJKlmNoPQRstuVwxyz"
	b := &Bot{token: token}

	// A url.Error-shaped message with the token embedded in the path, as the HTTP
	// client would produce it.
	err := errors.New(`Post "https://api.telegram.org/bot` + token + `/sendMessage": dial tcp: connection refused`)
	red := b.redactErr("sendMessage", err)
	got := red.Error()

	if strings.Contains(got, token) {
		t.Errorf("token leaked through redactErr: %q present in %q", token, got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("expected the token to be masked with [redacted]; got %q", got)
	}
	// The method and the surrounding, non-secret error text must survive so the error
	// stays actionable.
	if !strings.Contains(got, "sendMessage") {
		t.Errorf("method context lost: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("non-secret error detail lost: %q", got)
	}
}

// TestRedactErrEmptyTokenNoSpuriousRedaction guards the `b.token != ""` check: with an
// empty token, strings.ReplaceAll(msg, "", "[redacted]") would splice "[redacted]"
// between every character. The guard must skip replacement, returning the message
// (wrapped) unchanged.
func TestRedactErrEmptyTokenNoSpuriousRedaction(t *testing.T) {
	b := &Bot{token: ""}
	red := b.redactErr("getUpdates", errors.New("dial tcp: connection refused"))
	got := red.Error()
	if strings.Contains(got, "[redacted]") {
		t.Errorf("empty token produced spurious redaction: %q", got)
	}
	if !strings.Contains(got, "connection refused") || !strings.Contains(got, "getUpdates") {
		t.Errorf("empty-token error was not returned sensibly: %q", got)
	}
}

// TestRedactErrTokenAbsentReturnsSensibly covers an error that simply does not contain
// the token (e.g. a marshal error before the URL is built): it is wrapped and returned
// intact, with no redaction marker.
func TestRedactErrTokenAbsentReturnsSensibly(t *testing.T) {
	b := &Bot{token: "123456:SECRETTOKENVALUE"}
	red := b.redactErr("sendMessage", errors.New("json: unsupported value"))
	got := red.Error()
	if strings.Contains(got, "[redacted]") {
		t.Errorf("token-absent error should carry no redaction marker: %q", got)
	}
	if !strings.Contains(got, "json: unsupported value") || !strings.Contains(got, "sendMessage") {
		t.Errorf("token-absent error was not returned sensibly: %q", got)
	}
}
