// checks.go holds the three audit CheckFuncs. Each derives a worktree-confined
// <relpath>:<line> locator from the claim's Evidence.SourceURL, reaches the LOCAL
// worktree once via a fixed sed/grep verb in the box (I4), and asserts a typed Status
// host-side. There is no network leg — the verdict is a pure function of files on disk.
//
// Verdict discipline shared by all three:
//   - locator missing / unparseable / path-escape  => Unverifiable, and for an escape
//     NO box call is made (the locator is refused before any reach).
//   - sandbox-level error (the box could not run)   => Unverifiable.
//   - the cited line / match definitively absent     => Fail (re-derive the finding).
//   - the cited line / match present and matching     => Pass.
//
// Fail vs Unverifiable matters for requeue routing: a Fail re-derives the finding's
// content, an Unverifiable fixes the locator/binding. The raw on-disk line is read and
// asserted host-side and is NEVER echoed into Detail (I7) — only a bounded,
// harness-authored note describing the verdict leaves the pack.

package audit

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"nilcore/internal/artifact"
	"nilcore/internal/sandbox"
)

// value returns the trimmed asserted datum, or an error if empty. An empty Value is not
// a real assertion (it would vacuously match every line), so the pattern/reproduce
// checks treat it as Unverifiable rather than a free Pass.
func value(c artifact.Claim) (string, error) {
	v := strings.TrimSpace(c.Evidence.Value)
	if v == "" {
		return "", fmt.Errorf("evidence.value (the asserted finding text) is required")
	}
	return v, nil
}

// readCitedLine runs ONE `sed -n '<N>p' '<path>'` inside the box and returns the raw
// line at the cited locator. It centralizes the fail-closed discipline the existence
// and pattern checks share: a nil box ⇒ refuse (no host-side file read, which would
// bypass the sandbox boundary — I4); a sandbox-level error ⇒ Unverifiable; a non-zero
// sed exit (e.g. the file is absent) or an empty body ⇒ the caller decides (existence
// is a Fail there). ok is false on a refuse/error path, carrying the Status/detail to
// return verbatim. The returned line is UNTRUSTED data parsed host-side; it is never
// echoed into Detail by the caller (I7).
//
// The verb (verbSed) is a pack constant; only the validated, single-quoted line number
// and path follow it, so the model cannot inject a command (I4).
func readCitedLine(ctx context.Context, box sandbox.Sandbox, loc fileLine) (line string, exit int, st artifact.Status, d string, ok bool) {
	if box == nil {
		return "", 0, artifact.StatusUnverifiable, "no sandbox available (refusing a host-side file read)", false
	}
	// sed -n '<N>p' '<path>' — print only the cited line. The line number is an integer
	// (validated) so it is safe to format directly; the path is single-quoted (validated
	// to carry no quote/whitespace/control byte).
	cmd := fmt.Sprintf("%s '%dp' %s", verbSed, loc.line, quote(loc.path))
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		return "", 0, artifact.StatusUnverifiable, detail("sandbox: " + err.Error()), false
	}
	return res.Stdout, res.ExitCode, artifact.StatusUnverified, "", true
}

