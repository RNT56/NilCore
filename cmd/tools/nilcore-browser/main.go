// Command nilcore-browser is the operator-trusted, in-sandbox headless-browser
// driver that internal/tools/browser.go (the browser_view tool) shells out to.
// The tool runs exactly:
//
//	nilcore-browser --url '<url>' --format json
//
// and json-Unmarshals our stdout into {title, text, console, screenshot_b64} — or,
// for a flow, `nilcore-browser --actions '<json>' [--url '<url>'] --format json`.
// We print EXACTLY that object and exit non-zero on any failure so the tool fails
// closed (it never fabricates a passing observation).
//
// TWO PATHS, one contract. The BATCH path (no --actions) drives a real headless
// Chromium through its built-in one-shot flags (--headless=new --dump-dom
// --screenshot ...), which Chrome supports precisely for scripted capture — no
// persistent connection needed. The FLOW path (--actions: navigate/click/type/key/
// wait) needs to *act* between renders, which the batch flags can't, so it launches
// a long-lived headless Chrome with a debugging port and drives it over the pure-Go
// Chrome DevTools Protocol client in `internal/cdp` (a minimal RFC6455 WebSocket +
// CDP, stdlib only). Both honor I6 — zero external dependencies, no tree-sitter-style
// native bindings — and build with CGO_ENABLED=0.
//
// It is a standalone tool (compiled into the sandbox image; see images/sandbox/),
// NOT linked into the default nilcore binary — but it is pure stdlib, so it builds
// under the default `go build ./...` and its pure-logic tests run under
// `make verify`. The browser run itself (batch and the --actions flow) is exercised
// only by a CI e2e job against a fixture server — same pattern as the sandbox-linux
// job — because no real Chromium is available in unit-test environments.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		// Stderr carries the human-readable cause; the tool surfaces it as fenced,
		// UNTRUSTED data. A non-zero exit is the fail-closed signal browser_view
		// keys on (res.ExitCode != 0).
		fmt.Fprintln(os.Stderr, "nilcore-browser:", err)
		os.Exit(1)
	}
}

// run is the testable seam for main: it parses flags, locates Chromium, invokes
// it, and prints the JSON observation. The browser invocation is isolated behind
// the runBrowser function variable so tests can drive run without a real browser.
func run(ctx context.Context, args []string, stdout *os.File) error {
	// The interactive flow path is opt-in via --actions; we split it out first so
	// the batch path's flag set (and its tests) are unchanged when --actions is
	// absent. When present, we drive Chrome over CDP through the steps in order.
	actionsJSON, rest, err := extractActions(args)
	if err != nil {
		return err
	}

	chromium, err := resolveChromium(os.Getenv(envChromium))
	if err != nil {
		return err
	}

	var obs observation
	if actionsJSON != "" {
		// Interactive path: --url is optional (a navigate step may supply the
		// initial URL instead). format still must be json.
		url, format, err := parseInteractiveArgs(rest)
		if err != nil {
			return err
		}
		if format != "json" {
			return fmt.Errorf("unsupported --format %q (only json)", format)
		}
		steps, err := parseSteps(actionsJSON)
		if err != nil {
			return err
		}
		if err := requireDestination(url, steps); err != nil {
			return err
		}
		obs, err = runInteractive(ctx, chromium, url, steps)
		if err != nil {
			return err
		}
	} else {
		// Batch path: behavior UNCHANGED — strict parseArgs requires --url.
		url, format, err := parseArgs(rest)
		if err != nil {
			return err
		}
		if format != "json" {
			return fmt.Errorf("unsupported --format %q (only json)", format)
		}
		obs, err = runBrowser(ctx, chromium, url)
		if err != nil {
			return err
		}
	}

	out, err := encodeObservation(obs)
	if err != nil {
		return fmt.Errorf("encoding observation: %w", err)
	}
	if _, err := stdout.Write(append(out, '\n')); err != nil {
		return fmt.Errorf("writing stdout: %w", err)
	}
	return nil
}

