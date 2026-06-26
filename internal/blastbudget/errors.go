package blastbudget

import (
	"errors"
	"fmt"
)

// ErrBlast is the base sentinel every blast-radius breach wraps, so a caller can
// test broadly (errors.Is(err, ErrBlast) — "any capability ceiling tripped") or
// narrowly against one of the axis sentinels below. A breach records nothing;
// like budget.ErrCeiling it is a refusal, not an after-the-fact report.
var ErrBlast = errors.New("blastbudget: capability budget exhausted")

// Axis sentinels. Each wraps ErrBlast, so errors.Is matches both the specific
// axis and the base.
var (
	ErrHostCeiling  = fmt.Errorf("%w: distinct egress hosts", ErrBlast)
	ErrIrrevCeiling = fmt.Errorf("%w: irreversible actions", ErrBlast)
	ErrWallCeiling  = fmt.Errorf("%w: sandbox wall-time", ErrBlast)
	ErrDayCeiling   = fmt.Errorf("%w: per-day auto-approval dollars", ErrBlast)
)
