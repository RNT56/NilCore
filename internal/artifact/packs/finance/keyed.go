package finance

import (
	"context"
	"encoding/json"
	"fmt"
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
//   - The curl COMMAND STRING references the key by shell-variable NAME ($NILCORE_FRED_KEY),
//     never the literal value — so the value can never appear in a logged command, in
//     the persisted Evidence.SourceURL, or in an event Detail.
//   - The request URL is DERIVED at run time from a key-free public base plus the
//     injected $NAME; a model-written full URL is never trusted for a keyed reach, and
//     the persisted Evidence.SourceURL stays the canonical, key-free public URL.
//   - No key supplied (env var empty) ⇒ Unverifiable, never Pass.

// fetchKeyedBody runs a keyed curl GET. urlTemplate is a key-free command-string
// fragment that may reference the shell variable $envName (e.g. ".../series?key=$NILCORE_FRED_KEY").
// The literal key value is placed only into the env map, never into urlTemplate. A
// missing key (empty env value) returns ok=false so the caller fails closed.
//
// The full URL is built by curl's shell expansion inside the box from a key-free
// template; we still validate the key-free portion (the persisted SourceURL) up front.
func fetchKeyedBody(ctx context.Context, box sandbox.Sandbox, envName, urlTemplateWithVar string) (body string, ok bool, why string) {
	if box == nil {
		return "", false, "no sandbox available (refusing host-side request)"
	}
	keyVal := strings.TrimSpace(os.Getenv(envName))
	if keyVal == "" {
		// No key ⇒ no decisive verdict is possible. Fail closed (never Pass).
		return "", false, fmt.Sprintf("no API key supplied ($%s unset)", envName)
	}
	// The command references the key by NAME only; the VALUE rides in the env map for
	// this single invocation. curl performs the $VAR expansion inside the box.
	cmd := fmt.Sprintf("curl -fsSL --max-time 30 --max-redirs 5 '%s'", urlTemplateWithVar)
	env := map[string]string{envName: keyVal}
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

// fredBase is the key-free public FRED base; the persisted Evidence.SourceURL is this
// canonical URL with no api_key query param.
const fredBase = "https://api.stlouisfed.org/fred/series/observations"

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
	// DERIVED, key-free template with the key referenced by NAME only. file_type=json
	// gives a JSON body; sort_order=desc&limit=1 yields the newest observation first.
	tmpl := fmt.Sprintf("%s?series_id=%s&file_type=json&sort_order=desc&limit=1&api_key=$%s",
		fredBase, seriesID, EnvFREDKey)

	body, ok, why := fetchKeyedBody(ctx, box, EnvFREDKey, tmpl)
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
	tmpl := fmt.Sprintf("%s/%s?apikey=$%s", fmpBase, symbol, EnvMarketKey)

	body, ok, why := fetchKeyedBody(ctx, box, EnvMarketKey, tmpl)
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
