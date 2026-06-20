package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// The keyed checks (finance.fred_series, finance.market_quote) reach a public API that
// requires an API key. The discipline (I3) is strict and tested:
//
//   - The key VALUE is read from the process environment at run time — the wiring task
//     (P11-T12) populates that env from the SecretStore; this leaf only knows the env
//     var NAME. The value is placed ONLY into the per-invocation env map passed to
//     box.ExecWithEnv, so it reaches the box for one command and is never persisted.
//   - The fully-resolved request URL (key-free base + key) is built in Go and handed to
//     the box THROUGH the env map under a dedicated var ($NILCORE_KEYED_URL). The curl
//     COMMAND STRING references only that var, inside DOUBLE quotes ("$NILCORE_KEYED_URL"),
//     so the shell expands it inside the box — the key never appears in the command
//     string, in the persisted Evidence.SourceURL, or in an event Detail. (Single quotes
//     here would have been the bug: sh -c passes the literal "$VAR" to the API, 401/403.)
//   - The request URL is DERIVED at run time from a key-free public base plus the
//     injected key; a model-written full URL is never trusted for a keyed reach, and
//     the persisted Evidence.SourceURL stays the canonical, key-free public URL.
//   - No key supplied (env var empty) ⇒ Unverifiable, never Pass.

// envKeyedURL is the dedicated env var that carries the fully-resolved (key-bearing)
// request URL into the box for one invocation. It rides ONLY in the env map; the curl
// command references it by name inside double quotes, so the value never lands in the
// command string, a log, the persisted SourceURL, or an event Detail.
const envKeyedURL = "NILCORE_KEYED_URL"

// fetchKeyedBody runs a keyed curl GET. buildURL turns the live key value into the
// FULLY-RESOLVED request URL (key-free base + key); it is invoked here, where the key is
// read, so the key value flows only through buildURL and into the env map — never through
// a parameter that a caller might log. envName names the env var holding the key value.
//
// The resolved (key-bearing) URL is handed to the box ONLY via the $NILCORE_KEYED_URL env
// map entry. The command references it by name inside DOUBLE quotes so the box-side shell
// expands it; single quotes here would pass the literal "$NILCORE_KEYED_URL" text and the
// real endpoint would never be reached. A missing key fails closed (ok=false).
func fetchKeyedBody(ctx context.Context, box sandbox.Sandbox, envName string, buildURL func(key string) string) (body string, ok bool, why string) {
	if box == nil {
		return "", false, "no sandbox available (refusing host-side request)"
	}
	keyVal := strings.TrimSpace(os.Getenv(envName))
	if keyVal == "" {
		// No key ⇒ no decisive verdict is possible. Fail closed (never Pass).
		return "", false, fmt.Sprintf("no API key supplied ($%s unset)", envName)
	}
	resolvedURL := buildURL(keyVal)
	// The command references the resolved URL by env-var NAME only (double-quoted so the
	// box-side shell expands it); the URL VALUE — which carries the key — rides in the env
	// map for this single invocation and never touches the command string.
	cmd := fmt.Sprintf("curl -fsSL --max-time 30 --max-redirs 5 \"$%s\"", envKeyedURL)
	env := map[string]string{envKeyedURL: resolvedURL}
	res, err := box.ExecWithEnv(ctx, cmd, env)
	if err != nil {
		return "", false, "sandbox: " + err.Error()
	}
	if res.ExitCode != 0 {
		d := strings.TrimSpace(res.Stderr)
		if d == "" {
			d = fmt.Sprintf("curl exited %d", res.ExitCode)
		}
		return "", false, "non-2xx or unreachable: " + d
	}
	return res.Stdout, true, ""
}

// fredBaseURL is the key-free public FRED base; the persisted Evidence.SourceURL is this
// canonical URL with no api_key query param. It is a var (not a const) only so tests can
// redirect the keyed reach at a local server — production never reassigns it.
var fredBaseURL = "https://api.stlouisfed.org/fred/series/observations"

