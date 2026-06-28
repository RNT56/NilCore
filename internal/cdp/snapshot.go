package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// sleepCtx sleeps for ms milliseconds honoring cancellation, so a settle/wait step
// never outlives its context.
func sleepCtx(ctx context.Context, ms int) error {
	if ms <= 0 {
		return nil
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// This file adds the Phase-14 perception + navigation primitives on top of the
// D1/R3 command set in commands.go: an accessibility "set-of-marks" snapshot
// (the cheap, deterministic perception channel that replaces raw screenshots for
// most tasks), ref-based actuation that reuses the existing selector primitives,
// scroll/history/select, and a DOM-stability wait. Everything Chrome returns is
// UNTRUSTED data (I7); we decode it into typed Go values and never let it steer
// control flow.

// Element is one interactive element in a set-of-marks snapshot: a stable ref id,
// its accessibility role and name, the current value (for inputs), and the center
// of its bounding box in viewport pixels. Role/Name/Value are page-controlled and
// therefore UNTRUSTED.
type Element struct {
	Ref   int     `json:"ref"`
	Role  string  `json:"role"`
	Name  string  `json:"name"`
	Value string  `json:"value"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// maxSnapshotElements bounds the set-of-marks so a pathological page cannot blow
// the model's context. The cap is reported by the driver (silent truncation reads
// as "saw everything"); 200 interactive elements is far past any sane page.
const maxSnapshotElements = 200

// snapshotJS walks the DOM for interactive elements, (re-)stamps each with a
// data-nilref attribute (so a later ref-based click/type resolves it via the
// existing querySelector primitives), and returns a compact array. It first clears
// every prior data-nilref so a fresh snapshot never collides with a stale stamp —
// this is the staleness defense: a ref from an old snapshot will not match after a
// re-render, so ElementCenter fails closed rather than acting on the wrong node.
//
// It is a single Runtime.evaluate (one round-trip, deterministic) — the SoM-lite
// pattern the field uses (browser-use/Stagehand) — not the multi-call
// Accessibility.getFullAXTree protocol, which is heavier for no gain here.
var snapshotJS = `(function(){
  document.querySelectorAll('[data-nilref]').forEach(function(e){e.removeAttribute('data-nilref');});
  var sel = 'a[href], button, input:not([type=hidden]), select, textarea, ' +
            '[role=button], [role=link], [role=checkbox], [role=tab], [role=menuitem], ' +
            '[contenteditable=true], [onclick], summary, [tabindex]:not([tabindex="-1"])';
  var els = Array.prototype.slice.call(document.querySelectorAll(sel));
  var out = [];
  var n = 0;
  for (var i = 0; i < els.length && n < ` + strconv.Itoa(maxSnapshotElements) + `; i++) {
    var el = els[i];
    var r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) continue;                 // not rendered
    var style = window.getComputedStyle(el);
    if (style && (style.visibility === 'hidden' || style.display === 'none')) continue;
    el.setAttribute('data-nilref', String(n));
    var role = el.getAttribute('role') || el.tagName.toLowerCase();
    // Never surface the live value of a secret-bearing field back to the model (I3):
    // a host-side {{secret:NAME}} substitution that the model typed must not reflow
    // out via the next snapshot's .value. A password input (or one masking its own
    // input) is treated as secret: its value is replaced by a length-only sentinel,
    // and el.value is dropped from the name fallback below.
    var isSecret = (el.tagName.toLowerCase() === 'input') &&
                   (el.type === 'password' ||
                    (el.getAttribute('autocomplete') || '').indexOf('current-password') >= 0 ||
                    (el.getAttribute('autocomplete') || '').indexOf('new-password') >= 0);
    var nameVal = isSecret ? '' : (el.value || '');
    var name = (el.getAttribute('aria-label') || el.getAttribute('placeholder') ||
                (el.innerText || el.textContent || '').trim() || nameVal || el.title ||
                el.getAttribute('alt') || el.name || '').replace(/\s+/g, ' ').slice(0, 200);
    var val = isSecret
      ? (el.value ? '••• (' + el.value.length + ' chars, hidden)' : '')
      : (el.value || '').slice(0, 200);
    out.push({ ref: n, role: role, name: name, value: val,
               x: r.left + r.width / 2, y: r.top + r.height / 2 });
    n++;
  }
  return out;
})()`

// InteractiveSnapshot returns the set-of-marks: the page's interactive elements,
// each stamped with a data-nilref the ref-based actions resolve against. The slice
// may be empty (a page with no interactive elements, or a canvas/WebGL app) — the
// caller should fall back to a screenshot in that case.
func (c *Conn) InteractiveSnapshot(ctx context.Context) ([]Element, error) {
	v, err := c.Eval(ctx, snapshotJS)
	if err != nil {
		return nil, fmt.Errorf("collecting interactive snapshot: %w", err)
	}
	if len(v) == 0 || string(v) == "null" {
		return nil, nil
	}
	var els []Element
	if err := json.Unmarshal(v, &els); err != nil {
		return nil, fmt.Errorf("decoding interactive snapshot: %w", err)
	}
	return els, nil
}

// refSelector builds the querySelector for a snapshot ref. The integer is rendered
// directly (no model-controlled string), so there is no injection surface here.
func refSelector(ref int) string {
	return `[data-nilref="` + strconv.Itoa(ref) + `"]`
}

// ClickRef clicks the element carrying data-nilref=ref from the latest snapshot.
// A ref that no longer matches (the page re-rendered) fails closed via
// ElementCenter's "matched no visible element".
func (c *Conn) ClickRef(ctx context.Context, ref int) error {
	return c.ClickSelector(ctx, refSelector(ref))
}

// TypeRef focuses the element carrying data-nilref=ref and types text into it.
func (c *Conn) TypeRef(ctx context.Context, ref int, text string) error {
	return c.TypeIntoSelector(ctx, refSelector(ref), text)
}

// SelectRef sets a <select> (data-nilref=ref) to the option whose value or visible
// text equals want, then fires a change event so the page's handlers run. Returns
// an error if the ref matched nothing or no option matched (fail closed).
func (c *Conn) SelectRef(ctx context.Context, ref int, want string) error {
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("encoding option: %w", err)
	}
	selJSON, _ := json.Marshal(refSelector(ref))
	expr := fmt.Sprintf(`(function(){
  var el = document.querySelector(%s);
  if (!el || el.tagName.toLowerCase() !== 'select') return 'no-select';
  var want = %s;
  for (var i = 0; i < el.options.length; i++) {
    var o = el.options[i];
    if (o.value === want || (o.text || '').trim() === want) {
      el.selectedIndex = i;
      el.dispatchEvent(new Event('input', {bubbles:true}));
      el.dispatchEvent(new Event('change', {bubbles:true}));
      return 'ok';
    }
  }
  return 'no-option';
})()`, string(selJSON), string(wantJSON))
	v, err := c.Eval(ctx, expr)
	if err != nil {
		return err
	}
	switch s := string(v); s {
	case `"ok"`:
		return nil
	case `"no-select"`:
		return fmt.Errorf("ref %d is not a <select>", ref)
	default:
		return fmt.Errorf("no option matching %q in select ref %d", want, ref)
	}
}

// Scroll scrolls the window by (dx,dy) CSS pixels via window.scrollBy. JS scrolling
// is deterministic and needs no Input plumbing; a negative dy scrolls up.
func (c *Conn) Scroll(ctx context.Context, dx, dy int) error {
	_, err := c.Eval(ctx, fmt.Sprintf("window.scrollBy(%d,%d)", dx, dy))
	return err
}

// Back navigates back in history.
func (c *Conn) Back(ctx context.Context) error {
	_, err := c.Eval(ctx, "history.back()")
	return err
}

// Forward navigates forward in history.
func (c *Conn) Forward(ctx context.Context) error {
	_, err := c.Eval(ctx, "history.forward()")
	return err
}

// CurrentURL returns document.location.href.
func (c *Conn) CurrentURL(ctx context.Context) (string, error) {
	return c.EvalString(ctx, "document.location.href")
}

// WaitReady polls until the document has finished loading AND the DOM has been
// stable (body HTML length unchanged) across two consecutive polls, or ctx fires.
// This is the deterministic DOM-stability wait that replaces an arbitrary sleep —
// the production reliability primitive (never act on a page that is still
// changing). polls is the number of stability checks; gapMS the spacing.
func (c *Conn) WaitReady(ctx context.Context, polls, gapMS int) error {
	if polls < 2 {
		polls = 2
	}
	var lastLen json.RawMessage
	stable := 0
	for stable < polls {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := c.Eval(ctx, "({rs: document.readyState, len: (document.body? document.body.innerHTML.length : 0)})")
		if err != nil {
			return err
		}
		var state struct {
			RS  string `json:"rs"`
			Len int    `json:"len"`
		}
		if err := json.Unmarshal(v, &state); err != nil {
			return fmt.Errorf("decoding ready state: %w", err)
		}
		cur, _ := json.Marshal(state.Len)
		if state.RS == "complete" && string(cur) == string(lastLen) {
			stable++
		} else {
			stable = 0
		}
		lastLen = cur
		if stable >= polls {
			return nil
		}
		if err := sleepCtx(ctx, gapMS); err != nil {
			return err
		}
	}
	return nil
}
