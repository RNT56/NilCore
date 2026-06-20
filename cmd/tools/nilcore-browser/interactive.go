// Interactive (flow) mode for nilcore-browser. WHY this exists alongside the
// batch path: behavioral verification often needs to *drive* a page — click a
// button, type into a field, wait for a render — not just snapshot a URL. The
// batch flags (--dump-dom/--screenshot) are one-shot, so the flow path instead
// launches a long-lived headless Chrome with a debugging port and drives it over
// the Chrome DevTools Protocol via the pure-Go internal/cdp client (I6: no new
// module). The final observation it prints uses the EXACT same JSON contract as
// the batch path, so browser_view (internal/tools/browser.go) parses both
// identically. Like the batch path it is fail-closed: any step or capture error
// returns non-zero so a partial/forged run can never look like a pass.
//
// Everything Chrome returns — titles, text, screenshots, the DevTools endpoint
// JSON — is UNTRUSTED data (I7): we transport and decode it, never let it steer
// control flow.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"nilcore/internal/cdp"
)

// ───────────────────────────── action model ─────────────────────────────

// step is one instruction in the --actions array. Only the fields relevant to
// the action are populated; parseSteps validates the combination.
type step struct {
	Action   string `json:"action"`
	URL      string `json:"url,omitempty"`
	Selector string `json:"selector,omitempty"`
	Text     string `json:"text,omitempty"`
	MS       int    `json:"ms,omitempty"`
}

// Recognized action names. Keeping them explicit (rather than free-form) means an
// unknown action fails loudly instead of being silently skipped.
const (
	actNavigate = "navigate"
	actClick    = "click"
	actType     = "type"
	actWait     = "wait"
)

// maxSteps bounds an actions script so a pathological input cannot drive an
// unbounded loop (the harness's bounded-loop discipline applies here too).
const maxSteps = 256

// maxWaitMS caps a single wait so a script cannot wedge the tool; the overall
// process is also bounded by interactiveHardTimeout.
const maxWaitMS = 30000

// parseSteps decodes and validates the --actions JSON array. It rejects unknown
// actions and missing required fields up front (fail-closed) so a malformed
// script never partially executes against a live browser.
func parseSteps(actionsJSON string) ([]step, error) {
	var steps []step
	dec := json.NewDecoder(strings.NewReader(actionsJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&steps); err != nil {
		return nil, fmt.Errorf("parsing --actions JSON: %w", err)
	}
	if len(steps) == 0 {
		return nil, errors.New("--actions must contain at least one step")
	}
	if len(steps) > maxSteps {
		return nil, fmt.Errorf("--actions has %d steps, exceeds limit %d", len(steps), maxSteps)
	}
	for i, s := range steps {
		switch s.Action {
		case actNavigate:
			if strings.TrimSpace(s.URL) == "" {
				return nil, fmt.Errorf("step %d (navigate) requires a non-empty url", i)
			}
		case actClick:
			if strings.TrimSpace(s.Selector) == "" {
				return nil, fmt.Errorf("step %d (click) requires a selector", i)
			}
		case actType:
			if strings.TrimSpace(s.Selector) == "" {
				return nil, fmt.Errorf("step %d (type) requires a selector", i)
			}
		case actWait:
			if s.MS < 0 || s.MS > maxWaitMS {
				return nil, fmt.Errorf("step %d (wait) ms=%d out of range [0,%d]", i, s.MS, maxWaitMS)
			}
		case "":
			return nil, fmt.Errorf("step %d is missing an action", i)
		default:
			return nil, fmt.Errorf("step %d has unknown action %q", i, s.Action)
		}
	}
	return steps, nil
}

// requireDestination ensures the flow has somewhere to go: either an explicit
// --url (used as the initial navigation) or at least one navigate step. Without
// one, a screenshot would capture about:blank — almost certainly a mistake — so
// we fail closed.
func requireDestination(url string, steps []step) error {
	if strings.TrimSpace(url) != "" {
		return nil
	}
	for _, s := range steps {
		if s.Action == actNavigate {
			return nil
		}
	}
	return errors.New("interactive mode needs a destination: pass --url or include a navigate step")
}

// ───────────────────────────── flag parsing (interactive) ─────────────────────────────

// parseInteractiveArgs parses --url/--format for the flow path. Unlike the batch
// parseArgs, --url is OPTIONAL here (a navigate step may supply it). Unknown
// flags are still rejected loudly.
func parseInteractiveArgs(args []string) (url, format string, err error) {
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
	return url, format, nil
}

// ───────────────────────────── Chrome launch (pure arg builder) ─────────────────────────────

// interactiveChromiumArgs builds the headless Chrome command line for the flow
// path. WHY these differ from the batch flags: we want a *persistent* browser we
// can drive, so we use --remote-debugging-port to expose the DevTools endpoint
// and DROP the one-shot --dump-dom/--screenshot batch flags. --headless=new and
// the container-safety flags (--no-sandbox/--disable-gpu) match the batch path.
// --remote-debugging-address pins the listener to loopback so the debug endpoint
// is never reachable off-host. A throwaway --user-data-dir keeps runs isolated.
func interactiveChromiumArgs(port int, userDataDir string) []string {
	return []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--window-size=1280,1024",
		"--remote-debugging-address=127.0.0.1",
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + userDataDir,
		// A blank starting page; the flow navigates explicitly.
		"about:blank",
	}
}

