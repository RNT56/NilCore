// Package finance is the finance/market domain verifier pack (Phase 11, Pillar 2).
// It registers a small, typed catalog of verifier-ids that assert a financial datum
// against an authoritative public source, each reduced to ONE reach into the worker's
// sandbox (I4) followed by a trusted host-side parse of the response. The pack is a
// LEAF: it imports only artifact, evverify, sandbox (and the standard library); it
// never imports the orchestrator (super/roster/agent) nor any finance/SEC SDK — the
// whole point is curl-in-box + encoding/json, so go.mod stays unchanged (I6).
//
// Two trust disciplines are structural here:
//
//   - Keyless checks (finance.sec_fact, finance.worldbank_indicator, finance.imf_series)
//     reach public, key-free endpoints. The response body is UNTRUSTED data parsed by
//     trusted Go (verifier code, not the model) — no guard.Wrap before parsing — and
//     only a bounded, harness-authored detail tail leaves the pack (I7).
//   - Keyed checks (finance.fred_series, finance.market_quote) DERIVE their request URL
//     from a key-free public base plus an injected $NAME at run time, and call
//     box.ExecWithEnv so the key VALUE reaches the box only for that single invocation
//     (I3). The literal key never appears in the command string (only the shell
//     variable name does), never in the persisted Evidence.SourceURL, and never in the
//     emitted event Detail. A keyed check with no key supplied fails closed to
//     Unverifiable — it can never become Pass.
//
// Every check is fail-closed: a nil Box, a denied/unreachable host, a non-2xx status,
// or a parse error is StatusUnverifiable (no decisive verdict), never StatusPass.
package finance

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"nilcore/internal/artifact/evverify"
	"nilcore/internal/sandbox"
)

// Verifier-ids registered by this pack. They are namespaced under "finance." so a
// claim names e.g. Evidence.Verifier = "finance.sec_fact". The keyed ids reference a
// SecretStore env var by NAME only; the wiring task injects the VALUE.
const (
	IDSecFact            = "finance.sec_fact"
	IDWorldBankIndicator = "finance.worldbank_indicator"
	IDIMFSeries          = "finance.imf_series"
	IDFREDSeries         = "finance.fred_series"  // keyed: $NILCORE_FRED_KEY
	IDMarketQuote        = "finance.market_quote" // keyed: $NILCORE_MARKET_KEY
)

// Env var NAMES for the keyed checks. Only the NAME lives in this leaf; the VALUE is
// injected by the wiring (P11-T12) from the SecretStore via box.ExecWithEnv (I3). The
// name is referenced as a shell variable ($NAME) in the curl command so the literal
// secret never appears in the command string.
const (
	EnvFREDKey   = "NILCORE_FRED_KEY"
	EnvMarketKey = "NILCORE_MARKET_KEY"
)

// floatTolerance is the relative tolerance applied when comparing a model-authored
// Evidence.Value against a float fact fetched from the source. Floats match iff
// |fetched-claimed| <= floatTolerance*max(1,|fetched|). 1e-6 absorbs JSON float
// round-tripping and source-side rounding without admitting a materially different
// number.
//
// EXACT-INT comparison (no tolerance) fires ONLY when BOTH sides are integral. In
// practice that is the sec_fact check, the one caller that probes the fetched value's
// integrality (secLatestFact's Int64 probe) and passes fetchedIsInt=true; the
// worldbank/imf/fred/market checks treat their fetched values as floats (those sources
// are float series), so an integer claim against them goes through the tolerant-float
// path, not the exact-int branch. (See numericMatch.)
const floatTolerance = 1e-6

// RegisterAll adds this pack's five verifier-ids to r. It registers exactly the three
// keyless and two keyed checks and nothing else — an unregistered id elsewhere stays
// Unverifiable. Registration is the single seam Pillar 2 fills (evverify.Registry).
func RegisterAll(r *evverify.Registry) {
	r.Register(IDSecFact, checkSECFact)
	r.Register(IDWorldBankIndicator, checkWorldBankIndicator)
	r.Register(IDIMFSeries, checkIMFSeries)
	r.Register(IDFREDSeries, checkFREDSeries)
	r.Register(IDMarketQuote, checkMarketQuote)
}

