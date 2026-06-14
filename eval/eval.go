// Package eval is the measure-first harness (P5-T03): it scores configs (backends,
// models) on a suite of coding tasks with objective, verifier-based pass/fail, and
// reports pass rate, cost, and latency. Its structured output is the evidence the
// router uses to earn strength-routing — improvements come from data, not vibes.
package eval

import (
	"context"
	"encoding/json"
	"time"
)

// Case is one eval task: a goal scored by an objective check.
type Case struct {
	Name string
	Goal string
}

// Result is one case's outcome under one config.
type Result struct {
	Case    string        `json:"case"`
	Config  string        `json:"config"`
	Passed  bool          `json:"passed"`
	Latency time.Duration `json:"latency_ns"`
	Cost    float64       `json:"cost"`
}

// Report aggregates results for a config.
type Report struct {
	Config    string   `json:"config"`
	Results   []Result `json:"results"`
	PassRate  float64  `json:"pass_rate"`
	TotalCost float64  `json:"total_cost"`
}

// Runner runs one case under a config and returns its objective pass/fail + cost.
// In production this drives the orchestrator against a fixture repo; the verifier
// decides pass/fail, so the score is honest.
type Runner func(ctx context.Context, c Case) (passed bool, cost float64)

// Run executes every case under run, timing each, and returns a structured report.
func Run(ctx context.Context, cases []Case, config string, run Runner) Report {
	rep := Report{Config: config}
	passed := 0
	for _, c := range cases {
		start := time.Now()
		ok, cost := run(ctx, c)
		rep.Results = append(rep.Results, Result{
			Case: c.Name, Config: config, Passed: ok, Latency: time.Since(start), Cost: cost,
		})
		rep.TotalCost += cost
		if ok {
			passed++
		}
	}
	if len(cases) > 0 {
		rep.PassRate = float64(passed) / float64(len(cases))
	}
	return rep
}

// JSON renders the report as structured JSON the router can consume.
func (r Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }
