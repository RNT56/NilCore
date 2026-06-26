package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"nilcore/internal/blastbudget"
)

// fakeRun is an injected exec seam: it records that the real command ran and
// returns a canned result, so the wall-time fence's charge/reconcile path is
// exercisable without launching a container.
func newFakeContainer(ran *bool) *Container {
	c := NewContainer("podman", "img", "/tmp/wt")
	c.run = func(ctx context.Context, _ []string) (Result, error) {
		*ran = true
		return Result{ExitCode: 0, Stdout: "ok"}, nil
	}
	return c
}

func TestExecWithEnv_NilBlastByteIdentical(t *testing.T) {
	var ran bool
	c := newFakeContainer(&ran)
	// no Blast attached ⇒ the fence block is skipped entirely.
	res, err := c.ExecWithEnv(context.Background(), "echo hi", nil)
	if err != nil || res.ExitCode != 0 || !ran {
		t.Fatalf("nil-Blast path must run the command unchanged: ran=%v res=%+v err=%v", ran, res, err)
	}
}

func TestExecWithEnv_WallChargeAndReconcile(t *testing.T) {
	var ran bool
	c := newFakeContainer(&ran)
	b := blastbudget.New()
	b.SetWallCeiling(10 * time.Minute) // budget present, plenty remaining
	c.Blast = b

	res, err := c.ExecWithEnv(context.Background(), "echo hi", nil)
	if err != nil || res.ExitCode != 0 || !ran {
		t.Fatalf("within-budget exec must run: ran=%v res=%+v err=%v", ran, res, err)
	}
	// After reconcile, only the (tiny) actual elapsed remains charged — far below
	// the bound that was pre-charged, proving the unused remainder was credited.
	if u := b.Used(""); u.Wall >= time.Minute {
		t.Errorf("wall after reconcile = %v, want ~the actual elapsed (well under a minute)", u.Wall)
	}
}

func TestExecWithEnv_WallBreachRefusesBeforeRun(t *testing.T) {
	var ran bool
	c := newFakeContainer(&ran)
	b := blastbudget.New()
	b.SetWallCeiling(time.Second)
	// Exhaust the wall budget so the remaining is <= 0.
	if err := b.ChargeWall(context.Background(), time.Second); err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	c.Blast = b

	res, err := c.ExecWithEnv(context.Background(), "echo hi", nil)
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