// Hosts is the documented egress host-set this pack reaches. It is co-designed with
// the finance egress profile (P11-T25) and cross-checked by P11-T35 (every pack host
// must be a subset of its profile). Keep it in sync with the endpoints below.
var Hosts = []string{
	"data.sec.gov",              // sec_fact (companyfacts)
	"api.worldbank.org",         // worldbank_indicator
	"www.imf.org",               // imf_series
	"api.stlouisfed.org",        // fred_series (keyed)
	"financialmodelingprep.com", // market_quote (keyed)
}

// maxDetail bounds the harness-authored detail tail so a verifier note can never flood
// the artifact JSON or an event Detail. (Kept local so the leaf imports no orchestrator
// package; mirrors evverify's own bound.)
const maxDetail = 512

// detail trims a harness-authored note to the bounded tail. It carries verifier
// commentary ONLY — never the raw remote body and never a model-authored field echoed
// unfenced (I7).
func detail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxDetail {
		return s[len(s)-maxDetail:]
	}
	return s
}

// validatePublicURL constrains a model-authored SourceURL before it is placed into a
// sandbox command: http/https only, a host present, and no single quote / whitespace /
// control byte, so the URL cannot break out of single-quoting and smuggle a second
// command. Mirrors evverify.validateURL (kept local so the leaf stays self-contained).
func validatePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("source_url is required")
	}
	for _, r := range raw {
		if r == '\'' {
			return "", fmt.Errorf("source_url may not contain a single quote")
		}
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("source_url may not contain whitespace or control characters")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid source_url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("only http and https source_url is allowed (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("source_url has no host")
	}
	return raw, nil
}

// curlBody runs a single curl GET inside the box and returns the response body. -fsS
// makes curl exit non-zero on an HTTP error status, so a non-2xx is surfaced as a
// non-nil okErr (the caller maps it to Unverifiable). A nil box or a sandbox-level
// error is likewise non-fatal-to-Go but fails the check closed. The body is UNTRUSTED
// data; the caller parses it with trusted Go.
//
// header sets a value for the User-Agent (data.sec.gov rejects requests without one).
// The URL must already be validated and is single-quoted; env (nil for keyless) is
// injected per-invocation for keyed checks.
func curlBody(ctx context.Context, box sandbox.Sandbox, safeURL, userAgent string, env map[string]string) (body string, okErr error) {
	if box == nil {
		// No sandbox to reach through. Refuse rather than fall back to a host-side
		// request, which would bypass the sandbox boundary and the egress policy (I4).
		return "", fmt.Errorf("no sandbox available (refusing host-side request)")
	}
	ua := ""
	if userAgent != "" {
		ua = fmt.Sprintf("-A '%s' ", userAgent)
	}
	cmd := fmt.Sprintf("curl -fsSL %s--max-time 30 --max-redirs 5 '%s'", ua, safeURL)
	res, err := box.ExecWithEnv(ctx, cmd, env)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	if res.ExitCode != 0 {
		d := strings.TrimSpace(res.Stderr)
		if d == "" {
			d = fmt.Sprintf("curl exited %d", res.ExitCode)
		}
		return "", fmt.Errorf("non-2xx or unreachable: %s", d)
	}
	return res.Stdout, nil
}

// ensure the package compiles against the stable CheckFunc signature.
var (
	_ evverify.CheckFunc = checkSECFact
	_ evverify.CheckFunc = checkWorldBankIndicator
	_ evverify.CheckFunc = checkIMFSeries
	_ evverify.CheckFunc = checkFREDSeries
	_ evverify.CheckFunc = checkMarketQuote
)
