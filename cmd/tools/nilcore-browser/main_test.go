// Hermetic unit tests for the nilcore-browser driver. They cover the pure pieces
// — flag parsing, the Chromium arg builder, DOM→title/text extraction, console
// scraping, and the JSON encoder — plus the fail-closed behavior when Chromium is
// absent. The real browser run is exercised only by the CI browser-e2e job
// (mirroring the sandbox-linux pattern), since no Chromium is available here.
//
//go:build browserdriver

package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantURL    string
		wantFormat string
		wantErr    bool
	}{
		{
			name:       "contract form space-separated",
			args:       []string{"--url", "https://example.com", "--format", "json"},
			wantURL:    "https://example.com",
			wantFormat: "json",
		},
		{
			name:       "equals form",
			args:       []string{"--url=https://example.com", "--format=json"},
			wantURL:    "https://example.com",
			wantFormat: "json",
		},
		{
			name:       "format defaults to json",
			args:       []string{"--url", "https://example.com"},
			wantURL:    "https://example.com",
			wantFormat: "json",
		},
		{name: "missing url", args: []string{"--format", "json"}, wantErr: true},
		{name: "empty url", args: []string{"--url", "   "}, wantErr: true},
		{name: "url without value", args: []string{"--url"}, wantErr: true},
		{name: "format without value", args: []string{"--url", "u", "--format"}, wantErr: true},
		{name: "unexpected arg", args: []string{"--url", "u", "--evil"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, format, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseArgs(%v) = nil error, want error", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%v) unexpected error: %v", tt.args, err)
			}
			if url != tt.wantURL || format != tt.wantFormat {
				t.Fatalf("parseArgs(%v) = (%q,%q), want (%q,%q)", tt.args, url, format, tt.wantURL, tt.wantFormat)
			}
		})
	}
}

func TestChromiumArgs(t *testing.T) {
	args := chromiumArgs("/tmp/shot.png", "https://example.com/page")

	// The URL must be the final positional argument (passed as argv, never shell).
	if got := args[len(args)-1]; got != "https://example.com/page" {
		t.Fatalf("last arg = %q, want the URL", got)
	}

	want := map[string]bool{
		"--headless=new":             false,
		"--no-sandbox":               false,
		"--disable-gpu":              false,
		"--disable-dev-shm-usage":    false,
		"--dump-dom":                 false,
		"--screenshot=/tmp/shot.png": false,
	}
	for _, a := range args {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for flag, seen := range want {
		if !seen {
			t.Errorf("chromiumArgs missing required flag %q (got %v)", flag, args)
		}
	}

	// virtual-time-budget must be present and numeric (milliseconds).
	var foundVTB bool
	for _, a := range args {
		if strings.HasPrefix(a, "--virtual-time-budget=") {
			foundVTB = true
			val := strings.TrimPrefix(a, "--virtual-time-budget=")
			if val == "" || strings.ContainsAny(val, "abcdef") {
				t.Errorf("virtual-time-budget not numeric: %q", a)
			}
		}
	}
	if !foundVTB {
		t.Errorf("chromiumArgs missing --virtual-time-budget (got %v)", args)
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name string
		dom  string
		want string
	}{
		{name: "simple", dom: "<html><head><title>Hello</title></head></html>", want: "Hello"},
		{name: "case insensitive", dom: "<TITLE>Mixed Case</TITLE>", want: "Mixed Case"},
		{name: "attributes on tag", dom: `<title data-x="y">Attr Title</title>`, want: "Attr Title"},
		{name: "entities decoded", dom: "<title>Tom &amp; Jerry &lt;3</title>", want: "Tom & Jerry <3"},
		{name: "whitespace collapsed", dom: "<title>  a\n  b\tc </title>", want: "a b c"},
		{name: "newlines inside", dom: "<title>line1\nline2</title>", want: "line1 line2"},
		{name: "first wins", dom: "<title>First</title><title>Second</title>", want: "First"},
		{name: "absent", dom: "<html><body>no title</body></html>", want: ""},
		{name: "empty", dom: "<title></title>", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractTitle(tt.dom); got != tt.want {
				t.Fatalf("extractTitle(%q) = %q, want %q", tt.dom, got, tt.want)
			}
		})
	}
}

