package main

// kernel_equiv_test.go is the UOK equivalence harness (UOK-T09): it proves a
// kernel-routed run is EVENT-FOR-EVENT identical to the legacy machine call. The cutover
// rests on this — the *ViaKernel helpers wrap the SAME machine as the kernel envelope's
// Flat runner, so routing through kernel.Run must add nothing to the event log and must
// return the same outcome. We assert it over a REAL agent.Orchestrator (the FLAT path,
// the representative I2/gate-bearing shape) with a hermetic fake backend + verifier; the
// build/swarm helpers use the identical wrap pattern, and the kernel's own
// transparency is proven in internal/kernel.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"nilcore/internal/agent"
	"nilcore/internal/backend"
	"nilcore/internal/eventlog"
	"nilcore/internal/verify"
)

type equivBackend struct{}

func (equivBackend) Name() string { return "equiv" }
func (equivBackend) Run(context.Context, backend.Task) (backend.Result, error) {
	return backend.Result{Backend: "equiv", Summary: "did work", SelfClaimed: true}, nil
}

type equivVerifier struct{ pass bool }

func (v equivVerifier) Check(context.Context) (verify.Report, error) {
	return verify.Report{Passed: v.pass}, nil
}

// TestKernelEnabledDefaultOnWithEscapeHatch proves the flip: the kernel is the DEFAULT
// engine (unset ⇒ on), with an opt-out escape hatch to the legacy machine path.
func TestKernelEnabledDefaultOnWithEscapeHatch(t *testing.T) {
	t.Setenv("NILCORE_KERNEL", "")
	if !kernelEnabled() {
		t.Fatal("unset NILCORE_KERNEL must default ON (the unified kernel is the engine)")
	}
	for _, off := range []string{"0", "off", "OFF", "false", "no", " no "} {
		t.Setenv("NILCORE_KERNEL", off)
		if kernelEnabled() {
			t.Errorf("NILCORE_KERNEL=%q must route to the legacy machine (escape hatch)", off)
		}
	}
	for _, on := range []string{"1", "yes", "true", "on"} {
		t.Setenv("NILCORE_KERNEL", on)
		if !kernelEnabled() {
			t.Errorf("NILCORE_KERNEL=%q must keep the kernel on", on)
		}
	}
}

func initEquivGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@nilcore.local", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return repo
}

// logKinds reads the ordered event Kinds from a JSONL event log.
func logKinds(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var kinds []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var e struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind != "" {
			kinds = append(kinds, e.Kind)
		}
	}
	return kinds
}

// runOrchOnce builds a real single-task orchestrator over a fresh temp repo + log and
// runs one task through runViaKernel with the kernel ON or OFF. It returns the ordered
// event Kinds + the outcome so the two can be compared.
func runOrchOnce(t *testing.T, kernelOn bool) ([]string, agent.Outcome) {
	t.Helper()
	repo := initEquivGitRepo(t)
	logPath := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	orch := &agent.Orchestrator{
		BaseRepo: repo,
		NewEnv:   func(string) agent.Env { return agent.Env{Backend: equivBackend{}, Verifier: equivVerifier{pass: true}} },
		Log:      log,
		Router:   agent.SingleRouter{},
		Spawner:  agent.NoSpawner{},
	}
	if kernelOn {
		t.Setenv("NILCORE_KERNEL", "1")
	} else {
		t.Setenv("NILCORE_KERNEL", "0") // the escape hatch — route directly to the legacy machine
	}
	out, err := runViaKernel(context.Background(), orch, backend.Task{ID: "equiv-1", Goal: "do a thing"})
	if err != nil {
		t.Fatalf("runViaKernel(kernelOn=%v): %v", kernelOn, err)
	}
	if cerr := log.Close(); cerr != nil {
		t.Fatalf("close log: %v", cerr)
	}
	return logKinds(t, logPath), out
}

// TestKernelEquivalence_Run proves the cutover is safe: routing `run` through the kernel
// emits the SAME event-log Kind sequence and returns the SAME outcome as the legacy
// direct orch.Execute. The kernel adds nothing — it is a transparent unified entry.
func TestKernelEquivalence_Run(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	offKinds, offOut := runOrchOnce(t, false)
	onKinds, onOut := runOrchOnce(t, true)

	if len(offKinds) == 0 {
		t.Fatal("legacy run emitted no events — the harness is not exercising the orchestrator")
	}
	if !reflect.DeepEqual(offKinds, onKinds) {
		t.Fatalf("kernel-routed run must be event-for-event identical to legacy\n legacy = %v\n kernel = %v", offKinds, onKinds)
	}
	if offOut != onOut {
		t.Fatalf("kernel-routed outcome must equal legacy: %+v vs %+v", offOut, onOut)
	}
	if !onOut.Verified {
		t.Fatal("the equiv run should verify green (sanity)")
	}
}
