package main

// report_swarm_test.go is the SW-T16 hermetic smoke test for the swarm-dimension
// extension of `nilcore report`. It builds a real hash-chained event log (verify +
// the swarm Kinds scoreboard_snapshot/swarm_pass_clean + an artifact_verify) and a
// matching persisted artifact whose SourceURL carries a ?api_key= credential, then
// drives runSwarmReport over the new formats. The assertions pin the three things the
// extension must guarantee:
//   - the swarm formats render non-empty over a real fold (matrix + json + the
//     legacy text/md/html still work via the same command core),
//   - a corrupted hash chain forces FinalPass=false / exit 1 in EVERY format — the
//     RED verdict is never hidden behind a format choice (I2),
//   - the json deliverable is the REDACTED projection: a smuggled ?api_key=secret in
//     a SourceURL is scrubbed, so no secret rides out (I3).
//
// It reuses the in-package helpers plainStyle/breakChain from report_test.go (same
// package main) and adds only a swarm-flavored log seeder.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nilcore/internal/artifact"
	"nilcore/internal/eventlog"
)

// theSecret is the credential value smuggled into the artifact's SourceURL query
// string. The json deliverable must never echo it (I3) — the test greps the whole
// rendered document for this literal.
const theSecret = "supersekretvalue123"

// seedSwarmLog writes a hash-chained log carrying a passing verify event, the two
// swarm Kinds the SwarmDimension folds (a scoreboard_snapshot with a clean final
// tally + a swarm_pass_clean signal), and an artifact_verify naming a GREEN artifact
// persisted under root. The artifact's single claim carries a SourceURL with a
// ?api_key=<theSecret> param so the json-redaction assertion has a real key to scrub.
// Returns the log path.
func seedSwarmLog(t *testing.T, root string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "swarm.jsonl")
	log, err := eventlog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log.Append(eventlog.Event{Task: "swarm/run-1/0", Kind: "verify", Detail: map[string]any{"passed": true}})
	// One scoreboard snapshot: the final pass, every shard checked and passed, zero
	// remaining — the clean-tally leg of FinalCleanPass.
	log.Append(eventlog.Event{Task: "swarm/run-1", Kind: "scoreboard_snapshot", Detail: map[string]any{
		"pass": 1, "checked": 1, "passed": 1, "failed": 0, "retry_pass": 0, "remaining": 0,
	}})
	// The clean-pass signal: the controller's MarkClean gate fired on a converged pass.
	log.Append(eventlog.Event{Task: "swarm/run-1", Kind: "swarm_pass_clean", Detail: map[string]any{"pass": 1}})
	log.Append(eventlog.Event{Task: "swarm/run-1/0", Kind: "artifact_verify", Detail: map[string]any{"id": "art-1", "green": true}})
	log.Close()

	a := &artifact.Artifact{
		ID:    "art-1",
		Kind:  artifact.KindReport,
		Title: "Swarm shard art-1",
		Claims: []artifact.Claim{{
			ID:    "c-1",
			Field: "revenue_fy2024",
			Evidence: artifact.Evidence{
				Value: "100",
				// A SourceURL with a smuggled credential param — the json deliverable's
				// redactSource MUST strip it (I3). The host is innocuous; only the param
				// is the secret carrier the test greps for.
				SourceURL: "https://example.com/facts?api_key=" + theSecret,
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

// TestReportSwarmFormats drives the SW-T16 additive formats over a real swarm log +
// fold. Each subtest is hermetic (its own temp root + log).
func TestReportSwarmFormats(t *testing.T) {
	// matrix over --dir ⇒ a non-empty cross-shard grid, exit 0 on a clean chain.
	t.Run("matrix over --dir renders non-empty exit 0", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedSwarmLog(t, root)
		out, exit, err := runSwarmReport(logPath, ".", root, "matrix", "", "", plainStyle(t))
		if err != nil {
			t.Fatalf("runSwarmReport(matrix): %v", err)
		}
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (clean chain)", exit)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatalf("matrix render is empty")
		}
		// The grid pivots the folded claim's field into a column header.
		if !strings.Contains(out, "Claim matrix") || !strings.Contains(out, "revenue_fy2024") {
			t.Errorf("matrix missing header/field column:\n%s", out)
		}
		// A clean swarm gets the clean-pass headline (chain verified + clean event +
		// zero remaining).
		if !strings.Contains(out, "swarm clean") {
			t.Errorf("clean swarm matrix missing clean headline:\n%s", out)
		}
	})

	// json over --dir ⇒ a non-empty redacted document, exit 0.
	t.Run("json over --dir renders non-empty exit 0", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedSwarmLog(t, root)
		out, exit, err := runSwarmReport(logPath, ".", root, "json", "", "", plainStyle(t))
		if err != nil {
			t.Fatalf("runSwarmReport(json): %v", err)
		}
		if exit != 0 {
			t.Fatalf("exit = %d, want 0 (clean chain)", exit)
		}
		if !strings.Contains(out, `"chain_verified": true`) || !strings.Contains(out, `"swarm_final_clean_pass": true`) {
			t.Errorf("json missing trusted swarm verdict fields:\n%s", out)
		}
	})

	// The legacy single-run formats still render via the same core (no --dir).
	t.Run("legacy text without --dir still renders", func(t *testing.T) {
		root := t.TempDir()
		logPath := seedSwarmLog(t, root)
		out, exit, err := runSwarmReport(logPath, root, "", "text", "", "", plainStyle(t))
		if err != nil {
			t.Fatalf("runSwarmReport(text): %v", err)
		}
		if exit != 0 {
			t.Fatalf("exit = %d, want 0", exit)
		}
		if strings.TrimSpace(out) == "" {
			t.Fatalf("text render is empty")
		}
	})
}

// TestReportSwarmBrokenChain proves a corrupted hash chain forces a non-zero exit
// (FinalPass=false) in EVERY swarm format — the RED verdict can never be hidden
// behind a format choice (I2/I5).
func TestReportSwarmBrokenChain(t *testing.T) {
	for _, format := range []string{"text", "md", "html", "json", "matrix"} {
		format := format
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			logPath := seedSwarmLog(t, root)
			breakChain(t, logPath)
			out, exit, err := runSwarmReport(logPath, ".", root, format, "", "", plainStyle(t))
			if err != nil {
				t.Fatalf("runSwarmReport(%s): %v", format, err)
			}
			if exit == 0 {
				t.Fatalf("format %s: exit = 0, want non-zero on a broken chain", format)
			}
			// The redacted json deliverable carries the trust verdict as a field so a
			// scripted consumer sees the broken chain too.
			if format == "json" && !strings.Contains(out, `"chain_verified": false`) {
				t.Errorf("json over a broken chain must report chain_verified=false:\n%s", out)
			}
			// The matrix shows the loud RED banner and suppresses the clean headline.
			if format == "matrix" {
				if !strings.Contains(out, "CHAIN BROKEN") {
					t.Errorf("matrix over a broken chain missing RED banner:\n%s", out)
				}
				if strings.Contains(out, "swarm clean —") {
					t.Errorf("matrix over a broken chain must not show the clean headline:\n%s", out)
				}
			}
		})
	}
}