// extractActions pulls a single `--actions <json>` (or `--actions=<json>`) out of
// args, returning the JSON and the remaining args for the existing flag parser.
// Splitting it out keeps parseArgs — and the contract it enforces — untouched on
// the batch path. With --actions absent the rest is args verbatim (behavior
// UNCHANGED). The url may still come from --url; for the interactive path it is
// applied as the first navigation when no explicit navigate step is given.
func extractActions(args []string) (actionsJSON string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	seen := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--actions":
			if seen {
				return "", nil, errors.New("--actions given more than once")
			}
			if i+1 >= len(args) {
				return "", nil, errors.New("--actions requires a value")
			}
			actionsJSON = args[i+1]
			seen = true
			i++
		case strings.HasPrefix(a, "--actions="):
			if seen {
				return "", nil, errors.New("--actions given more than once")
			}
			actionsJSON = strings.TrimPrefix(a, "--actions=")
			seen = true
		default:
			rest = append(rest, a)
		}
	}
	if seen && strings.TrimSpace(actionsJSON) == "" {
		return "", nil, errors.New("--actions value is empty")
	}
	return actionsJSON, rest, nil
}

// runBrowser is a variable so unit tests can substitute a fake; the production
// implementation captures from a real headless Chromium.
var runBrowser = captureWithChromium

// ───────────────────────────── contract ─────────────────────────────

// envChromium overrides the Chromium binary. Default is "chromium".
const envChromium = "NILCORE_CHROMIUM"

// defaultChromium is the binary name searched on $PATH when the env var is unset.
const defaultChromium = "chromium"

// virtualTimeBudget bounds how long Chromium's headless run advances virtual time
// before dumping; it caps a single navigation (matching the tool's bounded-loop
// discipline) without depending on wall-clock network latency.
const virtualTimeBudget = 5 * time.Second

// hardTimeout is the wall-clock ceiling on the whole Chromium process, well above
// virtualTimeBudget, so a wedged browser is killed rather than hanging the tool.
const hardTimeout = 30 * time.Second

// maxText bounds the extracted text we emit. browser_view re-bounds to 16 KiB,
// but trimming here keeps the JSON payload (and the pipe) small.
const maxText = 64 * 1024

// observation is the JSON contract browser_view parses. The field set and tags
// MUST stay byte-compatible with browserObservation in internal/tools/browser.go.
type observation struct {
	Title         string   `json:"title"`
	Text          string   `json:"text"`
	Console       []string `json:"console"`
	ScreenshotB64 string   `json:"screenshot_b64"`
}

// ───────────────────────────── flag parsing ─────────────────────────────

// parseArgs reads the fixed flag set browser_view emits: `--url <u> --format <f>`.
// We hand-parse (rather than the flag package) so the accepted surface is exactly
// the tool's contract and unknown flags are rejected loudly — the driver is
// operator-trusted but defensive.
func parseArgs(args []string) (url, format string, err error) {
	format = "json"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--url":
			if i+1 >= len(args) {
				return "", "", errors.New("--url requires a value")
			}
			url = args[i+1]
			i++
		case strings.HasPrefix(a, "--url="):
			url = strings.TrimPrefix(a, "--url=")
		case a == "--format":
			if i+1 >= len(args) {
				return "", "", errors.New("--format requires a value")
			}
			format = args[i+1]
			i++
		case strings.HasPrefix(a, "--format="):
			format = strings.TrimPrefix(a, "--format=")
		default:
			return "", "", fmt.Errorf("unexpected argument %q", a)
		}
	}
	if strings.TrimSpace(url) == "" {
		return "", "", errors.New("--url is required")
	}
	return url, format, nil
}

// ───────────────────────────── chromium location ─────────────────────────────