// interactiveHardTimeout bounds the whole flow run (launch + all steps + final
// capture) so a wedged browser is killed rather than hanging the tool.
const interactiveHardTimeout = 60 * time.Second

// ───────────────────────────── live run (CI-only) ─────────────────────────────

// runInteractive launches Chrome, connects via CDP, executes the steps in order,
// and returns the final observation. It is the live path exercised only by the
// CI browser-e2e job (no real browser in unit tests); the pure pieces it relies
// on (parseSteps, interactiveChromiumArgs, applyStep dispatch) are unit-tested
// hermetically. Any failure returns an error → non-zero exit (fail-closed).
func runInteractive(ctx context.Context, chromium, initialURL string, steps []step) (observation, error) {
	runCtx, cancel := context.WithTimeout(ctx, interactiveHardTimeout)
	defer cancel()

	port, err := freeLoopbackPort()
	if err != nil {
		return observation{}, fmt.Errorf("allocating debug port: %w", err)
	}

	userDir, err := os.MkdirTemp("", "nilcore-browser-cdp-*")
	if err != nil {
		return observation{}, fmt.Errorf("creating user-data dir: %w", err)
	}
	defer os.RemoveAll(userDir)

	cmd := exec.CommandContext(runCtx, chromium, interactiveChromiumArgs(port, userDir)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return observation{}, fmt.Errorf("launching chromium: %w", err)
	}
	// Ensure the browser is reaped on every exit path.
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	wsURL, err := waitForDevToolsWS(runCtx, port)
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return observation{}, fmt.Errorf("%w (chromium stderr: %s)", err, lastLine(detail))
		}
		return observation{}, err
	}

	conn, err := cdp.Dial(runCtx, wsURL)
	if err != nil {
		return observation{}, fmt.Errorf("connecting to devtools: %w", err)
	}
	defer conn.Close()

	if err := conn.Enable(runCtx); err != nil {
		return observation{}, fmt.Errorf("enabling cdp domains: %w", err)
	}

	// An explicit --url is an implicit leading navigation.
	if strings.TrimSpace(initialURL) != "" {
		if err := conn.Navigate(runCtx, initialURL); err != nil {
			return observation{}, err
		}
	}

	for i, s := range steps {
		if err := applyStep(runCtx, conn, s); err != nil {
			return observation{}, fmt.Errorf("step %d (%s): %w", i, s.Action, err)
		}
	}

	return captureObservation(runCtx, conn, stderr.String())
}

