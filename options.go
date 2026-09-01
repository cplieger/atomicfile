package atomicfile

import (
	"log/slog"
	"os"
)

type cfg struct {
	logger         *slog.Logger
	maxBytes       int64
	mode           os.FileMode
	mkdirMode      os.FileMode
	recursive      bool
	repairOwnedDir bool
}

// Option configures an atomic write, a stale-temp sweep, or EnsurePrivateDir.
// Each option's doc names which operations it applies to; the rest ignore it.
type Option func(*cfg)

// WithLogger sets a custom logger. If not provided, slog.Default() is used.
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
}

// WithRepairOwnedDir controls whether EnsurePrivateDir repairs a PRE-EXISTING
// directory's mode instead of refusing it with ErrModeTooOpen. Affects
// EnsurePrivateDir only; every write entry point ignores it.
//
// Repair is sound because EnsurePrivateDir already established ownership (a
// directory that fstats as owned by the effective uid was made by this uid,
// not planted by a neighbour): repairing it just re-applies the caller's own
// mode after a filesystem widened it (e.g. an inheritable ACL). Every other
// refusal (ErrNotOwned, ErrNotDirectory, the symlink refusal) still fires
// first.
//
// Repair is NOT the default because a wide mode on an owned directory can be
// deliberate rather than drift (seadex-scout's report directory is
// intentionally left readable). Repaired reports whether a repair happened.
func WithRepairOwnedDir(repair bool) Option {
	return func(c *cfg) { c.repairOwnedDir = repair }
}

// WithRecursive controls whether a stale-temp sweep descends into
// subdirectories, or sweeps only the named directory. Affects
// CleanupStaleTemps and CleanupStaleTempsInRoot; every write entry point
// ignores it.
//
// Descent is not the default because a sweep is destructive, so how much of
// the tree it touches must be stated at the call site. Pass true when temps
// can be staged below the named directory — a temp is created in the SAME
// directory as its final target, so a write to out/example.com/cert.pfx
// leaves its orphan in out/example.com, invisible to a flat sweep of out/.
func WithRecursive(recursive bool) Option {
	return func(c *cfg) { c.recursive = recursive }
}

// WithMode sets the permission applied to the written file. Defaults to 0o644.
func WithMode(mode os.FileMode) Option {
	return func(c *cfg) { c.mode = mode }
}

// WithMkdirMode creates the parent directory (and any missing ancestors) with
// the given permission before writing; without it a missing parent is an
// error. Applies to every write entry point; sweeps and EnsurePrivateDir
// ignore it.
//
// The mode is ENFORCED on each created directory, not merely requested:
// mkdir(2) passes it through umask, and measured under umask 077 a requested
// 0o755 stored 0o700. A filesystem that won't store the mode fails the write
// with ErrModeNotStored. A PRE-EXISTING directory is never chmod'ed.
//
// Each created directory's parent is fsynced as it is made; Result.Durable is
// false if any of those fsyncs failed, but the write still succeeds.
func WithMkdirMode(mode os.FileMode) Option {
	return func(c *cfg) { c.mkdirMode = mode }
}

// WithMaxBytes caps the content size an atomic write will stage: the
// write-side mirror of ReadBounded's bound, so a writer that also owns the
// reader can refuse to persist a file its own read path would refuse to
// load. WriteFile/WriteFileInRoot reject over-cap content before the temp is
// created; WriteReader/WriteReaderInRoot reject the copy chunk that would
// cross the cap; a PendingFile rejects the Write/WriteString/ReadFrom call
// that would cross it, whole (see PendingFile.Write). The barrier also
// verifies the staged file's actual size before publication, so bytes staged
// outside those interfaces (WriteAt, Write after Seek) cannot publish an
// over-cap file. Every rejection matches ErrFileTooLarge and leaves the
// previous file at the target path intact. n <= 0 means no cap (the
// default).
func WithMaxBytes(n int64) Option {
	return func(c *cfg) { c.maxBytes = n }
}

func buildCfg(opts []Option) *cfg {
	c := &cfg{mode: 0o644}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}
