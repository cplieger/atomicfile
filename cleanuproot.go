package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// SweepResult is the per-outcome accounting of one CleanupStaleTempsInRoot walk.
//
// Failed and Unreadable are separate because they are different operator problems.
// Failed means a temp was found and could not be reclaimed (permissions on the file,
// or an IO error) — orphans are accumulating. Unreadable means a subdirectory could
// not be entered at all, so it may be hiding orphans nobody has counted. A caller
// that folds them together cannot tell an operator which one to go fix.
type SweepResult struct {
	// Removed counts temps reclaimed.
	Removed int
	// Failed counts candidates that could not be inspected or unlinked.
	Failed int
	// Unreadable counts sub-paths the walk could not enter.
	Unreadable int
}

// CleanupStaleTempsInRoot removes temp files orphaned by an interrupted atomic
// write, confined to root's tree.
//
// It sweeps ONE directory by default, exactly as CleanupStaleTemps does; pass
// WithRecursive to descend. The two functions differ only in the properties named
// below, never in how much of the filesystem they touch — that is the caller's
// explicit choice for both, because the operation deletes files.
//
// Confinement. Every stat and unlink goes through the *os.Root, so a symlink planted
// under the swept tree cannot redirect a removal outside it. The ambient variant
// builds paths with filepath.Join and calls os.Lstat/os.Remove on them, which is
// safe for a directory the caller owns outright and not safe for one a co-mounting
// writer can modify underneath — reconstructing an ambient path and handing it to
// CleanupStaleTemps reintroduces exactly that TOCTOU window.
//
// Cancellation. The walk stops between entries once ctx is done, so a sweep of a
// large tree cannot hold up shutdown, and an already-cancelled context does no work.
// A cancelled sweep returns ctx.Err() alongside the counts accumulated so far.
//
// Unlike CleanupStaleTemps this reports counts instead of logging aggregates: it
// returns every number a caller needs to narrate the outcome itself, so emitting its
// own summary would only duplicate that in a second voice. Per-entry diagnostics
// (which path, which errno) still go to the configured logger at Debug, since those
// are details the counts cannot carry.
//
// maxAge must be positive; a non-positive value skips the sweep with a warning
// rather than reaping everything. Only names matching this package's own temp shape
// are candidates, so a caller-owned file is never removed.
//
// The caller owns root; CleanupStaleTempsInRoot does not close it.
func CleanupStaleTempsInRoot(ctx context.Context, root *os.Root, maxAge time.Duration, opts ...Option) (SweepResult, error) {
	c := buildCfg(opts)
	if root == nil {
		return SweepResult{}, errors.New("atomicfile: nil root")
	}
	if maxAge <= 0 {
		c.logger.Warn("atomicfile.CleanupStaleTempsInRoot: non-positive maxAge; skipping cleanup",
			"max_age", maxAge)
		return SweepResult{}, nil
	}

	s := &rootSweep{
		root:      root,
		cutoff:    time.Now().Add(-maxAge),
		logger:    c.logger,
		ctx:       ctx,
		recursive: c.recursive,
	}
	if walkErr := fs.WalkDir(root.FS(), ".", s.visit); walkErr != nil {
		return s.result, walkErr
	}
	return s.result, nil
}

// rootSweep carries the mutable accounting of one CleanupStaleTempsInRoot walk, so
// the WalkDir callback is a named method rather than a closure over five variables.
type rootSweep struct {
	ctx       context.Context
	root      *os.Root
	logger    *slog.Logger
	cutoff    time.Time
	result    SweepResult
	recursive bool
}

// visit is the WalkDir callback. A done context aborts the sweep between entries. A
// walk error at the root aborts it too — the tree itself is unusable — while an
// error below the root is counted as an unreadable sub-path and skipped, because a
// partial sweep is strictly better than none and the count tells the caller it
// happened.
func (s *rootSweep) visit(rel string, d fs.DirEntry, walkErr error) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if walkErr != nil {
		if rel == "." {
			return walkErr
		}
		s.logger.Debug("atomicfile.CleanupStaleTempsInRoot: skipping unreadable path",
			"path", rel, "error", walkErr)
		s.result.Unreadable++
		return nil
	}
	if d.IsDir() {
		if !s.recursive && rel != "." {
			// Without WithRecursive the sweep is one directory deep, matching
			// CleanupStaleTemps. SkipDir prunes the subtree rather than visiting and
			// discarding every entry in it, so a flat sweep of a directory holding a
			// large tree stays cheap.
			return fs.SkipDir
		}
		return nil
	}
	didRemove, didFail := reapStaleTempInRoot(s.root, rel, d.Name(), s.cutoff, s.logger)
	if didRemove {
		s.result.Removed++
	}
	if didFail {
		s.result.Failed++
	}
	return nil
}

// reapStaleTempInRoot is reapStaleTemp with every filesystem touch routed through
// root. rel is the root-relative path; base is its final element, which is what the
// temp-name shape is matched against.
//
// The re-stat before removal is not redundant. The walk's DirEntry came from a
// readdir that may be several entries stale, so the name could since have become a
// directory or a symlink; Lstat through the root re-establishes that this is still a
// regular file immediately before unlinking it, and never follows a symlink left
// under a temp's name.
func reapStaleTempInRoot(root *os.Root, rel, base string, cutoff time.Time, logger *slog.Logger) (didRemove, didFail bool) {
	if !isStaleTempName(base) {
		return false, false
	}
	info, statErr := root.Lstat(rel)
	if statErr != nil {
		logger.Debug("atomicfile.CleanupStaleTempsInRoot: stat failed", "path", rel, "error", statErr)
		if errors.Is(statErr, fs.ErrNotExist) {
			// Vanished between the readdir and the Lstat: a co-mounting reaper, or
			// the write it belonged to completing its rename. Benign, as below.
			return false, false
		}
		// A permission or IO error means orphans may be accumulating unnoticed, so
		// it has to reach the caller's aggregate rather than only the Debug log.
		return false, true
	}
	if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
		return false, false
	}
	if rmErr := root.Remove(rel); rmErr != nil {
		if errors.Is(rmErr, fs.ErrNotExist) {
			return false, false
		}
		logger.Debug("atomicfile.CleanupStaleTempsInRoot: remove failed", "path", rel, "error", rmErr)
		return false, true
	}
	return true, false
}
