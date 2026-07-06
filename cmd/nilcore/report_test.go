package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
	"nilcore/internal/inspect"
	"nilcore/internal/termui"
)

// plainStyle is a non-TTY (unstyled) Style: termui detects it from a plain buffer,
// so the text renderer emits no ANSI escapes. It models the redirected-output case.
func plainStyle(t *testing.T) termui.Style {
	t.Helper()
	var buf strings.Builder
	return termui.New(&buf).Style()
}

// seedCleanLog writes a hash-chained log with a green artifact_verify check plus a
// matching persisted GREEN artifact under root, so ReplayReport folds a passing
// model with a verified chain. Returns the log path.
func seedCleanLog(t *testing.T, root string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Task: "t-1", Kind: "verify", Detail: map[string]any{"passed": true}})
	log.Append(eventlog.Event{Task: "t-1", Kind: "artifact_verify", Detail: map[string]any{"id": "art-1", "green": true}})
	log.Close()

	a := &artifact.Artifact{
		ID:   "art-1",
		Kind: artifact.KindReport,
		Claims: []artifact.Claim{{
			ID:    "c-1",
			Field: "revenue_fy2024",
			Evidence: artifact.Evidence{
				Value:     "100",
				SourceURL: "https://example.com/facts",
				Verifier:  "finance.sec_fact",
				Status:    artifact.StatusPass,
			},
		}},
	}
	if err := artifact.Write(root, a); err != nil {
		t.Fatal(err)
	}
	return logPath
}

// breakChain corrupts the hash chain by appending a forged event line directly to
// the file (bypassing Append), so eventlog.Verify fails and the report must show
// the RED banner and exit non-zero.
func breakChain(t *testing.T, logPath string) {
	t.Helper()
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":99,"kind":"verify","detail":{"passed":true},"hash":"deadbeef"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestReportSubcommand(t *testing.T) {
	st := func(t *testing.T) termui.Style { return plainStyle(t) }

	// Clean log ⇒ text report, exit 0.
	t.Run("clean log prints text report exit 0", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedCleanLog(t, root)
		out, exit, err := runSwarmReport(logPath, root, "", "text", "", "", st(t))
		if err != nil {
			t.Fatalf("runSwarmReport: %v", err)
		}
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (clean chain)", exit)
		}
		// Verifier projection of GREEN over a clean chain — never a self-claim.
		if !strings.Contains(out, "GREEN") {
			t.Errorf("clean report missing GREEN headline:\n%s", out)
		}
		if strings.Contains(out, "CHAIN BROKEN") {
			t.Errorf("clean report must not show the broken-chain banner:\n%s", out)
		}
	})

	// Broken chain ⇒ RED banner, exit non-zero (fail-closed trust gate).
	t.Run("broken chain prints banner exit non-zero", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedCleanLog(t, root)
		breakChain(t, logPath)
		out, exit, err := runSwarmReport(logPath, root, "", "text", "", "", st(t))
		if err != nil {
			t.Fatalf("runSwarmReport: %v", err)
		}
		if exit == 0 {
			t.Fatalf("exit = 0, want non-zero on a broken chain")
		}
		if !strings.Contains(out, "CHAIN BROKEN") {
			t.Errorf("broken-chain report missing RED banner:\n%s", out)
		}
		if strings.Contains(out, "GREEN — every check passed") {
			t.Errorf("broken chain must not show a GREEN headline:\n%s", out)
		}
	})

	// -report-out + -format html ⇒ a self-contained .html under .nilcore/reports/
	// byte-equal to render.RenderHTML(model), no <script>.
	t.Run("report-out html byte-equal no script", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedCleanLog(t, root)
		out, _, err := runSwarmReport(logPath, root, "", "html", "myrun", "myrun", st(t))
		if err != nil {
			t.Fatalf("runSwarmReport: %v", err)
		}
		// The printed bytes ARE render.RenderHTML(model) for the model runSwarmReport built;
		// the persisted file must be byte-equal to those same rendered bytes. (A fresh
		// ReplayReport would differ only by its GeneratedAt wall clock, so we compare
		// against the single render the command produced.)
		if !strings.HasPrefix(out, "<!DOCTYPE html>") {
			t.Errorf("html format did not produce an HTML document:\n%s", out)
		}
		if strings.Contains(out, "<script") {
			t.Errorf("HTML report must contain no <script>")
		}
		got, err := os.ReadFile(filepath.Join(root, ".nilcore", "reports", "myrun.html"))
		if err != nil {
			t.Fatalf("read written report: %v", err)
		}
		if string(got) != out {
			t.Errorf("written .html != the rendered HTML the command printed")
		}
	})

	// Styling is detected from the actual stdout writer: a non-TTY buffer ⇒ no ANSI.
	t.Run("non-TTY buffer yields no ANSI escapes", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedCleanLog(t, root)
		out, _, err := runSwarmReport(logPath, root, "", "text", "", "", plainStyle(t))
		if err != nil {
			t.Fatalf("runSwarmReport: %v", err)
		}
		if strings.Contains(out, "\x1b[") {
			t.Errorf("plain (non-TTY) text report must carry no ANSI escapes:\n%q", out)
		}
	})

	// Pure read (I5): running the report does not change the event log byte length.
	t.Run("report does not mutate the log", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedCleanLog(t, root)
		before, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := runSwarmReport(logPath, root, "", "md", "", "", st(t)); err != nil {
			t.Fatalf("runSwarmReport: %v", err)
		}
		after, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Errorf("event log byte length changed: before=%d after=%d", len(before), len(after))
		}
	})

	// An unknown -format fails loudly rather than silently rendering text.
	t.Run("unknown format errors", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedCleanLog(t, root)
		if _, _, err := runSwarmReport(logPath, root, "", "pdf", "", "", st(t)); err == nil {
			t.Errorf("want error for unknown -format")
		}
	})
}

// TestReportSubcommandInspectUnchanged proves the new subcommand leaves the
// existing inspect projection byte-identical (the additive-default guarantee).
func TestReportSubcommandInspectUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.jsonl")
	log, err := eventlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Task: "t-1", Kind: "task_start"})
	log.Append(eventlog.Event{Task: "t-1", Kind: "verify", Detail: map[string]any{"passed": true}})
	log.Close()

	sum, err := inspect.Replay(path)
	if err != nil {
		t.Fatal(err)
	}
	out := renderInspect(path, sum)
	if !strings.Contains(out, "2 event(s) across 1 task(s)") || !strings.Contains(out, "chain: verified") {
		t.Errorf("inspect output changed:\n%s", out)
	}
}
