package maint

import (
	"fmt"
	"os"
)

// RotateLog caps a log file at maxBytes. If path is at or below the limit (or
// does not exist), it is left alone. Otherwise the current file is rotated to
// path+".1" — replacing any previous ".1" — and a fresh, empty path is created
// in its place, so the active writer keeps appending to the same path while the
// bulky history is preserved one generation back.
//
// Rotation is intentionally single-generation: this is a safety valve against
// unbounded growth, not a full logrotate. Callers wanting deeper retention can
// chain their own generations on top.
func RotateLog(path string, maxBytes int64) error {
	if maxBytes < 0 {
		return fmt.Errorf("maint: rotate %q: negative maxBytes %d", path, maxBytes)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to rotate
		}
		return fmt.Errorf("maint: stat %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("maint: rotate %q: is a directory", path)
	}
	if info.Size() <= maxBytes {
		return nil // under the cap; leave it alone
	}

	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		return fmt.Errorf("maint: rotating %q to %q: %w", path, rotated, err)
	}

	// Recreate the live path empty so the writer keeps the same handle target.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("maint: recreating %q after rotate: %w", path, err)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("maint: closing recreated %q: %w", path, cerr)
	}
	return nil
}
