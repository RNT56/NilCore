//go:build !linux

package main

import (
	"nilcore/internal/blastbudget"
	"nilcore/internal/sandbox"
)

// attachNamespaceBlast is a no-op on non-Linux hosts: the *sandbox.Namespace backend
// only compiles under //go:build linux, so there is nothing to wire the wall-time
// blast budget onto here. Keeping the same signature as the Linux build lets
// attachBlast call it unconditionally without a build tag of its own.
func attachNamespaceBlast(sandbox.Sandbox, *blastbudget.Budget) {}
