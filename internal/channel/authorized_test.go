package channel_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nilcore/internal/channel"
	"nilcore/internal/eventlog"
)

type seqChannel struct {
	reqs    []channel.TaskRequest
	i       int
	updates []string
}

func (s *seqChannel) Receive(ctx context.Context) (channel.TaskRequest, error) {
	if s.i >= len(s.reqs) {
		<-ctx.Done()
		return channel.TaskRequest{}, ctx.Err()
	}
	r := s.reqs[s.i]
	s.i++
	return r, nil
}
func (s *seqChannel) Update(_ context.Context, _ string, msg string) error {
	s.updates = append(s.updates, msg)
	return nil
}
func (s *seqChannel) Ask(context.Context, string, string) (bool, error) { return true, nil }

func openLog(t *testing.T) (*eventlog.Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return log, path
}

func TestAuthorizedReceiveFilters(t *testing.T) {
	log, path := openLog(t)
	defer log.Close()
	sc := &seqChannel{reqs: []channel.TaskRequest{
		{Goal: "rm everything", Sender: "intruder", ThreadID: "t1"},
		{Goal: "fix the bug", Sender: "alice", ThreadID: "t2"},
	}}
	a := channel.NewAuthorized(sc, []string{"alice"}, log)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := a.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if req.Sender != "alice" || req.Goal != "fix the bug" {
		t.Fatalf("authorized request not returned: %+v", req)
	}
	// The intruder was told off.
	var told bool
	for _, u := range sc.updates {
		if strings.Contains(u, "Unauthorized") {
			told = true
		}
	}
	if !told {
		t.Error("unauthorized sender was not notified")
	}
	// ...and logged, but never executed (never returned).
	log.Close()
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "unauthorized_command") || !strings.Contains(string(b), "intruder") {
		t.Errorf("unauthorized command not logged: %s", b)
	}
}

func TestAuthorizedDenyByDefault(t *testing.T) {
	log, _ := openLog(t)
	defer log.Close()
	a := channel.NewAuthorized(&seqChannel{}, nil, log) // empty allowlist
	if a.Permit("anyone") {
		t.Error("empty allowlist must deny everyone")
	}
}
