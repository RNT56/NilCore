// Package browse is the Phase-14 browser-agent evaluation harness (Pillar 7). It
// measures what separates a demo from production: not just "does it work once"
// (pass@1) but "does it work EVERY time under realistic faults" (pass^k). The
// WAREX finding (arXiv:2510.03285) is the motivation — injecting realistic
// network/server faults collapses naive web agents from ~42% to ~2% success, so a
// browse eval that never injects faults over-reports reliability.
//
// This file holds the PURE, hermetically-testable core: a deterministic fault plan,
// a scenario catalog, a grading predicate, and the pass@1 / pass^k reliability
// math. The LIVE runner — driving `nilcore browse` against a fixture server behind
// a fault-injecting proxy — is a CI-only seam (no Chromium in unit tests), exactly
// like the browser-e2e job; it consumes these pure pieces. Keeping the scoring pure
// means the reliability numbers are themselves verifiable, not vibes.
package browse

import "sort"

// FaultKind is one realistic fault a reliability run injects (WAREX taxonomy).
type FaultKind string

const (
	FaultNone         FaultKind = ""
	FaultNetworkDelay FaultKind = "network_delay" // slow response — exercises the DOM-stability wait
	FaultHTTP5xx      FaultKind = "http_5xx"      // server error — exercises recovery/retry
	FaultHTTP4xx      FaultKind = "http_4xx"      // client error — exercises fail-closed handling
	FaultJSError      FaultKind = "js_error"      // script failure — exercises partial-render handling
	FaultPopup        FaultKind = "popup"         // interstitial — exercises distraction resistance
)

// AllFaults is the catalog a full reliability sweep iterates.
func AllFaults() []FaultKind {
	return []FaultKind{FaultNetworkDelay, FaultHTTP5xx, FaultHTTP4xx, FaultJSError, FaultPopup}
}

// FaultPlan maps a 0-based step index to the fault injected at that step. It is
// deterministic (no randomness) so a reliability run is reproducible and a
// regression is attributable to a code change, not a dice roll.
type FaultPlan struct {
	Faults map[int]FaultKind
}

// Plan builds a deterministic plan that injects kind every everyN steps (starting
// at step everyN-1, 0-based) up to maxSteps. everyN<=0 or kind==FaultNone yields an
// empty plan (the clean baseline). It is the simplest reproducible injector; richer
// schedules compose multiple Plans via Merge.
func Plan(kind FaultKind, everyN, maxSteps int) FaultPlan {
	p := FaultPlan{Faults: map[int]FaultKind{}}
	if kind == FaultNone || everyN <= 0 {
		return p
	}
	for step := everyN - 1; step < maxSteps; step += everyN {
		p.Faults[step] = kind
	}
	return p
}

// Merge overlays other onto p (other wins on a shared step), returning a new plan.
func (p FaultPlan) Merge(other FaultPlan) FaultPlan {
	out := FaultPlan{Faults: map[int]FaultKind{}}
	for k, v := range p.Faults {
		out.Faults[k] = v
	}
	for k, v := range other.Faults {
		out.Faults[k] = v
	}
	return out
}

// FaultAt returns the fault scheduled for a step, or FaultNone.
func (p FaultPlan) FaultAt(step int) FaultKind {
	if p.Faults == nil {
		return FaultNone
	}
	return p.Faults[step]
}

// Steps returns the injected step indices in ascending order (for logging/asserting).
func (p FaultPlan) Steps() []int {
	steps := make([]int, 0, len(p.Faults))
	for s := range p.Faults {
		steps = append(steps, s)
	}
	sort.Ints(steps)
	return steps
}

