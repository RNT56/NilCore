//go:build linux

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"nilcore/internal/blastbudget"
)

// newFakeNamespace injects an exec seam so the wall-time fence's charge/reconcile
// path is exercisable without a real namespace re-exec.
func newFakeNamespace(ran *bool) *Namespace {
	return &Namespace{
		HostDir: "/tmp/wt",
		run: func(ctx context.Context, _ string, _ map[string]string) (Result, error) {
			*ran = true
			return Result{ExitCode: 0, Stdout: "ok"}, nil
		},
	}
}

// With no budget attached the namespace fence block is skipped entirely and the
// command runs unchanged.
func TestNamespaceExecWithEnv_NilBlastByteIdentical(t *testing.T) {
	var ran bool
	n := newFakeNamespace(&ran)
	res, err := n.ExecWithEnv(context.Background(), "echo hi", nil)
	if err != nil || res.ExitCode != 0 || !ran {
		t.Fatalf("nil-Blast path must run unchanged: ran=%v res=%+v err=%v", ran, res, err)
	}
}

// A within-budget exec runs and reconciles: after the run only the (tiny) actual
// elapsed remains charged, proving the pre-charged bound's unused remainder was
// credited back — mirroring the container backend's fence.
func TestNamespaceExecWithEnv_WallChargeAndReconcile(t *testing.T) {
	var ran bool
	n := newFakeNamespace(&ran)
	b := blastbudget.New()
	b.SetWallCeiling(10 * time.Minute)
	n.Blast = b

	res, err := n.ExecWithEnv(context.Background(), "echo hi", nil)
	if err != nil || res.ExitCode != 0 || !ran {
		t.Fatalf("within-budget exec must run: ran=%v res=%+v err=%v", ran, res, err)
	}
	if u := b.Used(""); u.Wall >= time.Minute {
		t.Errorf("wall after reconcile = %v, want ~the actual elapsed (well under a minute)", u.Wall)
	}
}

// An exhausted wall budget refuses BEFORE running the real command, returning a
// non-zero Result (not a Go error) — the wall ceiling is now enforced on the
// default Linux backend, not silently ignored.
func TestNamespaceExecWithEnv_WallBreachRefusesBeforeRun(t *testing.T) {
	var ran bool
	n := newFakeNamespace(&ran)
	b := blastbudget.New()
	b.SetWallCeiling(time.Second)
	if err := b.ChargeWall(context.Background(), time.Second); err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	n.Blast = b

	res, err := n.ExecWithEnv(context.Background(), "echo hi", nil)
	if err != nil {
		t.Fatalf("a budget-refused command is a Result, not an error: %v", err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "blast-radius") {
		t.Errorf("breach must return a non-zero Result with a blast-radius stderr, got %+v", res)
	}
	if ran {
		t.Errorf("the real command must NEVER run on a wall breach")
	}
}
