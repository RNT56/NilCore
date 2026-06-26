package session

// asklevel.go is the conversational "ask budget" scale: a 1..6 ordinal the operator
// dials by talking to the agent ("ask me fewer/more questions" → the model calls
// set_ask_level) or with the /questions control verb. The level maps to a per-DRIVE
// ask_user ceiling; it is sticky across drives and persisted (like Mode), so the
// posture survives a restart. The default is "normal"; the zero value (an unset or
// pre-feature snapshot) normalizes to it, so a fresh conversation is never silently
// "off".

import (
	"fmt"
	"strconv"
	"strings"
)

// The ask-level scale. Off means never ask (the tool refuses, the model proceeds on
// assumptions); each higher rung raises the per-drive ask budget.
const (
	askLevelOff     = 1
	askLevelMinimal = 2
	askLevelLow     = 3
	askLevelNormal  = 4 // the default
	askLevelHigh    = 5
	askLevelMax     = 6
)

// askBudgetFor maps a (normalized) level to the per-drive ask_user ceiling.
func askBudgetFor(level int) int {
	switch normalizeAskLevel(level) {
	case askLevelOff:
		return 0
	case askLevelMinimal:
		return 1
	case askLevelLow:
		return 2
	case askLevelNormal:
		return 3
	case askLevelHigh:
		return 4
	default: // askLevelMax
		return 6
	}
}

// normalizeAskLevel clamps a stored level into [off, max]; the zero value (unset /
// pre-feature snapshot) maps to the default so a conversation is never silently off.
func normalizeAskLevel(level int) int {
	switch {
	case level == 0:
		return askLevelNormal
	case level < askLevelOff:
		return askLevelOff
	case level > askLevelMax:
		return askLevelMax
	default:
		return level
	}
}

// askLevelName renders a level for acks, /status and the audit event.
func askLevelName(level int) string {
	switch normalizeAskLevel(level) {
	case askLevelOff:
		return "off"
	case askLevelMinimal:
		return "minimal"
	case askLevelLow:
		return "low"
	case askLevelNormal:
		return "normal"
	case askLevelHigh:
		return "high"
	default:
		return "max"
	}
}

// applyAskSpec moves a current level by one notch ("less"/"more"), to a named or
// numeric level, or off. An empty spec is a no-op (show the current level). It
// returns the new level and an error for an unrecognized spec.
func applyAskSpec(cur int, spec string) (int, error) {
	cur = normalizeAskLevel(cur)
	s := strings.ToLower(strings.TrimSpace(spec))
	switch s {
	case "":
		return cur, nil // show, no change
	case "less", "fewer", "down", "-":
		return normalizeAskLevel(cur - 1), nil
	case "more", "up", "+":
		return normalizeAskLevel(cur + 1), nil
	case "off", "none", "never", "0":
		return askLevelOff, nil
	case "minimal", "min":
		return askLevelMinimal, nil
	case "low":
		return askLevelLow, nil
	case "normal", "default", "medium":
		return askLevelNormal, nil
	case "high":
		return askLevelHigh, nil
	case "max", "always":
		return askLevelMax, nil
	}
	// A bare number on the user-facing 0..5 scale (0=off .. 5=max).
	if n, err := strconv.Atoi(s); err == nil {
		return normalizeAskLevel(n + askLevelOff), nil
	}
	return cur, fmt.Errorf("unknown ask level %q — use less, more, off, normal, high, or max", spec)
}

// askLevelAck is the one-line confirmation shown to the operator and returned to the
// model after a level change.
func askLevelAck(level int) string {
	if b := askBudgetFor(level); b == 0 {
		return "ask level: off — I won't ask questions; I'll act on my best assumptions"
	} else {
		return fmt.Sprintf("ask level: %s — up to %d question(s) per task", askLevelName(level), b)
	}
}
