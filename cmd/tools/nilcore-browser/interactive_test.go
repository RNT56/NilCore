// Hermetic unit tests for the interactive (flow) path. They cover the pure
// pieces — the --actions extractor, the relaxed flag parser, the steps
// parser/validator, the destination guard, the Chrome arg builder, and the
// applyStep dispatch (against a recording fake) — with NO real browser. The live
// runInteractive run is exercised only by the CI browser-e2e job, exactly as the
// batch path's live capture is.
package main

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestExtractActions(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantActions string
		wantRest    []string
		wantErr     bool
	}{
		{
			name:        "absent leaves args untouched",
			args:        []string{"--url", "http://x/", "--format", "json"},
			wantActions: "",
			wantRest:    []string{"--url", "http://x/", "--format", "json"},
		},
		{
			name:        "space form pulled out",
			args:        []string{"--actions", "[]", "--url", "http://x/"},
			wantActions: "[]",
			wantRest:    []string{"--url", "http://x/"},
		},
		{
			name:        "equals form pulled out",
			args:        []string{"--actions=[1]", "--url=http://x/"},
			wantActions: "[1]",
			wantRest:    []string{"--url=http://x/"},
		},
		{name: "missing value", args: []string{"--actions"}, wantErr: true},
		{name: "empty value", args: []string{"--actions", "  "}, wantErr: true},
		{name: "given twice", args: []string{"--actions", "[]", "--actions", "[]"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotActions, gotRest, err := extractActions(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("extractActions(%v) = nil error, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractActions(%v) unexpected error: %v", tt.args, err)
			}
			if gotActions != tt.wantActions {
				t.Errorf("actions = %q, want %q", gotActions, tt.wantActions)
			}
			if !reflect.DeepEqual(gotRest, tt.wantRest) {
				t.Errorf("rest = %v, want %v", gotRest, tt.wantRest)
			}
		})
	}
}

func TestParseInteractiveArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantURL    string
		wantFormat string
		wantErr    bool
	}{
		{name: "url optional", args: []string{}, wantURL: "", wantFormat: "json"},
		{name: "url provided", args: []string{"--url", "http://x/"}, wantURL: "http://x/", wantFormat: "json"},
		{name: "equals form", args: []string{"--url=http://x/", "--format=json"}, wantURL: "http://x/", wantFormat: "json"},
		{name: "unknown flag rejected", args: []string{"--evil"}, wantErr: true},
		{name: "url without value", args: []string{"--url"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, format, err := parseInteractiveArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseInteractiveArgs(%v) = nil error, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if url != tt.wantURL || format != tt.wantFormat {
				t.Fatalf("= (%q,%q), want (%q,%q)", url, format, tt.wantURL, tt.wantFormat)
			}
		})
	}
}

func TestParseSteps(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantLen int
		wantErr bool
	}{
		{
			name:    "full valid flow",
			json:    `[{"action":"navigate","url":"http://x/"},{"action":"click","selector":"#login"},{"action":"type","selector":"#user","text":"alice"},{"action":"key","key":"Enter"},{"action":"wait","ms":500}]`,
			wantLen: 5,
		},
		{name: "empty array", json: `[]`, wantErr: true},
		{name: "not an array", json: `{"action":"navigate"}`, wantErr: true},
		{name: "navigate missing url", json: `[{"action":"navigate"}]`, wantErr: true},
		{name: "click missing selector", json: `[{"action":"click"}]`, wantErr: true},
		{name: "type missing selector", json: `[{"action":"type","text":"x"}]`, wantErr: true},
		{name: "key missing key", json: `[{"action":"key"}]`, wantErr: true},
		{name: "wait negative ms", json: `[{"action":"wait","ms":-5}]`, wantErr: true},
		{name: "wait too long", json: `[{"action":"wait","ms":999999}]`, wantErr: true},
		{name: "unknown action", json: `[{"action":"teleport"}]`, wantErr: true},
		{name: "missing action", json: `[{"url":"http://x/"}]`, wantErr: true},
		{name: "unknown field rejected", json: `[{"action":"wait","ms":1,"evil":true}]`, wantErr: true},
		{name: "malformed json", json: `[{"action":`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps, err := parseSteps(tt.json)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSteps(%q) = nil error, want error", tt.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSteps(%q) unexpected error: %v", tt.json, err)
			}
			if len(steps) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(steps), tt.wantLen)
			}
		})
	}
}

