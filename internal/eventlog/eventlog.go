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
	prev  string       // hash of the last *durably written* event
	seq   uint64       // sequence number for the next event
	key   []byte       // optional HMAC key (NILCORE_LOG_HMAC_KEY); nil = plain SHA-256
	store *store.Store // optional second backing (P4-T02); JSONL stays the export
	err   error        // first write failure, if any (a broken audit trail is loud)
}

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
	if l != nil {
		l.store = s
	}
}

// Open opens (creating if needed) the log at path, continuing the hash chain and
// the sequence counter from any existing content.
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, key: logKey()}
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
			Time: e.Time, Task: e.Task, Kind: e.Kind, Backend: e.Backend,
			Detail: string(detail), Prev: e.Prev, Hash: e.Hash,
		}); serr != nil {
			l.fail(fmt.Errorf("mirror event to store: %w", serr))
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

// Close closes the underlying file.
func (l *Log) Close() error {
	if l == nil {
		return nil
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
	var e Event
	if json.Unmarshal([]byte(lines[len(lines)-1]), &e) != nil {
		return Event{}, false
	}
	return e, true
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
