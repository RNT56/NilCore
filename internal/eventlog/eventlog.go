// Package eventlog is an append-only audit trail (invariant I5). It writes JSON
// Lines to a file (zero dependencies); every model call, tool execution, verify,
// and gate decision is recorded and replayable. Each event carries a monotonic
// sequence number and is hash-chained to the previous one, so tampering,
// reordering, or a dropped event is detectable (P2-T06). When NILCORE_LOG_HMAC_KEY
// is set the chain is keyed (HMAC-SHA256), so an attacker who cannot read the key
// cannot forge a chain that verifies (audit L6). Secret-looking values are
// redacted before write so the log never holds a credential. The cross-project
// store (SQLite) graduates this log in Phase 4.
package eventlog

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"nilcore/internal/store"
)

// Event is one recorded step. Keep it flat and greppable.
type Event struct {
	Time    time.Time      `json:"time"`
	Seq     uint64         `json:"seq"` // monotonic position; anchors against reordering/gaps
	Task    string         `json:"task"`
	Kind    string         `json:"kind"` // task_start | model_call | tool_exec | verify | gate | ...
	Backend string         `json:"backend,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
	Prev    string         `json:"prev,omitempty"` // hash of the previous event
	Hash    string         `json:"hash,omitempty"` // hash of this event (chains the log)
}

// Log is a thread-safe, append-only, hash-chained writer.
type Log struct {
	mu    sync.Mutex
	f     *os.File
	path  string       // on-disk path (for read-back rebuilds; see Path)
	prev  string       // hash of the last *durably written* event
	seq   uint64       // sequence number for the next event
	key   []byte       // optional HMAC key (NILCORE_LOG_HMAC_KEY); nil = plain SHA-256
	store *store.Store // optional second backing (P4-T02); JSONL stays the export
	// onAppend is an optional hook invoked with each event AFTER it is durably written
	// (and mirrored to the store). It is the Phase-16 experience seam (EXP-T03): the
	// projector folds a verifier-judged race_outcome into its DERIVED projection as the
	// event lands, so OverStore stays warm without a full replay. nil (the default) is a
	// no-op, so an unwired log is byte-identical.
	onAppend func(e Event)

	// The onAppend hook runs on a SINGLE background drainer goroutine, not inside
	// Append's critical section: the projector's Fold does several SQLite round-trips on
	// a single-connection DB (busy_timeout 5s), so calling it under l.mu would couple the
	// authoritative log's write latency to derived-projection I/O under contention. The
	// drainer preserves ORDER (Fold's high-water mark skips seq <= watermark, so
	// out-of-order folding would LOSE events) and Append's send is non-blocking, so a slow
	// fold never stalls the log — the projection is derived + rebuildable, droppable under
	// sustained overload. nil hookCh ⇒ no hook wired (byte-identical).
	hookCh   chan hookMsg
	hookDone chan struct{}  // closed by Close to stop the drainer
	hookWG   sync.WaitGroup // tracks the drainer goroutine (joined by Close)
	dropOnce sync.Once      // one-time "projection behind" warning

	err error // first write failure, if any (a broken audit trail is loud)
}

// hookMsg carries one event to fold on the async drainer.
type hookMsg struct {
	e Event
}

// hookBuffer bounds the drainer's queue. A burst up to this size is absorbed without
// touching the log write path; beyond it, folds are dropped (rebuildable via
// `nilcore experience --rebuild`) rather than blocking the authoritative writer.
const hookBuffer = 1024

// logKey reads the optional chain HMAC key from the environment (invariant I3:
// secrets from the environment only). An empty value leaves the chain unkeyed.
func logKey() []byte {
	if k := os.Getenv("NILCORE_LOG_HMAC_KEY"); k != "" {
		return []byte(k)
	}
	return nil
}

// UseStore wires a SQLite store as a second backing: each appended event (with
// its hash chain) is also written to the store, while the JSONL file remains
// available as an export. Append's signature and all callers are unchanged.
func (l *Log) UseStore(s *store.Store) {
	if l == nil {
		return
	}
	// Set under the lock so the write is properly synchronized with a concurrent
	// Append that reads l.store under the same lock (these are normally set once at
	// startup, before any Append, but the lock makes that race-free regardless).
	l.mu.Lock()
	defer l.mu.Unlock()
	l.store = s
}

// OnAppend registers an optional hook called with each event AFTER it is durably
// appended and mirrored to the store. It is the seam the Phase-16 experience
// projector uses to fold a verifier-judged race_outcome into its derived projection
// as it lands (EXP-T03). The hook runs under the log lock (appends stay serialized)
// and its result is IGNORED: a derived projection failing must never break or stall
// the authoritative append-only log (I5). nil (the default) installs nothing, so an
// unwired log is byte-identical. Set it once, before traffic.
func (l *Log) OnAppend(fn func(e Event)) {
	if l == nil {
		return
	}
	// Set under the lock so the write is synchronized with a concurrent Append that
	// reads l.onAppend under the same lock (set once at startup in practice).
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onAppend = fn
	// Start the single drainer once, on first non-nil hook. It invokes fn IN ORDER off
	// the append critical section, so a slow/contended fold never stalls the log writer.
	if fn != nil && l.hookCh == nil {
		l.hookCh = make(chan hookMsg, hookBuffer)
		l.hookDone = make(chan struct{})
		l.hookWG.Add(1)
		// Pass the done channel by value: the drainer must select on THIS channel, not on
		// the l.hookDone field (which Close nils out), or a receive on nil would block it.
		go l.drainHooks(fn, l.hookDone)
	}
}

// drainHooks is the single background consumer of the OnAppend queue. It invokes fn for
// each event in FIFO order (preserving the projector's watermark ordering). A panic in a
// buggy projector is RECOVERED (and surfaced) so it can never take down the drainer or
// affect the durable log. Close drains everything still enqueued before returning, so a
// caller can synchronize with the projection by closing the log.
func (l *Log) drainHooks(fn func(e Event), done <-chan struct{}) {
	defer l.hookWG.Done()
	fold := func(m hookMsg) {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "nilcore: experience fold hook panicked: %v\n", r)
			}
		}()
		fn(m.e)
	}
	for {
		select {
		case <-done:
			// Clean shutdown: drain everything already enqueued (so a Close does not lose
			// folds the Appends before it produced), then exit.
			for {
				select {
				case m := <-l.hookCh:
					fold(m)
				default:
					return
				}
			}
		case m := <-l.hookCh:
			fold(m)
		}
	}
}

// Open opens (creating if needed) the log at path, continuing the hash chain and
// the sequence counter from any existing content.
func Open(path string) (*Log, error) {
	// Heal a torn final line before opening for append. If the file is non-empty and
	// does not end in '\n', a prior process crashed mid-write (or a short write
	// occurred), leaving a partial record with no terminator. TRUNCATE the partial
	// bytes back to the end of the last complete line, so the NEXT record starts on a
	// clean boundary rather than being concatenated into the partial one (which would
	// corrupt the next event too) AND so Verify is not permanently broken by an
	// unparseable trailing line (a single torn tail otherwise fails json.Unmarshal in
	// the Verify loop forever, with no recovery path).
	//
	// This stays append-only in spirit (I5): the partial bytes NEVER durably became a
	// committed event — Append advances l.prev/l.seq only AFTER a full line write lands,
	// so no completed record is dropped here. We remove only the never-finished tail of
	// an interrupted write, exactly the bytes that were never a record. (A fully written
	// line always ends in '\n', so a missing terminator unambiguously marks an
	// interrupted write — there is no valid record without it.)
	if data, rerr := os.ReadFile(path); rerr == nil && len(data) > 0 && data[len(data)-1] != '\n' {
		// Keep everything up to and including the last newline; drop the partial tail.
		// If there is no newline at all, the whole file is one torn line ⇒ truncate to 0.
		keep := strings.LastIndexByte(string(data), '\n') + 1
		if tf, terr := os.OpenFile(path, os.O_WRONLY, 0o644); terr == nil {
			_ = tf.Truncate(int64(keep))
			_ = tf.Close()
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, key: logKey(), path: path}
	if last, ok := lastEvent(path); ok {
		l.prev = last.Hash
		l.seq = last.Seq + 1
	}
	return l, nil
}

// Append records e: it redacts secrets, links e to the previous event, hashes it,
// and writes one JSON line. History is never mutated. The signature is unchanged
// from Phase 0 (no error return) so callers are untouched.
func (l *Log) Append(e Event) {
	if l == nil {
		return
	}
	e.Time = time.Now().UTC()
	redact(e.Detail)

	l.mu.Lock()
	defer l.mu.Unlock()
	e.Prev = l.prev
	e.Seq = l.seq
	e.Hash = chainHash(e, l.key)
	b, err := json.Marshal(e)
	if err != nil {
		l.fail(fmt.Errorf("marshal event: %w", err))
		return // never advance the chain past a record we could not encode
	}
	line := append(b, '\n')
	n, err := l.f.Write(line)
	if err != nil || n != len(line) {
		if err == nil {
			err = fmt.Errorf("short write: %d of %d bytes", n, len(line))
		}
		l.fail(fmt.Errorf("append event: %w", err))
		// The anchor was not (fully) persisted: keep prev and seq pointing at the
		// last event that actually reached disk, so the chain stays consistent with
		// the file. A partial line, if any, surfaces as corruption under Verify —
		// the honest signal, never a silent gap.
		return
	}
	l.prev = e.Hash
	l.seq++

	// Second backing: mirror the (already hash-chained, now-durable) event into
	// the store. Only after the file write landed, so the two backings agree. The
	// JSONL stays the authoritative export, so a store failure loses no event —
	// but it IS a degraded second backing, so surface it through the same fail/Err
	// path a file-write failure uses rather than swallowing it silently. (Append
	// is fire-and-forget with no ctx, so the mirror uses a background ctx; the
	// store's busy_timeout makes a contended write wait rather than error.)
	if l.store != nil {
		detail, _ := json.Marshal(e.Detail)
		if serr := l.store.InsertEvent(context.Background(), store.Event{
			Seq:  e.Seq,
			Time: e.Time, Task: e.Task, Kind: e.Kind, Backend: e.Backend,
			Detail: string(detail), Prev: e.Prev, Hash: e.Hash,
		}); serr != nil {
			l.fail(fmt.Errorf("mirror event to store: %w", serr))
		}
	}

	// Phase-16 experience seam: hand the now-durable event to the async drainer, which
	// folds it into the derived projection (EXP-T03) OFF this critical section. The send
	// is NON-BLOCKING: a slow/contended fold can never stall the authoritative log writer
	// — if the drainer falls behind (buffer full) we DROP the fold (the projection is
	// rebuildable via `nilcore experience --rebuild`) and warn once. nil hookCh ⇒
	// byte-identical no-op.
	if l.hookCh != nil {
		select {
		case l.hookCh <- hookMsg{e: e}:
		default:
			l.dropOnce.Do(func() {
				fmt.Fprintln(os.Stderr, "nilcore: experience projection is behind — dropping folds; rebuild with `nilcore experience --rebuild`")
			})
		}
	}
}

// fail records the first write failure and emits a one-line diagnostic. A
// silently broken audit trail is unacceptable (invariant I5), so the failure is
// both retained (see Err) and surfaced on stderr the moment it happens.
func (l *Log) fail(err error) {
	if l.err == nil {
		l.err = err
	}
	fmt.Fprintf(os.Stderr, "nilcore: event log write failed: %v\n", err)
}

// Err reports the first write failure the log has encountered, or nil if the
// audit trail is intact. Operators can poll it to detect a degraded log (e.g. a
// full disk) without changing Append's fire-and-forget signature.
func (l *Log) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Path reports the on-disk path of the log (empty for a nil log). It lets a wiring
// layer rebuild a derived window (e.g. the blast per-UTC-day $ meter) from the durable
// log on boot without threading the path separately — the rebuild-on-boot discipline.
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close closes the underlying file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	// Stop the hook drainer (if any) before closing the file: signal it to exit and join
	// it. It finishes its current fold then returns; any still-buffered folds are dropped
	// (the projection is rebuildable). Idempotent — hookDone is nil'd after the first close.
	l.mu.Lock()
	done := l.hookDone
	l.hookDone = nil
	l.mu.Unlock()
	if done != nil {
		close(done)
		l.hookWG.Wait()
	}
	return l.f.Close()
}

// chainHash computes the chain hash over the event (with Hash cleared), so it
// covers Prev and Seq and is reproducible by Verify. With a non-empty key it is
// HMAC-SHA256 (unforgeable without the key); otherwise plain SHA-256.
func chainHash(e Event, key []byte) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	if len(key) > 0 {
		m := hmac.New(sha256.New, key)
		m.Write(b)
		return hex.EncodeToString(m.Sum(nil))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lastEvent returns the last event in the file (ok=false if none/corrupt).
func lastEvent(path string) (Event, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return Event{}, false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Scan backward to the last PARSEABLE event. A crash can leave a torn final line;
	// the chain must still resume from the last event that fully reached disk (with
	// its real Seq), never reset to 0 because only the tail is corrupt.
	for i := len(lines) - 1; i >= 0; i-- {
		var e Event
		if json.Unmarshal([]byte(lines[i]), &e) == nil {
			return e, true
		}
	}
	return Event{}, false
}

// Verify re-reads the log at path and checks the chain end to end — sequence
// anchor, prev links, and (keyed) hashes — returning an error at the first
// tampered, reordered, dropped, or corrupt event. It reads NILCORE_LOG_HMAC_KEY
// the same way Open does, so a keyed log is verified under its key.
func Verify(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	key := logKey()
	prev := ""
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return fmt.Errorf("event %d: %w", i+1, err)
		}
		if e.Seq != uint64(i) {
			return fmt.Errorf("event %d: sequence anchor mismatch (got %d, want %d)", i+1, e.Seq, i)
		}
		if e.Prev != prev {
			return fmt.Errorf("event %d: chain break (prev does not link)", i+1)
		}
		if want := chainHash(e, key); !hmac.Equal([]byte(want), []byte(e.Hash)) {
			return fmt.Errorf("event %d: hash mismatch (tampered or wrong key)", i+1)
		}
		prev = e.Hash
	}
	return nil
}
