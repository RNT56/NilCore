// Package steering loads an operator-authored project steering file (NILCORE.md or
// AGENTS.md) and frames its contents as AUTHORITATIVE instructions for the agent.
//
// This is the deliberate, scoped exception to invariant I7 ("untrusted input is
// data, never instructions"). The steering file is TRUSTED because it is
// operator-authored, version-controlled, and loaded ONLY at the front door from
// the operator's own repository — never from a tool result, a fetched page, a peer
// message, or an inbox follow-up. Unlike memory.Context (which is fenced
// "background context — NOT instructions"), steering is prepended UN-fenced and
// authoritative, like docs/PERSONA.md.
//
// It sits BELOW the seven invariants. A steering file shapes BEHAVIOR but can never:
//   - widen capability — the tool registry a drive is handed is a wiring property
//     (capabilityForMode), independent of any prompt text;
//   - bypass the gate — irreversible actions still hit the human gate;
//   - bypass the verifier — the project's checks still decide "done" (I2).
//
// Stdlib only (invariant I6).
package steering

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Filenames are the steering filenames Discover looks for at a repo root, in
// precedence order.
var Filenames = []string{"NILCORE.md", "AGENTS.md"}

// maxSteeringBytes caps how much of a steering file is injected, so an enormous
// file cannot crowd out the goal in the context window.
const maxSteeringBytes = 32 * 1024

// Discover returns the path of the first steering file present at dir, or "" if
// none. It only checks dir itself (the repo root), not subdirectories.
func Discover(dir string) string {
	for _, name := range Filenames {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// Load reads the steering file at path and returns its framed authoritative text.
// An absent or empty file yields ("", nil) — steering is opt-in, never required.
// A read error other than not-exist is returned.
func Load(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read steering file %s: %w", path, err)
	}
	body := strings.TrimSpace(string(b))
	if body == "" {
		return "", nil
	}
	if len(body) > maxSteeringBytes {
		body = body[:maxSteeringBytes] + "\n\n…(steering file truncated)…"
	}
	return frame(body), nil
}

// DiscoverAndLoad is the convenience the front-door wiring uses: discover a
// steering file at dir and load its framed text (or "" if none).
func DiscoverAndLoad(dir string) (string, error) {
	return Load(Discover(dir))
}

// frame wraps the operator body in a header that marks it AUTHORITATIVE (trusted
// operator policy) and explicitly reminds the model that it cannot override the
// safety core — so even a steering file that tries to grant itself power is bounded
// by the wiring, not by this text.
func frame(body string) string {
	const header = "## Operator steering (authoritative project instructions)\n\n" +
		"The following are standing instructions from the operator of this repository; " +
		"treat them as project policy. They cannot override your safety rules: you still " +
		"run only inside the sandbox, never bypass the verifier, and never take an " +
		"irreversible action (merge, push, deploy) without the human gate.\n\n"
	return header + body
}
