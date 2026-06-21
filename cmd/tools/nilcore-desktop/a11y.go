package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"nilcore/internal/desktopwire"
)

// a11yNode is the JSON shape nilcore-a11y-dump prints (see images/sandbox-desktop/
// nilcore-a11y-dump). Every field is UNTRUSTED screen data (I7).
type a11yNode struct {
	Role    string   `json:"role"`
	Name    string   `json:"name"`
	Value   string   `json:"value"`
	Box     box      `json:"box"`
	Actions []string `json:"actions"`
}

type box struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// parseA11y turns the dump tool's stdout into numbered refs with TRUE-pixel boxes
// (the boxes are screen-coordinate extents). It is pure — unit-tested against canned
// JSON, no live a11y bus. A node with a zero-area box is dropped (not actionable).
func parseA11y(jsonOut string) ([]desktopwire.Ref, error) {
	var nodes []a11yNode
	if err := json.Unmarshal([]byte(jsonOut), &nodes); err != nil {
		return nil, fmt.Errorf("parsing a11y dump: %w", err)
	}
	refs := make([]desktopwire.Ref, 0, len(nodes))
	id := 1
	for _, n := range nodes {
		if n.Box.W <= 0 || n.Box.H <= 0 {
			continue
		}
		refs = append(refs, desktopwire.Ref{
			ID:      id,
			Role:    n.Role,
			Name:    n.Name,
			Value:   n.Value,
			Box:     desktopwire.Box{X: n.Box.X, Y: n.Box.Y, W: n.Box.W, H: n.Box.H},
			Actions: n.Actions,
		})
		id++
	}
	return refs, nil
}

// dumpA11y is the live seam: shell to the image-baked nilcore-a11y-dump (CI-only).
// A var so unit tests substitute a fake. A non-zero exit / missing tool yields "[]"
// (an empty tree), which the ladder treats as a Rung-1 miss — fail-soft, never a
// fabricated tree.
var dumpA11y = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "nilcore-a11y-dump").Output()
	if err != nil {
		return "[]", nil // empty tree on failure; the ladder falls to a screenshot
	}
	return string(out), nil
}

// activeWindow is the live seam for the focused-window identity (the ladder cache
// key). A var so tests substitute. Best-effort: "" on failure.
var activeWindow = func(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "xdotool", "getactivewindow", "getwindowname").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
