package maint

import (
	"fmt"
	"os"
	"strconv"
)

// RotateLog caps a log file at maxBytes. If path is at or below the limit (or does
// not exist), it is left alone. Otherwise the current file is rotated out to a
// numbered generation and a fresh, empty path is created in its place, so the active
// writer keeps appending to the same path while the bulky history is preserved.
//
// Rotation is LOSSLESS (I5 — the event log is append-only / replayable, so no prior
// generation may ever be destroyed). It uses classic logrotate-style numbering where
// ".1" is always the MOST RECENT rotation: before moving the live file to ".1", any
// existing generations are cascaded up (".1"→".2", ".2"→".3", …) from the highest
// down, so nothing is overwritten. A second (and Nth) rotation therefore keeps BOTH
// (all) generations instead of clobbering the previous ".1". Consumers that read only
// "<path>.1" still see the newest generation; consumers that want deeper history can
// walk ".2", ".3", … until the first missing suffix.
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

	// Cascade existing generations up so ".1" is free to receive the live file without
	// destroying the prior ".1". Find the highest occupied generation first, then shift
	// from the top down (".N"→".N+1", …, ".1"→".2") so no rename ever overwrites a
	// generation we have not yet moved.
	top := highestGeneration(path)
	for gen := top; gen >= 1; gen-- {
		from := generationPath(path, gen)
		to := generationPath(path, gen+1)
		if _, statErr := os.Stat(from); statErr != nil {
			continue // sparse gap; nothing to move at this level
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("maint: shifting %q to %q: %w", from, to, err)
		}
	}

	rotated := generationPath(path, 1)
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

// generationPath returns the on-disk name of the nth rotated generation of path
// ("<path>.1", "<path>.2", …). Generation 0 is the live path itself.
func generationPath(path string, gen int) string {
	if gen <= 0 {
		return path
	}
	return path + "." + strconv.Itoa(gen)
}

// highestGeneration returns the largest n for which "<path>.n" currently exists,
// scanning contiguously from 1 (the numbering RotateLog itself produces is dense —
// it only ever shifts existing generations up by one, never skips). It stops at the
// first missing suffix, so a hand-created sparse gap simply bounds the cascade there,
// which is safe: shifting only ever moves files into higher, unoccupied slots.
func highestGeneration(path string) int {
	top := 0
	for gen := 1; ; gen++ {
		if _, err := os.Stat(generationPath(path, gen)); err != nil {
			return top
		}
		top = gen
	}
}
