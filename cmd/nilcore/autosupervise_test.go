package main

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"

	"nilcore/internal/agent"
)

// errBoom models a classifier transport fault (timeout / API error / budget ceiling).
var errBoom = errors.New("boom")

// TestAutoSuperviseFlagRegistered proves the -auto-supervise flag exists on the
// run-style common flag surface and defaults to FALSE (off). The whole byte-identical
// guarantee rests on this default, so it is asserted explicitly.
func TestAutoSuperviseFlagRegistered(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := registerCommon(fs)
	if c.autoSupervise == nil {
		t.Fatal("registerCommon did not register -auto-supervise")
	}
	if *c.autoSupervise {
		t.Error("-auto-supervise must default to false (off = single-task, byte-identical)")
	}
	if f := fs.Lookup("auto-supervise"); f == nil {
		t.Error("the -auto-supervise flag is not visible on the flag set (run -h would not show it)")
	}
	// Setting it flips the bool.
	if err := fs.Parse([]string{"-auto-supervise"}); err != nil {
		t.Fatalf("parse -auto-supervise: %v", err)
	}
	if !*c.autoSupervise {
		t.Error("-auto-supervise did not flip to true when passed")
	}
}

// TestWireAutoSuperviseDefaultOff is the byte-identical proof: with -auto-supervise
// UNSET (the default), wireAutoSupervise leaves Project and ShouldSupervise NIL, so
// Orchestrator.Execute takes the single-task path exactly as today. It returns a
// non-nil (no-op) cleanup the caller can defer unconditionally.
func TestWireAutoSuperviseDefaultOff(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	b := fakeBoot()

	c := buildCommon(t, []string{"-log", logPath}) // no -auto-supervise
	o := &agent.Orchestrator{BaseRepo: dir}
	// prov is nil here on purpose: even with a provider the off path must not touch
	// Project/ShouldSupervise, and nil also proves no classifier call is attempted.
	cleanup := wireAutoSupervise(o, c, b, nil, logAt(t, logPath), "anything at all")
	if cleanup == nil {
		t.Fatal("wireAutoSupervise must return a non-nil cleanup (a no-op when off)")
	}
	cleanup() // a no-op must be safe to call
	if o.Project != nil {
		t.Error("Project must stay nil when -auto-supervise is off (single-task path)")
	}
	if o.ShouldSupervise != nil {
		t.Error("ShouldSupervise must stay nil when -auto-supervise is off (single-task path)")
	}
}

// TestAutoSuperviseTriggerModelDriven proves the trigger is MODEL-DRIVEN when a
// native provider is available: a classifier "supervise"/"project" route trips the
// trigger, "native"/"chat" does not. This is the consistency-with-Part-1 property —
// run's scale-up decision now flows through the same authoritative classifier.
func TestAutoSuperviseTriggerModelDriven(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	log := logAt(t, logPath)

	cases := []struct {
		name  string
		reply string
		want  bool
	}{
		{"supervise route trips the trigger", `{"route":"supervise","reason":"multi-file"}`, true},
		{"project route trips the trigger", `{"route":"project","reason":"scaffold"}`, true},
		{"native route does not", `{"route":"native","reason":"one-liner"}`, false},
		{"chat route does not", `{"route":"chat","reason":"a question"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &replyProvider{id: "fake", reply: tc.reply}
			trigger := autoSuperviseTrigger(prov, log)
			if got := trigger("rewrite the auth subsystem"); got != tc.want {
				t.Errorf("trigger = %v, want %v (route %q)", got, tc.want, tc.reply)
			}
			if prov.calls != 1 {
				t.Errorf("classifier calls = %d, want exactly 1 (one metered sizing call)", prov.calls)
			}
		})
	}
}

// TestAutoSuperviseTriggerClassifierErrorFallsBack proves a classifier transport
// fault degrades to the cheap chatShouldSupervise heuristic rather than failing the
// run — the supervised seam is an enhancement, never required. A heuristic-complex
// goal still trips; a heuristic-simple goal does not.
func TestAutoSuperviseTriggerClassifierErrorFallsBack(t *testing.T) {
	dir := t.TempDir()
	log := logAt(t, filepath.Join(dir, "events.jsonl"))

	prov := &replyProvider{id: "fake", err: errBoom}
	trigger := autoSuperviseTrigger(prov, log)

	// "build a ..." is a chatShouldSupervise trigger keyword ⇒ complex ⇒ true.
	if !trigger("build a whole new payments service from scratch") {
		t.Error("on a classifier error a heuristic-complex goal must still trip the trigger")
	}
	// A short localized ask the heuristic sizes simple ⇒ false.
	if trigger("fix a typo") {
		t.Error("on a classifier error a heuristic-simple goal must not trip the trigger")
	}
}

// TestAutoSuperviseTriggerNoProviderUsesHeuristic proves that with NO native model
// provider (a delegated codex/claude-code backend), the trigger is chatShouldSupervise
// outright — so the supervised capability is still gained, just heuristic-driven.
func TestAutoSuperviseTriggerNoProviderUsesHeuristic(t *testing.T) {
	dir := t.TempDir()
	log := logAt(t, filepath.Join(dir, "events.jsonl"))

	trigger := autoSuperviseTrigger(nil, log)
	if !trigger("scaffold an end-to-end service with tests") {
		t.Error("no-provider trigger must use chatShouldSupervise (this goal is heuristic-complex)")
	}
	if trigger("rename a field") {
		t.Error("no-provider trigger must size this short ask simple")
	}
}