// Scenario is one browse-agent task with a machine-checkable success criterion. It
// is consumed by the CI live runner (which executes `nilcore browse` against a
// fixture) and graded by Grade. The success criterion is deliberately concrete —
// an exact value the run must extract — so grading is a runnable assertion, never a
// judgment call (the same I2 discipline the verifier packs use).
type Scenario struct {
	Name     string
	Goal     string
	StartURL string
	// ExpectField/ExpectValue: extraction scenarios assert the run recorded a finding
	// FIELD whose verified VALUE equals ExpectValue. (A non-extraction scenario may
	// leave these empty and assert via ExpectText.)
	ExpectField string
	ExpectValue string
	// ExpectText, when set, is a substring the final page/observation must contain.
	ExpectText string
}

// Outcome is one run's graded result.
type Outcome struct {
	Achieved bool
	Steps    int
	Detail   string
}

// Grade is the PURE success predicate: an extraction scenario is achieved iff the
// run recorded+verified the expected field/value; a text scenario iff the expected
// substring appeared. recorded maps field→verified-value (only claims the harness
// confirmed Pass belong here); finalText is the last observation's text. Grading
// never trusts the agent's self-report — only harness-confirmed facts (I2).
func Grade(s Scenario, recorded map[string]string, finalText string) Outcome {
	if s.ExpectField != "" {
		got, ok := recorded[s.ExpectField]
		if ok && got == s.ExpectValue {
			return Outcome{Achieved: true, Detail: "expected finding verified"}
		}
		return Outcome{Achieved: false, Detail: "expected finding missing or unverified"}
	}
	if s.ExpectText != "" {
		if containsFold(finalText, s.ExpectText) {
			return Outcome{Achieved: true, Detail: "expected text present"}
		}
		return Outcome{Achieved: false, Detail: "expected text absent"}
	}
	return Outcome{Achieved: false, Detail: "scenario has no success criterion"}
}

// Reliability aggregates K runs of one (scenario, fault) cell. pass@1 is the
// single-run success RATE (capability); pass^k is whether ALL K runs succeeded
// (reliability/consistency — the metric a demo's single run hides).
type Reliability struct {
	Scenario  string
	Fault     FaultKind
	Runs      int
	Successes int
}

// Record folds one outcome into the tally.
func (r *Reliability) Record(o Outcome) {
	r.Runs++
	if o.Achieved {
		r.Successes++
	}
}

// PassAt1 is the single-run success rate in [0,1] — the pass@1 capability estimate.
func (r Reliability) PassAt1() float64 {
	if r.Runs == 0 {
		return 0
	}
	return float64(r.Successes) / float64(r.Runs)
}

// PassPowK reports whether EVERY run succeeded — pass^k for k=Runs (the reliability
// gate: a cell that ever fails under a fault is not production-ready).
func (r Reliability) PassPowK() bool {
	return r.Runs > 0 && r.Successes == r.Runs
}

// DefaultScenarios is the seed catalog the CI live runner executes. They target a
// local fixture server (served by the e2e job), so they carry no live-internet
// dependency and grade deterministically.
func DefaultScenarios() []Scenario {
	return []Scenario{
		{
			Name:        "extract-version",
			Goal:        "Find the latest release version on the releases page and record it as latest_version.",
			StartURL:    "http://fixture.local/releases",
			ExpectField: "latest_version",
			ExpectValue: "v1.4.2",
		},
		{
			Name:       "navigate-and-confirm",
			Goal:       "Open the docs home and confirm the getting-started section is present.",
			StartURL:   "http://fixture.local/docs",
			ExpectText: "getting started",
		},
		{
			Name:        "form-login-extract",
			Goal:        "Log in with the test credentials and record the account name shown after login as account_name.",
			StartURL:    "http://fixture.local/login",
			ExpectField: "account_name",
			ExpectValue: "Test User",
		},
	}
}

// containsFold is a tiny case-insensitive substring check (kept local so the
// package stays dependency-free beyond sort).
func containsFold(hay, needle string) bool {
	if needle == "" {
		return false
	}
	h, n := toLower(hay), toLower(needle)
	return indexOf(h, n) >= 0
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func indexOf(hay, needle string) int {
	if len(needle) == 0 || len(needle) > len(hay) {
		return -1
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