// checkFREDSeries (finance.fred_series, keyed) asserts that the most recent FRED
// observation for the series equals Value. The series id is taken from Evidence.Value's
// sibling — we read it from Evidence.SourceURL's query (series_id) so the model still
// declares WHICH series, but the key-bearing request URL is DERIVED here from the
// key-free base plus $NILCORE_FRED_KEY. The persisted SourceURL stays key-free.
func checkFREDSeries(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	seriesID, err := queryParam(c.Evidence.SourceURL, "series_id")
	if err != nil || seriesID == "" {
		return artifact.StatusUnverifiable, detail("fred: series_id missing from source_url")
	}
	if !safeIdent(seriesID) {
		return artifact.StatusUnverifiable, detail("fred: series_id has unsafe characters")
	}
	// DERIVED request URL, built in Go from the key-free base plus the live key. The key
	// rides only into the env map (via fetchKeyedBody); it never enters the command string
	// or the persisted SourceURL. file_type=json gives a JSON body; sort_order=desc&limit=1
	// yields the newest observation first.
	buildURL := func(key string) string {
		return fmt.Sprintf("%s?series_id=%s&file_type=json&sort_order=desc&limit=1&api_key=%s",
			fredBaseURL, seriesID, url.QueryEscape(key))
	}

	body, ok, why := fetchKeyedBody(ctx, box, EnvFREDKey, buildURL)
	if !ok {
		return artifact.StatusUnverifiable, detail(why)
	}
	val, found := fredLatest(body)
	if !found {
		return artifact.StatusUnverifiable, detail("fred: no observation in response")
	}
	matched, m := numericMatch(c.Evidence.Value, val, false)
	if matched {
		return artifact.StatusPass, detail(m)
	}
	return artifact.StatusFail, detail(m)
}

// fredLatest pulls the newest observation value from a FRED observations JSON body:
// {"observations":[{"date":...,"value":"..."}]} (sorted desc, so index 0 is newest).
func fredLatest(body string) (float64, bool) {
	var doc struct {
		Observations []struct {
			Value string `json:"value"`
		} `json:"observations"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return 0, false
	}
	for _, o := range doc.Observations {
		if o.Value == "" || o.Value == "." { // FRED encodes a missing value as "."
			continue
		}
		var f float64
		if _, err := fmt.Sscanf(o.Value, "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// fmpBase is the key-free public Financial Modeling Prep quote base; the persisted
// Evidence.SourceURL is this canonical URL with no apikey query param.
const fmpBase = "https://financialmodelingprep.com/api/v3/quote"

// checkMarketQuote (finance.market_quote, keyed) asserts that the live quote price for
// a symbol equals Value within tolerance. The symbol is taken from the model-declared
// source_url (the /quote/<SYMBOL> path), but the key-bearing request URL is DERIVED
// here from the key-free base plus $NILCORE_MARKET_KEY. The persisted SourceURL stays
// key-free.
func checkMarketQuote(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	symbol := lastPathSegment(c.Evidence.SourceURL)
	if symbol == "" || !safeIdent(symbol) {
		return artifact.StatusUnverifiable, detail("market_quote: symbol missing/unsafe in source_url")
	}
	// DERIVED request URL, built in Go; the key rides only into the env map (via
	// fetchKeyedBody), never into the command string or the persisted SourceURL.
	buildURL := func(key string) string {
		return fmt.Sprintf("%s/%s?apikey=%s", fmpBase, symbol, url.QueryEscape(key))
	}

	body, ok, why := fetchKeyedBody(ctx, box, EnvMarketKey, buildURL)
	if !ok {
		return artifact.StatusUnverifiable, detail(why)
	}
	val, found := fmpPrice(body)
	if !found {
		return artifact.StatusUnverifiable, detail("market_quote: no price in response")
	}
	matched, m := numericMatch(c.Evidence.Value, val, false)
	if matched {
		return artifact.StatusPass, detail(m)
	}
	return artifact.StatusFail, detail(m)
}

// fmpPrice pulls the price from an FMP quote body: [{"symbol":...,"price":123.4}].
func fmpPrice(body string) (float64, bool) {
	var quotes []struct {
		Price json.Number `json:"price"`
	}
	if err := json.Unmarshal([]byte(body), &quotes); err != nil || len(quotes) == 0 {
		return 0, false
	}
	if quotes[0].Price.String() == "" {
		return 0, false
	}
	f, err := quotes[0].Price.Float64()
	if err != nil {
		return 0, false
	}
	return f, true
}
