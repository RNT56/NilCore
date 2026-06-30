package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// secUserAgent identifies the verifier to data.sec.gov, which rejects requests without
// a User-Agent. It carries no secret — it is a public courtesy header.
const secUserAgent = "NilCore-Verifier/1.0 (artifact-verifier)"

// checkSECFact (finance.sec_fact) asserts that a named fact in an SEC companyfacts
// document equals the claim's Value. The claim's SourceURL points at the key-free
// data.sec.gov companyfacts JSON; the claim's Field names the us-gaap (or other
// taxonomy) concept; the most recent reported value for that concept is compared to
// Evidence.Value with the documented numeric tolerance (1e-6 relative for floats,
// exact for ints).
//
//   - Pass        — the named fact resolves and matches Value within tolerance.
//   - Fail        — the fact resolves but the value is wrong.
//   - Unverifiable — non-2xx/unreachable, parse error, or the named fact is absent.
//
// The body is UNTRUSTED data parsed by trusted Go (no guard.Wrap before parsing, I7);
// only a bounded harness detail leaves the pack.
func checkSECFact(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	safeURL, err := validatePublicURL(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	body, err := curlBody(ctx, box, safeURL, secUserAgent, nil)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}

	val, isInt, ok := secLatestFact(body, c.Field)
	if !ok {
		return artifact.StatusUnverifiable, detail(fmt.Sprintf("fact %q not found in companyfacts", c.Field))
	}
	matched, why := numericMatch(c.Evidence.Value, val, isInt)
	if matched {
		return artifact.StatusPass, detail(why)
	}
	return artifact.StatusFail, detail(why)
}

// secLatestFact extracts the most recent reported value for concept from an SEC
// companyfacts JSON body. SEC shape: facts.<taxonomy>.<concept>.units.<unit>[].{val,end}.
// It scans every taxonomy for the concept, takes the unit datum with the latest "end"
// date, and reports whether the JSON number was integral (no fractional part) so the
// caller can apply exact-int vs tolerant-float comparison. Returns ok=false if the
// concept or any numeric datum is absent.
func secLatestFact(body, concept string) (val float64, isInt, ok bool) {
	var doc struct {
		Facts map[string]map[string]struct {
			Units map[string][]struct {
				Val json.Number `json:"val"`
				End string      `json:"end"`
			} `json:"units"`
		} `json:"facts"`
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return 0, false, false
	}
	type datum struct {
		end string
		raw json.Number
	}
	var found []datum
	for _, taxonomy := range doc.Facts {
		concrete, present := taxonomy[concept]
		if !present {
			continue
		}
		for _, series := range concrete.Units {
			for _, d := range series {
				found = append(found, datum{end: d.End, raw: d.Val})
			}
		}
	}
	if len(found) == 0 {
		return 0, false, false
	}
	// Latest "end" wins (ISO dates sort lexicographically).
	sort.Slice(found, func(i, j int) bool { return found[i].end < found[j].end })
	latest := found[len(found)-1].raw
	f, err := latest.Float64()
	if err != nil {
		return 0, false, false
	}
	_, intErr := latest.Int64()
	return f, intErr == nil, true
}

// checkWorldBankIndicator (finance.worldbank_indicator) asserts that a World Bank
// indicator's most recent value equals Value. The World Bank API returns a 2-element
// JSON array [meta, [datapoints...]] where each datapoint is {value, date}. The claim's
// SourceURL is the key-free api.worldbank.org indicator endpoint (format=json).
func checkWorldBankIndicator(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	safeURL, err := validatePublicURL(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	body, err := curlBody(ctx, box, safeURL, "", nil)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}

	val, ok := worldBankLatest(body)
	if !ok {
		return artifact.StatusUnverifiable, detail("no datapoint in World Bank response")
	}
	// World Bank values are floats; never treat as exact-int.
	matched, why := numericMatch(c.Evidence.Value, val, false)
	if matched {
		return artifact.StatusPass, detail(why)
	}
	return artifact.StatusFail, detail(why)
}

// worldBankLatest pulls the most recent non-null indicator value. The response is
// [meta, [{value,date}, ...]]. We do NOT trust the response ORDERING (it is shaped by
// the model-authored SourceURL query — e.g. &sort=asc or a date range could put the
// OLDEST point first); instead we sort by the parsed `date` field DESCENDING and take
// the latest non-null value, mirroring secLatestFact/imfLatest so the verdict never
// depends on a model-influenced order (I2).
func worldBankLatest(body string) (float64, bool) {
	var doc []json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil || len(doc) < 2 {
		return 0, false
	}
	var points []struct {
		Value json.Number `json:"value"`
		Date  string      `json:"date"`
	}
	if err := json.Unmarshal(doc[1], &points); err != nil {
		return 0, false
	}
	// Latest "date" wins (World Bank dates are years or ISO dates, both sorting
	// lexicographically). Sort descending so the newest point is first.
	sort.Slice(points, func(i, j int) bool { return points[i].Date > points[j].Date })
	for _, p := range points {
		if p.Value.String() == "" {
			continue
		}
		if f, err := p.Value.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

// checkIMFSeries (finance.imf_series) asserts that the most recent observation in an
// IMF data series equals Value. The IMF SDMX-JSON CompactData shape is
// CompactData.DataSet.Series.Obs[].{@TIME_PERIOD,@OBS_VALUE}; we take the observation
// with the latest TIME_PERIOD. The claim's SourceURL is the key-free www.imf.org
// (services.imf.org-style) endpoint.
func checkIMFSeries(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	safeURL, err := validatePublicURL(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	body, err := curlBody(ctx, box, safeURL, "", nil)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}

	val, ok := imfLatest(body)
	if !ok {
		return artifact.StatusUnverifiable, detail("no observation in IMF series")
	}
	matched, why := numericMatch(c.Evidence.Value, val, false)
	if matched {
		return artifact.StatusPass, detail(why)
	}
	return artifact.StatusFail, detail(why)
}

// imfLatest pulls the latest observation value from an IMF SDMX-JSON CompactData body.
// Obs may be a single object or an array, so we decode into a flexible shape and take
// the lexicographically-latest TIME_PERIOD.
func imfLatest(body string) (float64, bool) {
	var doc struct {
		CompactData struct {
			DataSet struct {
				Series struct {
					Obs json.RawMessage `json:"Obs"`
				} `json:"Series"`
			} `json:"DataSet"`
		} `json:"CompactData"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return 0, false
	}
	type obs struct {
		Time string `json:"@TIME_PERIOD"`
		Val  string `json:"@OBS_VALUE"`
	}
	raw := doc.CompactData.DataSet.Series.Obs
	if len(raw) == 0 {
		return 0, false
	}
	var list []obs
	if err := json.Unmarshal(raw, &list); err != nil {
		var single obs
		if err2 := json.Unmarshal(raw, &single); err2 != nil {
			return 0, false
		}
		list = []obs{single}
	}
	if len(list) == 0 {
		return 0, false
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Time < list[j].Time })
	latest := list[len(list)-1]
	var f float64
	if _, err := fmt.Sscanf(latest.Val, "%g", &f); err != nil {
		return 0, false
	}
	return f, true
}