func TestDOMToText(t *testing.T) {
	tests := []struct {
		name    string
		dom     string
		want    string
		wantNot string
	}{
		{
			name: "strips tags and collapses",
			dom:  "<html><body><h1>Title</h1><p>Hello   world</p></body></html>",
			want: "Title Hello world",
		},
		{
			name:    "drops script bodies",
			dom:     `<body>visible<script>var secret="leak";</script>more</body>`,
			want:    "visible more",
			wantNot: "secret",
		},
		{
			name:    "drops style bodies",
			dom:     `<body>text<style>.x{color:red}</style></body>`,
			want:    "text",
			wantNot: "color:red",
		},
		{
			name: "decodes entities",
			dom:  "<p>A &amp; B</p>",
			want: "A & B",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := domToText(tt.dom)
			if got != tt.want {
				t.Fatalf("domToText(%q) = %q, want %q", tt.dom, got, tt.want)
			}
			if tt.wantNot != "" && strings.Contains(got, tt.wantNot) {
				t.Fatalf("domToText(%q) = %q, must not contain %q", tt.dom, got, tt.wantNot)
			}
		})
	}
}

func TestDOMToTextBounded(t *testing.T) {
	huge := "<body>" + strings.Repeat("a", maxText*2) + "</body>"
	if got := domToText(huge); len(got) > maxText {
		t.Fatalf("domToText did not bound length: got %d, want <= %d", len(got), maxText)
	}
}

func TestCollectConsole(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   []string
	}{
		{name: "empty", stderr: "", want: nil},
		{name: "only noise", stderr: "Opening in existing browser session.\nDevTools listening on ws://...", want: nil},
		{
			name:   "console line kept",
			stderr: "[12345:67890:INFO:CONSOLE(5)] \"hello from page\"\nbenign startup line",
			want:   []string{`[12345:67890:INFO:CONSOLE(5)] "hello from page"`},
		},
		{
			name:   "error and warning kept",
			stderr: "[x] ERROR:net.cc: failed\n[y] WARNING:render: slow\nnothing here",
			want:   []string{"[x] ERROR:net.cc: failed", "[y] WARNING:render: slow"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectConsole(tt.stderr)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("collectConsole(%q) = %#v, want %#v", tt.stderr, got, tt.want)
			}
		})
	}
}

func TestEncodeObservation(t *testing.T) {
	obs := observation{
		Title:         "Tom & Jerry",
		Text:          "1 < 2 && 3 > 2",
		Console:       []string{"console: hi"},
		ScreenshotB64: "QUJD",
	}
	out, err := encodeObservation(obs)
	if err != nil {
		t.Fatalf("encodeObservation error: %v", err)
	}

	// Must NOT HTML-escape the excerpt: the model reads it verbatim. With
	// SetEscapeHTML(false) the raw < > & survive, and the \uXXXX escape sequences
	// the default encoder would emit must be absent.
	for _, escSeq := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(string(out), escSeq) {
			t.Fatalf("encodeObservation emitted HTML escape %s: %s", escSeq, out)
		}
	}
	if !strings.Contains(string(out), "&") || !strings.Contains(string(out), "<") {
		t.Fatalf("encodeObservation should preserve raw < and &: %s", out)
	}

	// Must round-trip to the exact contract shape browser_view parses.
	var round struct {
		Title         string   `json:"title"`
		Text          string   `json:"text"`
		Console       []string `json:"console"`
		ScreenshotB64 string   `json:"screenshot_b64"`
	}
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("output not parseable as contract: %v (out=%s)", err, out)
	}
	if round.Title != obs.Title || round.Text != obs.Text || round.ScreenshotB64 != obs.ScreenshotB64 {
		t.Fatalf("round-trip mismatch: %#v vs %#v", round, obs)
	}
	if !reflect.DeepEqual(round.Console, obs.Console) {
		t.Fatalf("console round-trip mismatch: %#v vs %#v", round.Console, obs.Console)
	}

	// The exact JSON keys the contract requires must all be present.
	for _, key := range []string{`"title"`, `"text"`, `"console"`, `"screenshot_b64"`} {
		if !strings.Contains(string(out), key) {
			t.Errorf("encodeObservation missing key %s in %s", key, out)
		}
	}
}