// resolveChromium returns the absolute path to the Chromium binary, honoring the
// NILCORE_CHROMIUM override and falling back to "chromium" on $PATH. A missing
// binary is a hard error (fail-closed): the tool must never silently degrade.
func resolveChromium(override string) (string, error) {
	name := strings.TrimSpace(override)
	if name == "" {
		name = defaultChromium
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("chromium binary %q not found on PATH (set %s): %w", name, envChromium, err)
	}
	return path, nil
}

// ───────────────────────────── arg construction (pure) ─────────────────────────────

// chromiumArgs builds the headless Chromium command line. WHY these flags:
//   - --headless=new: the current headless mode (the legacy one is removed in
//     recent Chromium); required for any of the batch flags below.
//   - --no-sandbox / --disable-gpu / --disable-dev-shm-usage: Chromium's own
//     sandbox and GPU are unavailable inside our container (already I4-confined);
//     dev-shm is tiny in containers and would crash the renderer otherwise.
//   - --dump-dom: print the rendered DOM to stdout (our text/title source).
//   - --screenshot=<file>: write a PNG we base64-encode.
//   - --virtual-time-budget: advance time then capture, so JS that paints on load
//     is reflected without an unbounded wait.
//   - --hide-scrollbars / --window-size: deterministic, headless-friendly render.
//
// The URL is passed as the final positional argument (never interpolated into a
// shell string here — exec.Command takes argv directly, so quoting/injection is a
// non-issue at this layer).
func chromiumArgs(screenshotPath, url string) []string {
	return []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--window-size=1280,1024",
		fmt.Sprintf("--virtual-time-budget=%d", virtualTimeBudget.Milliseconds()),
		"--dump-dom",
		"--screenshot=" + screenshotPath,
		url,
	}
}

// ───────────────────────────── browser invocation ─────────────────────────────

