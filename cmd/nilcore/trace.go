package main

// trace.go implements `nilcore trace <task>` (and its `why <task>` alias) — the
// read-only "why did it do that" explorer (Phase 13). It replays the append-only
// event log into a causal Step tree (internal/trace) and renders it to stdout: a
// task arg builds the trace for that one task; no arg (or `*`) builds one per task
// and renders each. Output is a TTY-styled text tree by default, or the trace(s) as
// JSON.
//
// It mirrors inspect/report's read-only discipline: a new subcommand off main's
// switch, no new event kinds (purely a reader — I5), default behavior off the
// literal `trace`/`why` first-arg so the rest of the CLI is byte-identical.
//
// I5 fail-flag (not fail-closed-silent): the trace LEAF already keeps building a
// structural trace over a broken hash chain but marks every node untrusted and
// stamps a loud CHAIN BROKEN verdict (trace.Trace.ChainVerified == false). The
// renderer surfaces that loudly; this command additionally EXITS NON-ZERO when any
// rendered trace is untrusted, so a script can detect a tampered log even though the
// structure is still shown for debugging.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"nilcore/internal/termui"
	"nilcore/internal/trace"
)

// traceMain is the `nilcore trace` / `nilcore why` entrypoint. The first positional
// (when present and not `*`) names the single task to trace; otherwise every task
// is traced. It renders to stdout and exits non-zero when a trace is over a broken
// chain (untrusted) so the broken-chain condition is scriptable. Read-only: it never
// writes the log.
func traceMain(args []string) {
	// The task is a positional that reads most naturally FIRST (`nilcore why t-1`),
	// but Go's flag package stops at the first non-flag token, which would silently
	// drop a trailing `--log`/`--format`. Split the leading positional out before
	// parsing so BOTH `trace t-1 --log X` and `trace --log X t-1` work (and a flag
	// after the task is honored rather than ignored). Empty/`*` ⇒ all tasks.
	task, rest := splitLeadingTask(args)

	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	logPath := fs.String("log", defaultLogPath, "append-only event log path")
	format := fs.String("format", "text", "render format: text | json")
	_ = fs.Parse(rest)

	// A positional that appeared AFTER the flags (e.g. `trace --log X t-1`) lands in
	// fs.Args(); honor it when no leading task was given, so either ordering names the
	// task.
	if task == "" {
		if tail := fs.Args(); len(tail) > 0 {
			task = tail[0]
		}
	}

	out, exit, err := runTrace(*logPath, task, *format, termui.New(os.Stdout).Style())
	if err != nil {
		// A genuinely unreadable/unparseable log is fatal; a broken CHAIN is NOT — that
		// still renders (with the untrusted flag) and returns exit != 0 below.
		fatal(err)
	}
	fmt.Fprint(os.Stdout, out)
	if exit != 0 {
		os.Exit(exit)
	}
}

// splitLeadingTask pulls a leading positional task arg off the front of args so a
// trailing flag is not swallowed by Go's flag parser (which stops at the first
// non-flag token). If the first arg does NOT start with '-', it is the task and the
// rest are the flags; otherwise there is no leading task and all args are flags (a
// post-flag positional, if any, is recovered by the caller from fs.Args()).
func splitLeadingTask(args []string) (task string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// runTrace is the pure command core, separated from traceMain so the exit code and
// the broken-chain flagging are testable without os.Exit/stdout capture. It returns
// the rendered text, the process exit code (0 clean, 1 when any rendered trace is
// untrusted — a broken chain, surfaced not hidden), and a fatal error for an
// unreadable/unparseable log or an invalid format. st only affects the text
// renderer; JSON is style-agnostic.
//
// A task arg ⇒ trace.Build (one trace); empty or "*" ⇒ trace.BuildAll (one per
// task, each rendered). The leaf already marks an over-broken-chain trace untrusted
// (ChainVerified == false); we read that to set the exit code, so the trust verdict
// is both visible (loud renderer banner) and scriptable (non-zero exit).
func runTrace(logPath, task, format string, st termui.Style) (string, int, error) {
	if err := validTraceFormat(format); err != nil {
		return "", 0, err
	}

	var traces []*trace.Trace
	if task == "" || task == "*" {
		ts, err := trace.BuildAll(logPath)
		if err != nil {
			return "", 0, fmt.Errorf("trace: %w", err)
		}
		traces = ts
	} else {
		tr, err := trace.Build(logPath, task)
		if err != nil {
			return "", 0, fmt.Errorf("trace: %w", err)
		}
		traces = []*trace.Trace{tr}
	}

	if format == "json" {
		// One JSON document for the whole result: a single trace for a task arg, or
		// the array for the all-tasks read. The leaf's exported Trace fields carry the
		// ChainVerified flag, so a consumer sees the trust verdict in the data too.
		b, err := marshalTraces(traces, task)
		if err != nil {
			return "", 0, fmt.Errorf("trace: marshalling: %w", err)
		}
		return string(b) + "\n", exitForTraces(traces), nil
	}

	out := ""
	for _, tr := range traces {
		out += trace.Render(tr, st)
	}
	return out, exitForTraces(traces), nil
}

// marshalTraces renders the JSON document for the result: the single trace object
// when a specific task was named, else the array of all traces. Keeping the
// single-task shape an object (not a one-element array) matches the text path
// (one trace in, one trace out) so a JSON consumer of `trace <task>` reads a Trace
// directly.
func marshalTraces(traces []*trace.Trace, task string) ([]byte, error) {
	if task != "" && task != "*" && len(traces) == 1 {
		return json.MarshalIndent(traces[0], "", "  ")
	}
	return json.MarshalIndent(traces, "", "  ")
}

// exitForTraces returns 1 when ANY rendered trace is over a broken chain (not
// ChainVerified), else 0. A break anywhere taints the whole single hash-chained log
// (the leaf gives every trace the same verdict), so this is conservative by
// construction — one untrusted trace fails the command.
func exitForTraces(traces []*trace.Trace) int {
	for _, tr := range traces {
		if tr != nil && !tr.ChainVerified {
			return 1
		}
	}
	return 0
}

// validTraceFormat rejects an unknown -format up front so a typo fails loudly
// rather than silently rendering text.
func validTraceFormat(format string) error {
	switch format {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("trace: unknown -format %q (want text | json)", format)
	}
}
