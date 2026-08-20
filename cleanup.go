package atomicfile

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tempPrefix and tempSuffix bound the single temp-name shape used for every
// temp file this package creates: tempPrefix + <decimal digits> + tempSuffix
// (the digits come from randomTempName's crypto/rand draw). CleanupStaleTemps
// reclaims exactly that shape and nothing else.
const (
	tempPrefix = ".atomicfile-"
	tempSuffix = ".tmp"
)

// isAllDigits reports whether s is non-empty and all ASCII decimal digits —
// the shape of randomTempName's random middle.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isStaleTempName reports whether name is one this package created:
// ".atomicfile-<digits>.tmp", with a non-empty all-digit middle. The digit
// requirement mirrors randomTempName's decimal middle exactly, so a
// caller-owned file that merely shares the prefix and suffix (e.g.
// ".atomicfile-notes.tmp") is never matched and never deleted.
//
// CutPrefix and CutSuffix rather than HasPrefix/HasSuffix plus a hand-computed
// slice: the arithmetic form was correct, but only because the prefix and suffix
// cannot overlap, so a reader had to prove that before believing the bounds.
// Cutting removes the argument along with the arithmetic.
func isStaleTempName(name string) bool {
	middle, ok := strings.CutPrefix(name, tempPrefix)
	if !ok {
		return false
	}
	middle, ok = strings.CutSuffix(middle, tempSuffix)
	if !ok {
		return false
	}
	return isAllDigits(middle)
}

// reapStaleTemp processes a single directory entry for CleanupStaleTemps. It
// removes e when e is a stale package temp (".atomicfile-<digits>.tmp", a
// regular file older than cutoff) and reports the outcome: didRemove is true
// when the file was unlinked, didFail is true when a stat or remove failed for
// a reason other than the file already being gone (logged at Debug). At most
// one of the two is ever true; a non-matching, non-regular, or too-recent entry
// returns (false, false).
//
// The reap decision comes from a fresh os.Lstat taken immediately before the
// unlink, NOT from the readdir-cached e.Info(): if the name was replaced
// between the directory scan and this entry's turn — e.g. an in-flight write
// created a fresh temp under the same random name — the fresh mtime fails the
// age gate and the entry is spared. POSIX offers no conditional unlink, so the
// lstat→remove window cannot be closed entirely; keeping the two calls
// adjacent makes it as small as portably possible.
//
// The package only ever makes regular-file temps, so a directory or symlink
// that merely shares the temp-name shape is skipped via the same fresh lstat:
// never reclaim it.
func reapStaleTemp(dir string, e os.DirEntry, cutoff time.Time, logger *slog.Logger) (didRemove, didFail bool) {
	name := e.Name()
	if !isStaleTempName(name) {
		return false, false
	}
	full := filepath.Join(dir, name)
	info, statErr := os.Lstat(full)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return false, false
		}
		logger.Debug("atomicfile.CleanupStaleTemps: stat failed", "name", name, "error", statErr)
		return false, true
	}
	if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
		return false, false
	}
	if rmErr := os.Remove(full); rmErr != nil {
		if errors.Is(rmErr, fs.ErrNotExist) {
			return false, false
		}
		logger.Debug("atomicfile.CleanupStaleTemps: remove failed", "path", full, "error", rmErr)
		return false, true
	}
	return true, false
}

// reapDir drains an open directory handle in batches, invoking
// reapStaleTemp on each entry, and returns the count removed and the
// count that failed. io.EOF is the normal terminal signal, not an
// error; any other readdir error is returned.
func reapDir(d *os.File, dir string, cutoff time.Time, logger *slog.Logger) (removed, failed int, err error) {
	for {
		entries, readErr := d.ReadDir(128)
		for _, e := range entries {
			didRemove, didFail := reapStaleTemp(dir, e, cutoff, logger)
			if didRemove {
				removed++
			}
			if didFail {
				failed++
			}
		}
		if errors.Is(readErr, io.EOF) {
			return removed, failed, nil
		}
		if readErr != nil {
			return removed, failed, readErr
		}
	}
}

// CleanupStaleTemps removes temp files in dir that this package created
// (".atomicfile-<digits>.tmp") and whose mtime is older than maxAge. It returns
// the number removed. A missing dir is not an error. Best-effort per file:
// individual stat/remove failures are logged at Debug and skipped (see
// reapStaleTemp); only a readdir failure is returned.
//
// # Choosing maxAge
//
// maxAge must exceed the longest time any CONCURRENT writer may hold a temp,
// because a sweep cannot tell an orphan from a write in progress — POSIX offers
// no way to ask. The gate is the temp's mtime, not its creation time, so a
// streaming write that keeps producing bytes keeps refreshing it and is safe at
// any duration; what is at risk is a temp nothing has written to for maxAge. The
// realistic case is a PendingFile: a caller that stages, then does slow work,
// then Commits. _Measured_: with a temp backdated past a one-hour maxAge, the
// sweep unlinked it and the subsequent Commit failed with
// *WriteError{PhaseRename} wrapping ENOENT. No data is lost (the previous file at
// the target is untouched) but that write is.
//
// A non-positive maxAge skips the sweep with a Warn rather than reaping
// everything, so a zero value from an unset config cannot empty a directory.
func CleanupStaleTemps(dir string, maxAge time.Duration, opts ...Option) (removed int, err error) {
	c := buildCfg(opts)
	if maxAge <= 0 {
		c.logger.Warn("atomicfile.CleanupStaleTemps: non-positive maxAge; skipping cleanup",
			"dir", dir, "max_age", maxAge)
		return 0, nil
	}
	d, rdErr := os.Open(dir)
	if rdErr != nil {
		if errors.Is(rdErr, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, rdErr
	}
	defer d.Close()

	cutoff := time.Now().Add(-maxAge)
	var failed int
	if c.recursive {
		removed, failed, err = reapTree(dir, cutoff, c.logger)
	} else {
		removed, failed, err = reapDir(d, dir, cutoff, c.logger)
	}
	if err != nil {
		return removed, err
	}
	if removed > 0 {
		c.logger.Info("atomicfile.CleanupStaleTemps: removed stale temps", "dir", dir, "count", removed)
	}
	if failed > 0 {
		c.logger.Warn("atomicfile.CleanupStaleTemps: some stale temps could not be removed",
			"dir", dir, "failed", failed, "removed", removed)
	}
	return removed, nil
}

// reapTree is reapDir extended over subdirectories, for WithRecursive.
//
// It exists so both sweeps expose the same depth choice: a caller whose output tree is
// nested needs every level swept, because a temp is staged in the SAME directory as its
// final target, and hand-rolling that walk beside a single-directory library call is what
// leaves an orphan in a subdirectory nobody swept.
//
// A directory it cannot enter is counted as a failure and skipped rather than aborting
// the walk: a partial sweep is strictly better than none, and the count reaches the
// caller's warning. Every path stays ambient here, matching this function's siblings —
// a caller that needs the removals confined against a co-mounting writer wants
// CleanupStaleTempsInRoot instead.
func reapTree(dir string, cutoff time.Time, logger *slog.Logger) (removed, failed int, err error) {
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == dir {
				return walkErr
			}
			logger.Debug("atomicfile.CleanupStaleTemps: skipping unreadable path", "path", path, "error", walkErr)
			failed++
			return nil
		}
		if d.IsDir() {
			return nil
		}
		didRemove, didFail := reapStaleTemp(filepath.Dir(path), d, cutoff, logger)
		if didRemove {
			removed++
		}
		if didFail {
			failed++
		}
		return nil
	})
	return removed, failed, walkErr
}
