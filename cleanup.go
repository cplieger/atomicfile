package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tempPattern is the single os.CreateTemp pattern used for every temp file.
// The "*" is replaced with a decimal random string, so each temp matches
// tempPrefix + <digits> + tempSuffix exactly.
const (
	tempPattern = ".atomicfile-*.tmp"
	tempPrefix  = ".atomicfile-"
	tempSuffix  = ".tmp"
)

// isAllDigits reports whether s is non-empty and all ASCII decimal digits —
// the shape of os.CreateTemp's random suffix.
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
// requirement mirrors os.CreateTemp's decimal suffix exactly, so a caller-owned
// file that merely shares the prefix and suffix (e.g. ".atomicfile-notes.tmp")
// is never matched and never deleted.
func isStaleTempName(name string) bool {
	if !strings.HasPrefix(name, tempPrefix) || !strings.HasSuffix(name, tempSuffix) {
		return false
	}
	middle := name[len(tempPrefix) : len(name)-len(tempSuffix)]
	return isAllDigits(middle)
}

// CleanupStaleTemps removes temp files in dir that this package created
// (".atomicfile-<digits>.tmp") and whose mtime is older than maxAge. It returns
// the number removed. A missing dir is not an error. Best-effort per file:
// individual stat/remove failures are logged at Debug and skipped; only a
// readdir failure is returned.
func CleanupStaleTemps(dir string, maxAge time.Duration, opts ...Option) (removed int, err error) {
	c := buildCfg(opts)
	if maxAge <= 0 {
		c.logger.Warn("atomicfile.CleanupStaleTemps: non-positive maxAge; skipping cleanup",
			"dir", dir, "max_age", maxAge)
		return 0, nil
	}
	entries, rdErr := os.ReadDir(dir)
	if rdErr != nil {
		if errors.Is(rdErr, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, rdErr
	}
	cutoff := time.Now().Add(-maxAge)
	failed := 0
	for _, e := range entries {
		name := e.Name()
		if !isStaleTempName(name) {
			continue
		}
		info, infErr := e.Info()
		if infErr != nil {
			if !errors.Is(infErr, fs.ErrNotExist) {
				c.logger.Debug("atomicfile.CleanupStaleTemps: stat failed", "name", name, "error", infErr)
				failed++
			}
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(dir, name)
		if rmErr := os.Remove(full); rmErr != nil {
			if !errors.Is(rmErr, fs.ErrNotExist) {
				c.logger.Debug("atomicfile.CleanupStaleTemps: remove failed", "path", full, "error", rmErr)
				failed++
			}
			continue
		}
		removed++
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
