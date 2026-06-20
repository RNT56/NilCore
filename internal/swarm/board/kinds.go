package board

// kinds.go is the single source of truth for the swarm event Kinds the scoreboard
// emits into the append-only log. They are FREE STRINGS (no schema change, I5/I6):
// the eventlog.Event.Kind is an open keyspace, so a new swarm dimension rides on new
// Kind strings rather than a struct change. Every Detail these events carry is
// METADATA ONLY (counts, ids, a pass number) — never a model-authored Value/SourceURL
// (I7) and never a secret (I3).
//
// CONTRACT WITH internal/report. report.ReplaySwarmReport (SW-T06) folds the swarm
// dimension by decoding two of these Kinds off the wire — it deliberately does NOT
// import this package (it stays a leaf that never reaches the swarm side), so the
// STRINGS are the contract. ScoreboardSnapshotKind and SwarmPassCleanKind below MUST
// stay byte-identical to report's scoreboardSnapshotKind / swarmPassCleanKind, and the
// snapshot Detail keys MUST match report.passRowFromEvent. The keystone live==replay
// test (TestLiveVsReplayAgree) is what guards this agreement: drive a Board, append
// the matching events, and assert Board.Snapshot() == ReplaySwarmReport(...).Swarm.
const (
	// SwarmStartKind marks the start of a whole swarm run. It anchors the total
	// wall-clock timer; its Detail is metadata only (the planned shard total).
	SwarmStartKind = "swarm_start"

	// ShardEnqueuedKind records that a shard was placed on the worklist (MarkQueued).
	ShardEnqueuedKind = "shard_enqueued"

	// ShardDispatchedKind records that a shard began running (MarkRunning) — the
	// per-shard start stamp that gives per-shard wall-clock a real source.
	ShardDispatchedKind = "shard_dispatched"

	// ShardVerifiedKind records one shard's verifier verdict (Record). It is the
	// per-shard pass/fail signal; the verdict in its Detail is the verifier's, never
	// a backend self-report (I2).
	ShardVerifiedKind = "shard_verified"

	// ShardRequeuedKind records that a failed shard was put back on the worklist for
	// another pass. It is bookkeeping for the requeue history; it never moves a count.
	ShardRequeuedKind = "shard_requeued"

	// ShardExhaustedKind records that a shard ran out of requeue attempts and stays
	// red. It is the terminal-red signal for one shard.
	ShardExhaustedKind = "shard_exhausted"

	// ScoreboardSnapshotKind carries one pass's whole Scoreboard tally
	// ({pass, checked, passed, failed, retry_pass, remaining}). report folds the LAST
	// one into the final SwarmDimension counts, and each into a PassRow — so the
	// Detail keys here are a hard contract with report.passRowFromEvent.
	ScoreboardSnapshotKind = "scoreboard_snapshot"

	// SwarmPassCleanKind is emitted EXACTLY when a pass converged with an empty
	// worklist on a verified chain (MarkClean). Its PRESENCE is the second leg of
	// report's FinalCleanPass gate; its Detail is metadata only.
	SwarmPassCleanKind = "swarm_pass_clean"

	// SwarmDoneKind marks the end of a whole swarm run. It closes the total
	// wall-clock timer; its Detail is metadata only.
	SwarmDoneKind = "swarm_done"
)

// Detail keys for the scoreboard_snapshot event. These MUST match the keys
// report.passRowFromEvent reads, or the keystone live==replay test fails. They are
// the only place a count crosses from the Board into the log, so they are named once
// here and reused by EmitSnapshot.
const (
	detailPass      = "pass"
	detailChecked   = "checked"
	detailPassed    = "passed"
	detailFailed    = "failed"
	detailRetryPass = "retry_pass"
	detailRemaining = "remaining"
)
