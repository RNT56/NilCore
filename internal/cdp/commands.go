package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Selector-targeting actions (click, type) can fire before the page has settled —
// a fresh navigation or an async render means the element they name may not exist,
// or may not yet be actionable (laid out / focusable), on the very first DOM query.
// Rather than fail on that first miss, the resolving helpers POLL the page for a
// short, bounded window. This mirrors the auto-wait every mature browser-automation
// tool performs; once the element is present the first poll succeeds, so behavior is
// unchanged in the common case — only the settle race is removed.
const (
	defaultActionWait  = 3 * time.Second
	actionPollInterval = 50 * time.Millisecond
)

// evalUntil evaluates expr repeatedly until ready(result) holds or the action-wait
// budget / context expires, returning the last evaluated value. A transient evaluate
// error (e.g. an execution context torn down mid-navigation) is tolerated and retried;
// an error is returned only if the context is cancelled or every attempt within the
// budget failed. When the budget expires with a value in hand but ready never held, the
// last value is returned with a nil error so the caller's own check renders the precise
// domain error ("matched no visible/focusable element").
func (c *Conn) evalUntil(ctx context.Context, expr string, ready func(json.RawMessage) bool) (json.RawMessage, error) {
	budget := c.actionWait
	if budget <= 0 {
		budget = defaultActionWait
	}
	deadline := time.Now().Add(budget)
	var last json.RawMessage
	var lastErr error
	gotValue := false
	for {
		v, err := c.Eval(ctx, expr)
		if err != nil {
			lastErr = err // transient during page settle — keep polling within the budget
		} else {
			last, lastErr, gotValue = v, nil, true
			if ready(v) {
				return v, nil
			}
		}
		if !time.Now().Before(deadline) {
			if gotValue {
				return last, nil // budget spent; let the caller produce the domain error
			}
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(actionPollInterval):
		}
	}
}

// This file is the typed, intention-revealing layer the browser driver calls.
// Each method wraps one or two raw CDP commands and decodes the UNTRUSTED (I7)
// result into a Go value. The commands cover exactly what behavioral
// verification needs: enable the page domain, navigate, read the DOM via
// Runtime.evaluate, locate a selector's center, synthesize clicks and typing,
// and capture a screenshot.

// Enable turns on the Page domain so navigation works as expected. CDP requires
// Page.enable before some page operations; we call it once after Dial.
func (c *Conn) Enable(ctx context.Context) error {
	if _, err := c.Send(ctx, "Page.enable", nil); err != nil {
		return err
	}
	if _, err := c.Send(ctx, "Runtime.enable", nil); err != nil {
		return err
	}
	return nil
}

