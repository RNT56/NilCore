package verify

import (
	"context"
	"strings"
	"testing"

	"nilcore/internal/sandbox"
)

// fakeBox is a sandbox.Sandbox whose Exec returns a canned exit code per command
// substring, so verifier behavior is testable without a real sandbox.
type fakeBox struct {
	exit map[string]int // command-substring -> exit code; default 0
}

func (b fakeBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	for sub, code := range b.exit {
		if strings.Contains(cmd, sub) {
			return sandbox.Result{Stdout: cmd, ExitCode: code}, nil
		}
	}
	return sandbox.Result{Stdout: cmd, ExitCode: 0}, nil
}
func (b fakeBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b fakeBox) Workdir() string { return "/work" }

func TestCompositePassesWhenAllPass(t *testing.T) {
	c := Composite{Named: []NamedVerifier{
		{Name: "make verify", V: New(fakeBox{}, "make verify")},
		{Name: "browser", V: NewBrowser(fakeBox{}, "browser-check http://localhost:8080")},
	}}
	rep, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rep.Passed {
		t.Fatalf("want pass, got %+v", rep)
	}
}

func TestCompositeRedWhenBrowserFails(t *testing.T) {
	c := Composite{Named: []NamedVerifier{
		{Name: "make verify", V: New(fakeBox{}, "make verify")},
		{Name: "browser", V: NewBrowser(fakeBox{exit: map[string]int{"browser-check": 1}}, "browser-check http://localhost:8080")},
	}}
	rep, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Passed {
		t.Fatal("want red when the behavioral check fails even though build/test pass")
	}
	if !strings.Contains(rep.Output, "browser") {
		t.Errorf("output should name the failing check: %q", rep.Output)
	}
}

func TestCompositeShortCircuitsOnBuildFailure(t *testing.T) {
	browserRan := false
	c := Composite{Named: []NamedVerifier{
		{Name: "make verify", V: New(fakeBox{exit: map[string]int{"make verify": 2}}, "make verify")},
		{Name: "browser", V: browserSpy{&browserRan}},
	}}
	rep, _ := c.Check(context.Background())
	if rep.Passed {
		t.Fatal("want red when build fails")
	}
	if browserRan {
		t.Error("browser check must not run after a failing build (short-circuit)")
	}
}

type browserSpy struct{ ran *bool }

func (s browserSpy) Check(context.Context) (Report, error) {
	*s.ran = true
	return Report{Passed: true}, nil
}

func TestBrowserVerifierFailsClosed(t *testing.T) {
	// No box configured -> red, never a false green.
	rep, err := (&BrowserVerifier{}).Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Passed {
		t.Fatal("a misconfigured browser verifier must fail closed")
	}
}
