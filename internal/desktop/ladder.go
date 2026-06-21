package desktop

import (
	"fmt"

	"nilcore/internal/desktopwire"
)

// Ladder decides which perception rung to use for the NEXT observation, per-step,
// against the currently-focused window (Phase CU, CU-T08). It implements the §0a
// trigger logic: Rung 1 (AT-SPI refs) when the tree is plausible; drop to Rung 2
// (SoM-annotated screenshot) when the tree is empty/sparse OR a ref-click verifiably
// did nothing (stagnation); drop to Rung 3 (raw coordinate) when no markable boxes
// exist. The choice is cached per-window and invalidated on focus change, resize, or
// stagnation — so a stable dialog isn't re-probed every action, but a mixed task
// (rich GTK dialog + empty Electron pane + canvas) isn't latched to a stale rung.
//
// Pure logic — the driver feeds it counts/flags, it returns a rung + reason. No I/O.
type Ladder struct {
	st  map[string]*winState
	cur string
}

type winState struct {
	downgraded bool // a verified no-op on this window's tree ⇒ stop trusting Rung 1
}

// RungInput is what the driver knows at decision time about the focused window.
type RungInput struct {
	Window           string // focused-window identity (for the cache); "" is allowed
	RefCount         int    // AT-SPI interactive refs found this observation
	Stagnant         bool   // the last ref-based act verifiably changed nothing
	HasMarkableBoxes bool   // AT-SPI extents and/or CV proposals exist for a SoM overlay
}

// RungDecision is the chosen rung + a human/audit reason.
type RungDecision struct {
	Rung   int
	Reason string
}

// NewLadder returns an empty ladder.
func NewLadder() *Ladder { return &Ladder{st: map[string]*winState{}} }

// Decide picks the rung for this observation, updating the per-window cache.
func (l *Ladder) Decide(in RungInput) RungDecision {
	// Focus change invalidates the cache: a (re)focused window gets a fresh probe.
	if in.Window != l.cur {
		l.cur = in.Window
		l.st[in.Window] = &winState{}
	}
	ws := l.st[in.Window]
	if ws == nil {
		ws = &winState{}
		l.st[in.Window] = ws
	}
	// A verified stagnation on this window means its tree is empty/sparse/lying — stop
	// trusting Rung 1 for it until focus changes (the secondary 1→2 drop trigger).
	if in.Stagnant {
		ws.downgraded = true
	}

	switch {
	case in.RefCount > 0 && !ws.downgraded:
		return RungDecision{Rung: desktopwire.RungATSPI, Reason: fmt.Sprintf("%d accessibility refs", in.RefCount)}
	case in.HasMarkableBoxes:
		reason := "no usable refs — marked screenshot"
		if ws.downgraded {
			reason = "ref-clicks stagnated — falling back to marked screenshot"
		}
		return RungDecision{Rung: desktopwire.RungSoM, Reason: reason}
	default:
		return RungDecision{Rung: desktopwire.RungCoordinate, Reason: "no refs and no markable boxes (canvas/immediate-mode)"}
	}
}

// Invalidate drops the cached state for a window (e.g. on a resize), forcing a fresh
// probe on its next observation.
func (l *Ladder) Invalidate(window string) {
	delete(l.st, window)
	if l.cur == window {
		l.cur = ""
	}
}