func TestParseStepsExceedsMax(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < maxSteps+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"action":"wait","ms":0}`)
	}
	b.WriteByte(']')
	if _, err := parseSteps(b.String()); err == nil {
		t.Fatal("parseSteps over the step limit = nil error, want error")
	}
}

func TestRequireDestination(t *testing.T) {
	navStep := []step{{Action: actNavigate, URL: "http://x/"}}
	clickStep := []step{{Action: actClick, Selector: "#a"}}
	tests := []struct {
		name    string
		url     string
		steps   []step
		wantErr bool
	}{
		{name: "url present", url: "http://x/", steps: clickStep},
		{name: "navigate step present", url: "", steps: navStep},
		{name: "neither", url: "", steps: clickStep, wantErr: true},
		{name: "blank url, no navigate", url: "   ", steps: clickStep, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireDestination(tt.url, tt.steps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("requireDestination(%q, ...) err=%v, wantErr=%v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestInteractiveChromiumArgs(t *testing.T) {
	args := interactiveChromiumArgs(9222, "/tmp/ud")

	// Must use the debugging port (not the batch flags), pinned to loopback.
	want := map[string]bool{
		"--headless=new":                       false,
		"--no-sandbox":                         false,
		"--disable-gpu":                        false,
		"--remote-debugging-port=9222":         false,
		"--remote-debugging-address=127.0.0.1": false,
		"--user-data-dir=/tmp/ud":              false,
	}
	for _, a := range args {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for flag, seen := range want {
		if !seen {
			t.Errorf("interactiveChromiumArgs missing %q (got %v)", flag, args)
		}
	}

	// Must NOT carry the one-shot batch flags — those are mutually exclusive with
	// driving a live debugging session.
	for _, a := range args {
		if strings.HasPrefix(a, "--dump-dom") || strings.HasPrefix(a, "--screenshot") {
			t.Errorf("interactive args must not include batch flag %q", a)
		}
	}
}

// recordingDriver is a fake stepDriver that records dispatched calls so applyStep
// can be tested without a real browser.
type recordingDriver struct {
	calls []string
}

func (r *recordingDriver) Navigate(_ context.Context, url string) error {
	r.calls = append(r.calls, "navigate:"+url)
	return nil
}

func (r *recordingDriver) ClickSelector(_ context.Context, sel string) error {
	r.calls = append(r.calls, "click:"+sel)
	return nil
}

func (r *recordingDriver) TypeIntoSelector(_ context.Context, sel, text string) error {
	r.calls = append(r.calls, "type:"+sel+"="+text)
	return nil
}

func (r *recordingDriver) TypeKey(_ context.Context, key string) error {
	r.calls = append(r.calls, "key:"+key)
	return nil
}

func TestApplyStepDispatch(t *testing.T) {
	d := &recordingDriver{}
	ctx := context.Background()
	steps := []step{
		{Action: actNavigate, URL: "http://x/"},
		{Action: actClick, Selector: "#login"},
		{Action: actType, Selector: "#user", Text: "alice"},
		{Action: actKey, Key: "Enter"}, // discrete key press (e.g. submit)
		{Action: actWait, MS: 0},       // 0ms wait returns immediately, no driver call
	}
	for _, s := range steps {
		if err := applyStep(ctx, d, s); err != nil {
			t.Fatalf("applyStep(%+v): %v", s, err)
		}
	}
	want := []string{"navigate:http://x/", "click:#login", "type:#user=alice", "key:Enter"}
	if !reflect.DeepEqual(d.calls, want) {
		t.Fatalf("dispatched calls = %v, want %v", d.calls, want)
	}
}

func TestApplyStepWaitHonorsCancel(t *testing.T) {
	d := &recordingDriver{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	err := applyStep(ctx, d, step{Action: actWait, MS: maxWaitMS})
	if err == nil {
		t.Fatal("applyStep wait with cancelled ctx = nil error, want ctx error")
	}
}

// TestRunInteractiveFailsClosedWhenChromiumMissing confirms the --actions path
// fails closed (non-zero) when no browser is present — the same guarantee the
// batch path gives — without launching anything.
func TestRunInteractiveFailsClosedWhenChromiumMissing(t *testing.T) {
	t.Setenv(envChromium, "nilcore-no-such-browser-xyzzy")
	args := []string{
		"--actions", `[{"action":"navigate","url":"http://127.0.0.1:1/"}]`,
		"--format", "json",
	}
	if err := run(context.Background(), args, os.Stdout); err == nil {
		t.Fatal("run --actions with missing chromium = nil error, want fail-closed error")
	}
}

// TestRunInteractiveRejectsBadActions confirms a malformed --actions value is
// rejected loudly (the steps parser runs before any browser launch).
func TestRunInteractiveRejectsBadActions(t *testing.T) {
	// Point at a binary that exists so resolveChromium succeeds and we reach the
	// steps parser; an unknown action must then error.
	t.Setenv(envChromium, "/bin/sh")
	args := []string{"--actions", `[{"action":"teleport"}]`, "--url", "http://x/"}
	if err := run(context.Background(), args, os.Stdout); err == nil {
		t.Fatal("run with an unknown action = nil error, want error")
	}
}

// TestRunInteractiveRequiresDestination confirms an --actions flow with neither a
// --url nor a navigate step is rejected before launching a browser.
func TestRunInteractiveRequiresDestination(t *testing.T) {
	t.Setenv(envChromium, "/bin/sh")
	args := []string{"--actions", `[{"action":"click","selector":"#x"}]`}
	if err := run(context.Background(), args, os.Stdout); err == nil {
		t.Fatal("run with no destination = nil error, want error")
	}
}
