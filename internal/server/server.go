// Package server is NilCore's long-running serve mode: it listens on a channel,
// dispatches each inbound task request through the orchestrator one at a time,
// streams progress back, and shuts down cleanly when its context is cancelled
// (SIGINT/SIGTERM). P1-T07.
package server

import (
	"context"
	"fmt"
	"sync/atomic"

	"nilcore/internal/backend"
	"nilcore/internal/channel"
	"nilcore/internal/eventlog"
	"nilcore/internal/policy"
)

// RunFunc executes one task for a thread and returns a human-readable summary.
// The approver routes irreversible-action gates back to that thread (chat gate).
type RunFunc func(ctx context.Context, t backend.Task, approver policy.Approver) (string, error)

// Server dispatches channel task requests through Run, one at a time.
type Server struct {
	Channel channel.Channel
	Log     *eventlog.Log
	Run     RunFunc
	seq     atomic.Int64
}

// Serve runs the listen→dispatch loop until ctx is cancelled. It returns nil on a
// clean shutdown; transient channel errors are logged and the loop continues.
func (s *Server) Serve(ctx context.Context) error {
	s.Log.Append(eventlog.Event{Kind: "serve_start"})
	for {
		req, err := s.Channel.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.Log.Append(eventlog.Event{Kind: "serve_stop"})
				return nil
			}
			s.Log.Append(eventlog.Event{Kind: "serve_error", Detail: map[string]any{"error": err.Error()}})
			continue
		}

		id := fmt.Sprintf("t-%d", s.seq.Add(1))
		s.Log.Append(eventlog.Event{Task: id, Kind: "serve_dispatch",
			Detail: map[string]any{"sender": req.Sender, "thread": req.ThreadID}})
		_ = s.Channel.Update(ctx, req.ThreadID, "Starting: "+req.Goal)

		approver := channel.NewApprover(ctx, s.Channel, req.ThreadID)
		summary, runErr := s.Run(ctx, backend.Task{ID: id, Goal: req.Goal}, approver)
		if runErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			_ = s.Channel.Update(ctx, req.ThreadID, "Failed: "+runErr.Error())
			continue
		}
		_ = s.Channel.Update(ctx, req.ThreadID, summary)
	}
}
