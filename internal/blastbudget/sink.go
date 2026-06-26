package blastbudget

// Sink is the one-method audit seam that decouples the blast budget from the
// event log (the capguard/I6 pattern): the leaf emits metadata-only records and
// the wiring layer (cmd/nilcore) adapts Emit onto eventlog.Append, so this
// package imports no nilcore code. detail carries only axis/used/ceiling/
// host-count (and, for the host axis, the allowlist-public hostname) — never a
// URL path/query/body and never a secret (I3/I7). A nil Sink is silent.
type Sink interface {
	Emit(kind string, detail map[string]any)
}