// applyStep dispatches a single validated step to the CDP connection. It is split
// from runInteractive so the dispatch table is unit-testable against a fake
// driver without a browser.
func applyStep(ctx context.Context, d stepDriver, s step) error {
	switch s.Action {
	case actNavigate:
		return d.Navigate(ctx, s.URL)
	case actClick:
		return d.ClickSelector(ctx, s.Selector)
	case actType:
		return d.TypeIntoSelector(ctx, s.Selector, s.Text)
	case actWait:
		// A bounded sleep honoring cancellation; lets a just-triggered render or
		// navigation settle before the next step or the final capture.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(s.MS) * time.Millisecond):
			return nil
		}
	default:
		// parseSteps already rejected unknown actions; this is belt-and-suspenders.
		return fmt.Errorf("unknown action %q", s.Action)
	}
}

// stepDriver is the slice of the CDP API applyStep needs. Defining it as an
// interface lets the hermetic test substitute a recording fake for *cdp.Conn.
type stepDriver interface {
	Navigate(ctx context.Context, url string) error
	ClickSelector(ctx context.Context, selector string) error
	TypeIntoSelector(ctx context.Context, selector, text string) error
}

// captureObservation reads the final title, text excerpt, and screenshot via CDP
// and assembles the contract object. The text is bounded and collapsed by the
// same helpers the batch path uses, so both modes emit comparable excerpts.
func captureObservation(ctx context.Context, conn *cdp.Conn, chromiumStderr string) (observation, error) {
	title, err := conn.Title(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("reading title: %w", err)
	}
	text, err := conn.Text(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("reading text: %w", err)
	}
	shot, err := conn.Screenshot(ctx)
	if err != nil {
		return observation{}, fmt.Errorf("capturing screenshot: %w", err)
	}

	bounded := collapseWS(text)
	if len(bounded) > maxText {
		bounded = bounded[:maxText]
	}
	return observation{
		Title:         collapseWS(title),
		Text:          bounded,
		Console:       collectConsole(chromiumStderr),
		ScreenshotB64: shot,
	}, nil
}

// ───────────────────────────── DevTools endpoint discovery ─────────────────────────────

// freeLoopbackPort asks the OS for an unused TCP port by binding :0 and reading
// the assigned port, then releasing it. Chrome then binds the same port for its
// debug endpoint. There is a tiny TOCTOU window, but it is acceptable for an
// in-sandbox, single-tenant tool and avoids hard-coding a port.
func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// devToolsTarget is the subset of a /json/list entry we need: the page's
// WebSocket debugger URL and its type (we want a "page" target).
type devToolsTarget struct {
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// waitForDevToolsWS polls Chrome's HTTP DevTools endpoint until a page target's
// WebSocket URL appears (Chrome writes it once the debug server is listening).
// It bounds the poll by ctx so a browser that never comes up fails closed.
func waitForDevToolsWS(ctx context.Context, port int) (string, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/list", port)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", fmt.Errorf("devtools endpoint never became ready: %w", lastErr)
			}
			return "", fmt.Errorf("devtools endpoint never became ready: %w", ctx.Err())
		case <-ticker.C:
			ws, err := fetchPageWS(ctx, endpoint)
			if err != nil {
				lastErr = err
				continue
			}
			if ws != "" {
				return ws, nil
			}
		}
	}
}

// fetchPageWS does a single GET of /json/list and returns the first page
// target's WebSocket debugger URL, or "" if none is ready yet. The response is
// UNTRUSTED data; we only extract the loopback ws:// URL (dialWebSocket re-checks
// scheme+host).
func fetchPageWS(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("devtools /json/list returned HTTP %d", resp.StatusCode)
	}
	var targets []devToolsTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return "", fmt.Errorf("decoding devtools target list: %w", err)
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, nil
		}
	}
	return "", nil
}

// lastLine returns the last non-empty line of s, used to surface the most
// relevant Chrome stderr line on failure without dumping all startup chatter.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}