// TestReportSwarmJSONRedactsSecret is the I3 keystone: a ?api_key=<secret> smuggled
// into a SourceURL must NOT survive into the json deliverable, which is the redacted
// projection (render.MarshalRedacted) — never a raw json.Marshal of the model.
func TestReportSwarmJSONRedactsSecret(t *testing.T) {
	root := t.TempDir()
	logPath := seedSwarmLog(t, root)
	out, _, err := runSwarmReport(logPath, ".", root, "json", "", "", plainStyle(t))
	if err != nil {
		t.Fatalf("runSwarmReport(json): %v", err)
	}
	if strings.Contains(out, theSecret) {
		t.Fatalf("json deliverable leaked the api_key secret:\n%s", out)
	}
	// Sanity: the claim itself is present (the redaction scrubbed the key, not the
	// whole document) so the test is asserting redaction, not an empty render.
	if !strings.Contains(out, "revenue_fy2024") {
		t.Errorf("json deliverable dropped the claim entirely:\n%s", out)
	}
}

// TestReportSwarmJSONOutWritesFile proves --out persists the json deliverable under
// the folded worktree's .nilcore/reports/<run>.json via the confined writer, and the
// persisted bytes carry no secret either (the same redaction guarantee on disk).
func TestReportSwarmJSONOutWritesFile(t *testing.T) {
	root := t.TempDir()
	logPath := seedSwarmLog(t, root)
	out, _, err := runSwarmReport(logPath, ".", root, "json", "myswarm", "myswarm", plainStyle(t))
	if err != nil {
		t.Fatalf("runSwarmReport(json,--out): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".nilcore", "reports", "myswarm.json"))
	if err != nil {
		t.Fatalf("read written swarm json: %v", err)
	}
	if string(got) != out {
		t.Errorf("written .json != the rendered json the command printed")
	}
	if strings.Contains(string(got), theSecret) {
		t.Errorf("persisted json deliverable leaked the api_key secret")
	}
}
