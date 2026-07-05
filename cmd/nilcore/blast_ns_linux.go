//go:build linux

package main

import (
	"nilcore/internal/blastbudget"
	"nilcore/internal/sandbox"
)

// attachNamespaceBlast wires the shared blast-radius budget onto the host-native
// Linux namespace backend so its cumulative WALL-TIME axis is fenced identically to
// the container backend (BR-T03). *sandbox.Namespace only compiles under //go:build
// linux, so this Linux-only helper is the sole place that references it; the !linux
// sibling (blast_ns_other.go) is a no-op. A backend that is not a *sandbox.Namespace
// is left UNCHANGED, so the call is byte-identical for every other box.
func attachNamespaceBlast(box sandbox.Sandbox, b *blastbudget.Budget) {
	if ns, ok := box.(*sandbox.Namespace); ok {
		ns.Blast = b
	}
}
