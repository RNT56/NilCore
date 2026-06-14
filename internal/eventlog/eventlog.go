// Package eventlog is an append-only audit trail. Phase 0 writes JSON Lines to a
// file (zero dependencies); every tool call, model call, verify, and gate is
// recorded and replayable. The cross-project memory store (SQLite) reads from
// and graduates this log in Phase 1+.
package eventlog

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event is one recorded step. Keep it flat and greppable.
type Event struct {
	Time    time.Time      `json:"time"`
	Task    string         `json:"task"`
	Kind    string         `json:"kind"` // task_start | model_call | tool_exec | verify | final_verify | gate | result
	Backend string         `json:"backend,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// Log is a thread-safe append-only writer.
type Log struct {
	mu sync.Mutex
	f  *os.File
}

func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

func (l *Log) Append(e Event) {
	if l == nil {
		return
	}
	e.Time = time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, _ := json.Marshal(e)
	_, _ = l.f.Write(append(b, '\n'))
}

func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	return l.f.Close()
}