func TestResolveChromiumMissingFailsClosed(t *testing.T) {
	// Point at a binary that cannot exist on PATH; resolution must error (the
	// driver then exits non-zero so browser_view fails closed).
	if _, err := resolveChromium("nilcore-no-such-browser-xyzzy"); err == nil {
		t.Fatal("resolveChromium with absent binary = nil error, want fail-closed error")
	}
}

func TestResolveChromiumDefault(t *testing.T) {
	// An empty override falls back to the default name. Whether it resolves depends
	// on the host; we only assert the fallback path is taken (no panic) and that
	// the error, if any, names the default binary.
	_, err := resolveChromium("")
	if err != nil && !strings.Contains(err.Error(), defaultChromium) {
		t.Fatalf("default resolution error should name %q: %v", defaultChromium, err)
	}
}

func TestRunFailsClosedWhenChromiumMissing(t *testing.T) {
	t.Setenv(envChromium, "nilcore-no-such-browser-xyzzy")
	err := run(context.Background(), []string{"--url", "https://example.com", "--format", "json"}, os.Stdout)
	if err == nil {
		t.Fatal("run with missing chromium = nil error, want fail-closed error")
	}
}

func TestRunRejectsNonJSONFormat(t *testing.T) {
	err := run(context.Background(), []string{"--url", "https://example.com", "--format", "html"}, os.Stdout)
	if err == nil {
		t.Fatal("run with --format html = nil error, want error")
	}
}

func TestRunPrintsContractWithFakeBrowser(t *testing.T) {
	// Substitute the browser invocation so we can drive run() end-to-end without a
	// real Chromium and assert it prints exactly the contract object.
	orig := runBrowser
	t.Cleanup(func() { runBrowser = orig })
	runBrowser = func(_ context.Context, _, url string) (observation, error) {
		return observation{
			Title:         "Fixture",
			Text:          "body text",
			Console:       []string{"console: ok"},
			ScreenshotB64: "QUJD",
		}, nil
	}
	// resolveChromium still runs; provide a path that exists so it succeeds.
	t.Setenv(envChromium, "/bin/sh")

	tmp, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--url", "https://example.com", "--format", "json"}, tmp); err != nil {
		t.Fatalf("run error: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	var obs observation
	if err := json.Unmarshal(data, &obs); err != nil {
		t.Fatalf("run output not contract JSON: %v (out=%s)", err, data)
	}
	if obs.Title != "Fixture" || obs.Text != "body text" || obs.ScreenshotB64 != "QUJD" {
		t.Fatalf("run printed unexpected observation: %#v", obs)
	}
}

func TestBuildObservation(t *testing.T) {
	dom := "<html><head><title>Page</title></head><body><p>Hi there</p></body></html>"
	obs := buildObservation(dom, "QUJD", "[x] CONSOLE(1): note\nbenign")
	if obs.Title != "Page" {
		t.Errorf("title = %q, want Page", obs.Title)
	}
	if !strings.Contains(obs.Text, "Hi there") {
		t.Errorf("text = %q, want it to contain 'Hi there'", obs.Text)
	}
	if obs.ScreenshotB64 != "QUJD" {
		t.Errorf("screenshot = %q, want QUJD", obs.ScreenshotB64)
	}
	if len(obs.Console) != 1 {
		t.Errorf("console = %#v, want one kept line", obs.Console)
	}
}
