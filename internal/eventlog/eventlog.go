// Package eventlog is an append-only audit trail (invariant I5). It writes JSON
// Lines to a file (zero dependencies); every model call, tool execution, verify,
// and gate decision is recorded and replayable. Each event is hash-chained to the
// previous one so tampering or reordering is detectable (P2-T06), and secret-
// looking values are redacted before write so the log never holds a credential.
// The cross-project store (SQLite) graduates this log in Phase 4.
package eventlog

import (
	"context"
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
	store *store.Store // optional second backing (P4-T02); JSONL stays the export
	err   error        // first write failure, if any (a broken audit trail is loud)
}

// UseStore wires a SQLite store as a second backing: each appended event (with
// its hash chain) is also written to the store, while the JSONL file remains
// available as an export. Append's signature and all callers are unchanged.
func (l *Log) UseStore(s *store.Store) {
	if l != nil {
		l.store = s
	}
}

// Open opens (creating if needed) the log at path, continuing the hash chain from
// any existing content.
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f}
	l.prev = lastHash(path)
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
	e.Hash = hashEvent(e)
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
		// The anchor was not (fully) persisted: keep prev pointing at the last
		// event that actually reached disk, so the chain stays consistent with
		// the file. A partial line, if any, surfaces as corruption under Verify —
		// the honest signal, never a silent gap.
		return
	}
	l.prev = e.Hash

	// Second backing: mirror the (already hash-chained, now-durable) event into
	// the store. Only after the file write landed, so the two backings agree.
	if l.store != nil {
		detail, _ := json.Marshal(e.Detail)
		_ = l.store.InsertEvent(context.Background(), store.Event{
			Time: e.Time, Task: e.Task, Kind: e.Kind, Backend: e.Backend,
			Detail: string(detail), Prev: e.Prev, Hash: e.Hash,
		})
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

// hashEvent computes the chain hash over the event (with Hash cleared), so it
// covers Prev and is reproducible by Verify.
func hashEvent(e Event) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lastHash returns the Hash of the last event in the file (empty if none).
func lastHash(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var e Event
	if json.Unmarshal([]byte(lines[len(lines)-1]), &e) != nil {
		return ""
	}
	return e.Hash
}

// Verify re-reads the log at path and checks the hash chain end to end, returning
// an error at the first tampered, reordered, or corrupt event.
func Verify(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	prev := ""
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return fmt.Errorf("event %d: %w", i+1, err)
		}
		if e.Prev != prev {
			return fmt.Errorf("event %d: chain break (prev does not link)", i+1)
		}
		if hashEvent(e) != e.Hash {
			return fmt.Errorf("event %d: hash mismatch (tampered)", i+1)
		}
		prev = e.Hash
	}
	return nil
}