// captureWithChromium runs Chromium once, capturing the dumped DOM from stdout,
// base64-encoding the screenshot file, and deriving the title from the DOM. Any
// non-zero exit, missing DOM, or write failure is returned as an error so the
// driver exits non-zero (fail-closed).
func captureWithChromium(ctx context.Context, chromium, url string) (observation, error) {
	dir, err := os.MkdirTemp("", "nilcore-browser-*")
	if err != nil {
		return observation{}, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	shotPath := filepath.Join(dir, "screenshot.png")

	runCtx, cancel := context.WithTimeout(ctx, hardTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, chromium, chromiumArgs(shotPath, url)...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runCtx.Err() != nil {
		return observation{}, fmt.Errorf("chromium timed out after %s loading %s", hardTimeout, url)
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return observation{}, fmt.Errorf("chromium failed loading %s: %s: %w", url, detail, runErr)
		}
		return observation{}, fmt.Errorf("chromium failed loading %s: %w", url, runErr)
	}

	dom := stdout.String()
	if strings.TrimSpace(dom) == "" {
		// An empty DOM with a clean exit means the page never rendered (e.g. the
		// host was unreachable under the egress allowlist). Treat it as a failure.
		return observation{}, fmt.Errorf("chromium produced no DOM for %s (page did not render)", url)
	}

	shot, err := readScreenshot(shotPath)
	if err != nil {
		return observation{}, err
	}

	return buildObservation(dom, shot, stderr.String()), nil
}

// readScreenshot reads the PNG Chromium wrote and base64-encodes it. A missing or
// empty file is not fatal — some pages legitimately yield no useful capture — so
// we return an empty string and let the caller proceed with DOM/text only.
func readScreenshot(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-trusted temp path we created
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading screenshot %s: %w", path, err)
	}
	if len(data) == 0 {
		return "", nil
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ───────────────────────────── DOM → observation (pure) ─────────────────────────────

// buildObservation assembles the contract object from the captured artifacts. It
// is pure (no I/O) so it is unit-testable without a browser.
func buildObservation(dom, screenshotB64, chromiumStderr string) observation {
	return observation{
		Title:         extractTitle(dom),
		Text:          domToText(dom),
		Console:       collectConsole(chromiumStderr),
		ScreenshotB64: screenshotB64,
	}
}

// titleRe matches the contents of the first <title>…</title>, case-insensitively
// and across newlines. WHY regexp not html parser: stdlib has no DOM parser in a
// form lighter than golang.org/x/net/html (a module we will not add for I6); the
// DOM is already rendered, so a single anchored title extraction is sufficient and
// the text path strips the rest of the markup.
var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// extractTitle returns the page title from the rendered DOM, trimmed and with HTML
// entities decoded and inner whitespace collapsed. Empty when absent.
func extractTitle(dom string) string {
	m := titleRe.FindStringSubmatch(dom)
	if m == nil {
		return ""
	}
	return collapseWS(unescapeEntities(m[1]))
}

// scriptStyleRe strips <script>/<style> blocks (their bodies are code, not text)
// before tag removal so we never leak JS/CSS into the text excerpt.
var scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)

// tagRe matches any HTML tag.
var tagRe = regexp.MustCompile(`(?s)<[^>]*>`)

// domToText reduces the rendered DOM to a plain-text excerpt: drop script/style,
// strip tags, decode entities, collapse whitespace, and bound the length. This is
// a best-effort excerpt for the model — not a faithful renderer.
func domToText(dom string) string {
	s := scriptStyleRe.ReplaceAllString(dom, " ")
	// Insert a space where block-ish tags were so words don't run together.
	s = tagRe.ReplaceAllString(s, " ")
	s = unescapeEntities(s)
	s = collapseWS(s)
	if len(s) > maxText {
		s = s[:maxText]
	}
	return s
}

// collectConsole derives best-effort console/diagnostic lines from Chromium's
// stderr. Headless Chromium emits page console output and load diagnostics there
// (e.g. lines containing "console" or DevTools "ERROR:"/"WARNING:" prefixes). We
// keep only those signal lines so the field is useful without dumping all of
// Chromium's startup chatter. The result is UNTRUSTED data (browser_view fences
// it, I7) — we never act on it.
func collectConsole(stderr string) []string {
	if strings.TrimSpace(stderr) == "" {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(stderr, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if isConsoleLine(ln) {
			out = append(out, ln)
		}
	}
	return out
}

// isConsoleLine reports whether a Chromium stderr line is page console / error
// output worth surfacing (as opposed to benign startup noise).
func isConsoleLine(ln string) bool {
	low := strings.ToLower(ln)
	switch {
	case strings.Contains(low, "console"):
		return true
	case strings.Contains(ln, "ERROR:"), strings.Contains(ln, "WARNING:"):
		return true
	default:
		return false
	}
}

// ───────────────────────────── JSON encoding (pure) ─────────────────────────────

// encodeObservation marshals the observation to the exact contract object. We use
// a non-HTML-escaping encoder so the DOM/text excerpt round-trips verbatim (the
// default Marshal escapes <, >, & which would corrupt the excerpt the model reads).
// A nil Console marshals to JSON null, which browser_view treats as no console.
func encodeObservation(obs observation) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obs); err != nil {
		return nil, err
	}
	// Encoder appends a trailing newline; trim it so callers control framing.
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}

// ───────────────────────────── tiny text helpers (pure) ─────────────────────────────

// wsRe matches runs of whitespace (incl. newlines/tabs) for collapsing.
var wsRe = regexp.MustCompile(`\s+`)

// collapseWS trims and collapses internal whitespace runs to single spaces.
func collapseWS(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// entityReplacer decodes the handful of HTML entities common in titles/text. We
// keep this small and explicit rather than pulling html.UnescapeString's full
// table — these cover the cases that matter for a readable excerpt, and avoiding
// the html package keeps the dependency surface minimal. (html is stdlib, but the
// explicit set documents intent and avoids surprising numeric-entity expansion.)
var entityReplacer = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&apos;", "'",
	"&nbsp;", " ",
)

// unescapeEntities decodes the common HTML entities listed above.
func unescapeEntities(s string) string {
	return entityReplacer.Replace(s)
}