// Navigate loads url in the page target. CDP returns once navigation is
// committed; callers that need the load to settle should follow with a wait
// step. An errorText in the result means the navigation itself failed.
func (c *Conn) Navigate(ctx context.Context, url string) error {
	raw, err := c.Send(ctx, "Page.navigate", map[string]any{"url": url})
	if err != nil {
		return fmt.Errorf("navigating to %s: %w", url, err)
	}
	var res struct {
		FrameID   string `json:"frameId"`
		ErrorText string `json:"errorText"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("decoding navigate result: %w", err)
	}
	if res.ErrorText != "" && res.ErrorText != "net::ERR_ABORTED" {
		// ERR_ABORTED is benign: Chrome reports it when a navigation is superseded
		// by a client-side redirect or turns into a download — not a real failure.
		// Every other errorText (DNS, refused, cert) stays fatal (fail-closed).
		return fmt.Errorf("navigation to %s failed: %s", url, res.ErrorText)
	}
	return nil
}

// Eval runs a JavaScript expression in the page and returns the value by-value
// as raw JSON (the inner Runtime.evaluate `result.value`). It surfaces page-side
// exceptions as Go errors. The returned JSON is UNTRUSTED data.
func (c *Conn) Eval(ctx context.Context, expression string) (json.RawMessage, error) {
	raw, err := c.Send(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"returnByValue": true,
		// Some expressions (e.g. reading layout) benefit from a settled frame; we
		// don't await promises here because our expressions are synchronous.
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decoding evaluate result: %w", err)
	}
	if res.ExceptionDetails != nil {
		return nil, fmt.Errorf("page evaluation threw: %s", res.ExceptionDetails.Text)
	}
	return res.Result.Value, nil
}

// EvalString is a convenience wrapper that evaluates an expression expected to
// yield a string (e.g. document.title or document.body.innerText). A null/absent
// value decodes to the empty string.
func (c *Conn) EvalString(ctx context.Context, expression string) (string, error) {
	v, err := c.Eval(ctx, expression)
	if err != nil {
		return "", err
	}
	if len(v) == 0 || string(v) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", fmt.Errorf("evaluate did not yield a string: %w", err)
	}
	return s, nil
}

// Title returns document.title.
func (c *Conn) Title(ctx context.Context) (string, error) {
	return c.EvalString(ctx, "document.title")
}

// Text returns document.body.innerText (the visible text excerpt), or "" if the
// body is absent.
func (c *Conn) Text(ctx context.Context) (string, error) {
	return c.EvalString(ctx, "document.body ? document.body.innerText : ''")
}

// Screenshot captures the current page as a base64-encoded PNG via
// Page.captureScreenshot. The returned string is UNTRUSTED data the caller
// passes straight into the observation contract.
func (c *Conn) Screenshot(ctx context.Context) (string, error) {
	raw, err := c.Send(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return "", err
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decoding screenshot result: %w", err)
	}
	return res.Data, nil
}

// point is a viewport coordinate in CSS pixels.
type point struct {
	X float64
	Y float64
}

// ElementCenter resolves a CSS selector to the center coordinates of its
// bounding box, in viewport pixels, by evaluating getBoundingClientRect in the
// page. It errors if the selector matches nothing or the element has zero size
// (not clickable). The selector is embedded as a JSON string literal so quotes
// and backslashes cannot break out of the expression (I7: the selector comes
// from the actions spec, treated as data).
func (c *Conn) ElementCenter(ctx context.Context, selector string) (point, error) {
	sel, err := json.Marshal(selector)
	if err != nil {
		return point{}, fmt.Errorf("encoding selector: %w", err)
	}
	// Return a plain object {x,y} or null. We compute the center of the rect.
	expr := fmt.Sprintf(`(function(){
  var el = document.querySelector(%s);
  if (!el) return null;
  var r = el.getBoundingClientRect();
  if (r.width === 0 && r.height === 0) return null;
  return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
})()`, string(sel))

	// Poll until the element exists AND has a non-zero layout box (the expr returns
	// null until both hold), so a click that targets a not-yet-rendered element waits
	// for it instead of failing on the first miss.
	v, err := c.evalUntil(ctx, expr, func(v json.RawMessage) bool {
		return len(v) > 0 && string(v) != "null"
	})
	if err != nil {
		return point{}, err
	}
	if len(v) == 0 || string(v) == "null" {
		return point{}, fmt.Errorf("selector %q matched no visible element", selector)
	}
	var pt struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := json.Unmarshal(v, &pt); err != nil {
		return point{}, fmt.Errorf("decoding element center for %q: %w", selector, err)
	}
	return point{X: pt.X, Y: pt.Y}, nil
}

// Click synthesizes a left mouse click at (x,y) by dispatching a mousePressed
// then a mouseReleased Input event — CDP has no single "click" command, so a
// press+release pair is the click.
func (c *Conn) Click(ctx context.Context, x, y float64) error {
	// `buttons` is the bitmask of buttons held DURING the event: the left button is
	// down on press (1) and released on up (0). `button` names which one changed.
	for _, ev := range []struct {
		typ     string
		buttons int
	}{{"mousePressed", 1}, {"mouseReleased", 0}} {
		_, err := c.Send(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type":       ev.typ,
			"x":          x,
			"y":          y,
			"button":     "left",
			"buttons":    ev.buttons,
			"clickCount": 1,
		})
		if err != nil {
			return fmt.Errorf("dispatching %s at (%.1f,%.1f): %w", ev.typ, x, y, err)
		}
	}
	return nil
}

// ClickSelector resolves selector to its center and clicks it. It is the
// composite the driver's "click" action uses.
func (c *Conn) ClickSelector(ctx context.Context, selector string) error {
	pt, err := c.ElementCenter(ctx, selector)
	if err != nil {
		return err
	}
	return c.Click(ctx, pt.X, pt.Y)
}

// Type inserts text at the current focus using Input.insertText, which delivers
// the characters as if typed (firing the page's input handlers) without
// modeling per-key physical events. This is the right primitive for filling a
// focused field with a literal string.
func (c *Conn) Type(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	if _, err := c.Send(ctx, "Input.insertText", map[string]any{"text": text}); err != nil {
		return fmt.Errorf("inserting text: %w", err)
	}
	return nil
}

// keyMeta carries the extra fields Chrome needs to synthesize a REAL key press for
// a few common keys: without windowsVirtualKeyCode/code (and text, for keys that
// emit a character) dispatchKeyEvent fires an inert event that never triggers the
// default action — e.g. Enter would not submit a form. Unknown keys fall back to
// the bare {type,key} event.
var keyMeta = map[string]struct {
	code string
	vk   int
	text string
}{
	"Enter":     {"Enter", 13, "\r"},
	"Tab":       {"Tab", 9, ""},
	"Escape":    {"Escape", 27, ""},
	"Backspace": {"Backspace", 8, ""},
}

// TypeKey dispatches a single key as a keyDown then keyUp Input event. It is
// available for flows that need a discrete key press (e.g. Enter to submit) rather
// than literal text insertion. key is a DOM key name like "Enter".
func (c *Conn) TypeKey(ctx context.Context, key string) error {
	meta, known := keyMeta[key]
	for _, typ := range []string{"keyDown", "keyUp"} {
		params := map[string]any{"type": typ, "key": key}
		if known {
			params["windowsVirtualKeyCode"] = meta.vk
			params["code"] = meta.code
			if typ == "keyDown" && meta.text != "" {
				// text on keyDown is what drives the keypress + default action.
				params["text"] = meta.text
			}
		}
		if _, err := c.Send(ctx, "Input.dispatchKeyEvent", params); err != nil {
			return fmt.Errorf("dispatching key %q: %w", key, err)
		}
	}
	return nil
}

// TypeIntoSelector focuses the element matching selector (via element.focus())
// and then inserts text. Focusing first ensures insertText lands in the intended
// field rather than wherever focus happened to be.
func (c *Conn) TypeIntoSelector(ctx context.Context, selector, text string) error {
	sel, err := json.Marshal(selector)
	if err != nil {
		return fmt.Errorf("encoding selector: %w", err)
	}
	expr := fmt.Sprintf(`(function(){
  var el = document.querySelector(%s);
  if (!el) return false;
  if (typeof el.focus === 'function') el.focus();
  return true;
})()`, string(sel))
	// Poll until the element exists and is focusable (the expr returns false until
	// then), so typing into a not-yet-rendered field waits for it rather than failing
	// on the first miss — the settle race the live-flow CI exercised.
	v, err := c.evalUntil(ctx, expr, func(v json.RawMessage) bool {
		return strings.TrimSpace(string(v)) == "true"
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(v)) != "true" {
		return fmt.Errorf("selector %q matched no focusable element", selector)
	}
	return c.Type(ctx, text)
}