// checkFileLineExists asserts the cited <relpath>:<line> resolves to a real, non-empty
// line in the worktree (the finding points at a location that exists). It runs ONE
// `sed -n '<N>p' '<path>'`:
//   - exit 0 and a non-empty body  => Pass (the line exists and has content).
//   - exit 0 but an empty body      => Fail (the file exists but has no such line, or the
//     line is blank — the finding's location is not real content).
//   - non-zero exit                  => Fail (the file is absent / unreadable: the cited
//     path does not exist in the worktree).
//   - a locator that does not parse / escapes the worktree => Unverifiable, and an escape
//     makes NO box call.
//   - a sandbox-level error          => Unverifiable.
func checkFileLineExists(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	loc, err := validateFileLine(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	line, exit, st, d, ok := readCitedLine(ctx, box, loc)
	if !ok {
		return st, d
	}
	if exit != 0 {
		// sed exits non-zero only when it cannot open the file — the cited path is not in
		// the worktree. A definitive "does not exist" is a Fail, not Unverifiable.
		return artifact.StatusFail, detail(fmt.Sprintf("%s: file not found or unreadable in worktree", loc.path))
	}
	// sed prints nothing (exit 0) when the file exists but has fewer than N lines. The
	// cited location is not real content => Fail. We assert on the raw line host-side but
	// never echo it (I7).
	if strings.TrimSpace(line) == "" {
		return artifact.StatusFail, detail(fmt.Sprintf("%s:%d resolves to an empty or absent line", loc.path, loc.line))
	}
	return artifact.StatusPass, fmt.Sprintf("%s:%d exists with content", loc.path, loc.line)
}

// checkPatternMatches asserts the cited line CONTAINS the model-authored Evidence.Value
// (whitespace-normalized). It reads the single cited line in-box and performs the
// substring test ENTIRELY host-side (the line is parsed as trusted Go, never re-shelled
// and never echoed — I7):
//   - the normalized Value is a substring of the normalized line => Pass.
//   - the line exists but does not contain the Value             => Fail.
//   - the line is absent/empty                                    => Fail.
//   - an empty Value / unparseable locator / path-escape          => Unverifiable (escape
//     makes NO box call).
//   - a sandbox-level error                                        => Unverifiable.
//
// Doing the match host-side (rather than via grep) keeps the model-authored pattern out
// of the shell entirely for this check — it is never interpolated as a regex/argument,
// so there is nothing to quote-escape, and the raw line never leaves the process.
func checkPatternMatches(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	loc, err := validateFileLine(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	want, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	line, exit, st, d, ok := readCitedLine(ctx, box, loc)
	if !ok {
		return st, d
	}
	if exit != 0 {
		return artifact.StatusFail, detail(fmt.Sprintf("%s: file not found or unreadable in worktree", loc.path))
	}
	if strings.TrimSpace(line) == "" {
		return artifact.StatusFail, detail(fmt.Sprintf("%s:%d resolves to an empty or absent line", loc.path, loc.line))
	}
	// Whitespace-normalize both sides so incidental indentation does not defeat a genuine
	// match. The comparison is host-side over trusted Go; the raw line is NEVER placed in
	// Detail (I7) — only a verdict note that names the locator (harness-authored) leaves.
	if strings.Contains(normalize(line), normalize(want)) {
		return artifact.StatusPass, fmt.Sprintf("%s:%d contains the asserted pattern", loc.path, loc.line)
	}
	return artifact.StatusFail, detail(fmt.Sprintf("%s:%d does not contain the asserted pattern", loc.path, loc.line))
}

// checkFindingReproduces asserts that the asserted pattern reproduces AT the cited line —
// i.e. grepping the file yields the CLAIMED number of matches that fall at (or within
// reproWindow lines of) the locator's cited line, not merely somewhere in the file. The
// cited line component is the anchor: a finding pinned at line 5 that only matches at line
// 200 does NOT reproduce, even though the pattern is present in the file. Evidence.Value
// carries the fixed-string pattern; the claimed count is an integer in the claim's
// Statement (the worker states "this appears N times at the citation"); the file is the
// locator's path.
//
// It runs ONE `grep -n -F '<pattern>' '<path>'`:
//   - -F: the pattern is a FIXED string, never a regex — a model-authored pattern can
//     never become a surprising regex, and is single-quoted so it stays DATA (I4).
//   - -n: grep prefixes each match with its 1-based line number ("<lineno>:<text>"). We
//     parse only the LINE NUMBERS host-side and count those within reproWindow of the
//     cited line; the matched line TEXT is never parsed into Detail (I7).
//   - exit 0 and the windowed count equals the claim       => Pass.
//   - exit 1 (grep: no match anywhere) and the claim is 0  => Pass; otherwise => Fail.
//   - exit 0 but the windowed count != the claim            => Fail (the finding does not
//     reproduce at the citation, even if the pattern appears elsewhere in the file).
//   - exit >1 (grep error, e.g. file unreadable)            => Unverifiable.
//   - a sandbox-level error / unparseable -n output / missing-or-bad claimed count => Unverifiable.
//   - an empty pattern / unparseable locator / path-escape   => Unverifiable (escape makes NO box call).
func checkFindingReproduces(ctx context.Context, box sandbox.Sandbox, c artifact.Claim) (artifact.Status, string) {
	loc, err := validateFileLine(c.Evidence.SourceURL)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	pattern, err := value(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	wantCount, err := claimedCount(c)
	if err != nil {
		return artifact.StatusUnverifiable, detail(err.Error())
	}
	if box == nil {
		return artifact.StatusUnverifiable, "no sandbox available (refusing a host-side file read)"
	}
	if strings.ContainsAny(pattern, "'\n") {
		// A single quote would break the single-quoting; a newline would split the grep
		// invocation. Such a pattern cannot be safely interpolated as a fixed string ⇒
		// refuse rather than risk it (the locator path was already validated).
		return artifact.StatusUnverifiable, "asserted pattern contains a quote or newline (cannot verify safely)"
	}
	// grep -n -F '<pattern>' '<path>': fixed-string, line-numbered. The verb is a pack
	// constant; the pattern and path are single-quoted DATA.
	cmd := fmt.Sprintf("%s %s %s", verbGrep, quote(pattern), quote(loc.path))
	res, err := box.Exec(ctx, cmd)
	if err != nil {
		return artifact.StatusUnverifiable, detail("sandbox: " + err.Error())
	}
	switch res.ExitCode {
	case 0:
		// Count only matches whose line number lands within reproWindow of the cited line —
		// anchoring the reproduction at the citation rather than anywhere in the file. The
		// line-number parse is over trusted Go; the matched TEXT is discarded (I7).
		got, perr := countNear(res.Stdout, loc.line, reproWindow)
		if perr != nil {
			return artifact.StatusUnverifiable, detail("unparseable grep -n output: " + perr.Error())
		}
		if got == wantCount {
			return artifact.StatusPass, fmt.Sprintf("%s:%d reproduces the finding %d time(s) as claimed", loc.path, loc.line, wantCount)
		}
		return artifact.StatusFail, detail(fmt.Sprintf("%s:%d has %d match(es) within %d line(s), claim asserts %d", loc.path, loc.line, got, reproWindow, wantCount))
	case 1:
		// grep exit 1 == zero matches anywhere in the file. The finding reproduces 0 times
		// at the citation: Pass iff the claim asserted 0, else Fail (the asserted finding is
		// not present).
		if wantCount == 0 {
			return artifact.StatusPass, fmt.Sprintf("%s:%d reproduces the finding 0 times as claimed", loc.path, loc.line)
		}
		return artifact.StatusFail, detail(fmt.Sprintf("%s:%d has 0 matches, claim asserts %d", loc.path, loc.line, wantCount))
	default:
		// grep exit >1 is a real error (file unreadable, bad invocation): not a decisive
		// verdict about the finding ⇒ Unverifiable.
		d := strings.TrimSpace(res.Stderr)
		if d == "" {
			d = fmt.Sprintf("grep exited %d", res.ExitCode)
		}
		return artifact.StatusUnverifiable, detail(d)
	}
}

// claimedCount parses the model-authored claimed match count carried in the claim's
// Statement (the prose-context field). It must be a non-negative integer; an empty or
// non-integer Statement is Unverifiable (the reproduce check has no count to assert).
// Statement is DATA — only the parsed integer is used, never echoed.
func claimedCount(c artifact.Claim) (int, error) {
	s := strings.TrimSpace(c.Statement)
	if s == "" {
		return 0, fmt.Errorf("evidence.statement (the claimed match count) is required")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("claimed count %q is not an integer", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("claimed count must be non-negative (got %d)", n)
	}
	return n, nil
}

// countNear parses `grep -n` output ("<lineno>:<text>" per match) and returns how many
// matches fall within window lines of cited (i.e. |lineno-cited| <= window) — the anchored
// reproduction count. Only the leading line NUMBER of each non-empty line is parsed; the
// matched text after the first ':' is DISCARDED and never surfaces (I7). A line whose
// prefix before the first ':' is not an integer is a malformed grep emission and yields an
// error (mapped to Unverifiable), so a garbled stream can never masquerade as a count.
func countNear(stdout string, cited, window int) (int, error) {
	n := 0
	for _, ln := range strings.Split(stdout, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		idx := strings.IndexByte(ln, ':')
		if idx < 0 {
			return 0, fmt.Errorf("grep -n line %q has no ':' line-number separator", ln)
		}
		num, err := strconv.Atoi(strings.TrimSpace(ln[:idx]))
		if err != nil {
			return 0, fmt.Errorf("grep -n line-number %q is not an integer", ln[:idx])
		}
		d := num - cited
		if d < 0 {
			d = -d
		}
		if d <= window {
			n++
		}
	}
	return n, nil
}
