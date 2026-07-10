// Shared file-read guards for every language backend. Each backend reads a
// walker-yielded path, and two hazards live at that read — so both are handled once,
// here, rather than re-derived per language:
//
//   - Symlink escape (I4). filepath.WalkDir does not DESCEND a symlinked directory, but
//     it DOES yield a symlinked FILE; a plain os.Open would then follow it, so a
//     `repo/evil.go -> /etc/secret` link would read out-of-worktree bytes into the index
//     (and, with an embedder wired, ship them to the embedding API — an I3 exfil risk).
//     Opening the final component with O_NOFOLLOW makes a symlink fail to open, so it is
//     skipped (fail-closed), never followed. This mirrors the O_NOFOLLOW discipline the
//     host-side file tools already use (internal/worktreefs).
//   - Unbounded read (DoS). The indexer caps file COUNT and embed-body size, but nothing
//     bounds a single file BEFORE it is read — so one crafted multi-GB source file would
//     OOM the host mid os.ReadFile / go/parser. Stat'ing the opened descriptor and
//     refusing anything over maxFileBytes bounds every read.
//
// Both guards resolve to errSkipFile, which the dispatcher (Symbols/References/Calls)
// turns into a clean (nil, nil) skip — the same graceful degradation an unsupported
// extension gets — so a hostile file drops out of the index instead of failing the walk.
// O_NOFOLLOW is available on darwin and linux (this repo's targets); syscall is stdlib,
// so this adds no dependency.
package ast

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// maxFileBytes bounds a single source file the indexer will read. It is deliberately
// generous for hand-written source yet small enough to buffer harmlessly, and equals
// the per-line max-token size the line scanners already use (their sc.Buffer cap): a
// file that passes this cap therefore cannot contain a line longer than the scanner's
// own limit, so the "oversized single line aborts the whole file" case is pre-empted
// here as a clean skip rather than surfacing as a mid-scan error.
const maxFileBytes = 4 << 20 // 4 MiB

// errSkipFile signals "skip this file, do not parse it": the final path component is a
// symlink (O_NOFOLLOW refused it — the I4 guard) or the file exceeds maxFileBytes (the
// DoS guard). It is a sentinel so the dispatcher can map exactly these two cases to a
// clean skip while genuine I/O errors (missing file, permission denied) still propagate.
var errSkipFile = errors.New("codeintel/ast: source file skipped (symlink or too large)")

// openSource opens a source file for indexing behind the two guards above. On success
// the caller owns closing the returned handle; a guard trip returns errSkipFile, and any
// other open/stat failure is returned as-is (so a genuine I/O error is never masked).
func openSource(path string) (*os.File, error) {
	// O_NOFOLLOW: a symlinked final component fails to open with ELOOP instead of being
	// followed out of the worktree. path is supplied by the worktree-confined index walk.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // O_NOFOLLOW-guarded read of a worktree-walked path, size-capped below
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errSkipFile // symlinked file: fail-closed, skip (I4)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.Size() > maxFileBytes {
		_ = f.Close()
		return nil, errSkipFile // oversized: skip before reading, so it cannot OOM the host
	}
	return f, nil
}

// readSource reads a whole source file behind the same guards, for a backend that needs
// the bytes rather than a streaming handle (the Go backend, whose go/parser would
// otherwise re-open the path itself and follow a final-component symlink).
func readSource(path string) ([]byte, error) {
	f, err := openSource(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	return io.ReadAll(f)
}

// skipErr reports whether err is the skip sentinel, so the dispatcher can turn a
// symlink/oversized guard trip into a clean (nil, nil) skip.
func skipErr(err error) bool { return errors.Is(err, errSkipFile) }
