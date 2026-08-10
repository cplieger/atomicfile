package atomicfile

import (
	"log/slog"
	"os"
)

// cfg holds resolved configuration for an atomic write.
type cfg struct {
	logger             *slog.Logger
	maxBytes           int64
	mode               os.FileMode
	mkdirMode          os.FileMode
	preserveMode       bool
	preserveOwnership  bool
	noSync             bool
	allowSymlinkTarget bool
	recursive          bool
	repairOwnedDir     bool
}

// Option configures an atomic write operation.
type Option func(*cfg)

// WithLogger sets a custom logger. If not provided, slog.Default() is used.
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
}

// WithRepairOwnedDir lets EnsurePrivateDir repair a PRE-EXISTING directory's
// mode instead of refusing it with ErrModeTooOpen. It affects EnsurePrivateDir
// only and is ignored by every write entry point.
//
// It exists for the caller whose directory is its OWN past output. An app that
// created <root>/<key>/ at 0700 on a filesystem that stored 0770 will, on the
// next release, meet its own directory and be refused by the default rule —
// which is how a mode fix turns into an outage on upgrade, with every item
// failing at a directory the app itself made.
//
// Repairing such a directory is sound, and the reason is ownership rather than
// trust: mkdir(2) gives a new directory to its creator, a directory cannot be the
// target of a hard link, and the open is O_NOFOLLOW at the final component — so a
// directory that fstats as owned by the effective uid was made by this uid, not
// planted by a neighbour who merely writes the parent. EnsurePrivateDir has
// already established exactly that before this option is consulted, and every
// other refusal (ErrNotOwned, ErrNotDirectory, the symlink refusal) still fires
// first and is unaffected.
//
// It is deliberately NOT the default, because a wide mode on a directory you
// own can be a DELIBERATE choice rather than drift. seadex-scout's report
// directory is intentionally left wider so an operator can read reports out of
// it; a library that quietly narrowed it would break that on a version bump.
// Refusing keeps the default honest — the caller has to say "this directory is
// mine and its privacy is not negotiable" — and Repaired reports when the
// option actually changed something, so an app can log the one-time repair
// rather than discover it later.
func WithRepairOwnedDir() Option {
	return func(c *cfg) { c.repairOwnedDir = true }
}

// WithRecursive makes a stale-temp sweep descend into subdirectories. It affects
// CleanupStaleTemps and CleanupStaleTempsInRoot and is ignored by every write entry
// point.
//
// Off by default, and the default is the interesting half: a sweep is a DESTRUCTIVE
// operation, so how much of the filesystem it touches must be stated at the call site
// rather than inferred from which function was reached for. Both sweeps therefore behave
// identically without it — one directory, no descent — and depth is opt-in.
//
// Reach for it when temps can be staged below the directory you name. That happens
// whenever the caller's own output tree is nested, because a temp is always created in
// the SAME directory as its final target: a write to out/example.com/cert.pfx leaves its
// orphan in out/example.com, which a flat sweep of out/ never sees. A caller with a flat
// cache directory does not need it and should not pass it.
func WithRecursive() Option {
	return func(c *cfg) { c.recursive = true }
}

// WithMode sets the permission applied to the written file. Defaults to 0o644.
func WithMode(mode os.FileMode) Option {
	return func(c *cfg) { c.mode = mode }
}

// WithMkdirMode creates the parent directory (and any missing ancestors) with
// the given permission before writing. Without it, a missing parent directory
// is an error.
func WithMkdirMode(mode os.FileMode) Option {
	return func(c *cfg) { c.mkdirMode = mode }
}

// WithPreserveMode stats the target before writing and applies its existing
// mode to the new file, falling back to the WithMode value if the target does
// not exist or cannot be stat-ed (a non-ErrNotExist stat failure is logged at Warn).
func WithPreserveMode() Option {
	return func(c *cfg) { c.preserveMode = true }
}

// WithPreserveOwnership stats the target before writing and chowns the temp
// file to match the target's uid/gid. Requires CAP_CHOWN or root; a no-op when
// the target does not exist, cannot be stat-ed, or off Unix (a non-ErrNotExist stat
// failure is logged at Warn). Best-effort: the chown runs after the
// temp-file fsync, so unlike content and mode it is not crash-covered. A chown
// failure is logged at Warn and does not fail the write (the file lands with
// the writer's ownership).
func WithPreserveOwnership() Option {
	return func(c *cfg) { c.preserveOwnership = true }
}

// WithNoSync skips the fsync on both the temp file and the parent directory,
// providing atomicity without durability. Result.Durable is then always false.
func WithNoSync() Option {
	return func(c *cfg) { c.noSync = true }
}

// WithAllowSymlinkTarget permits writing to a path that is currently a symlink.
// By default symlink targets are refused with ErrSymlinkTarget. Note the atomic
// rename REPLACES the symlink with a regular file; it does not write through to
// the link's target. Resolve with filepath.EvalSymlinks first if that is the
// intent.
func WithAllowSymlinkTarget() Option {
	return func(c *cfg) { c.allowSymlinkTarget = true }
}

// WithMaxBytes caps the content size an atomic write will stage, the
// write-side mirror of ReadBounded's bound: a writer that also owns the
// reader can refuse to persist a file its own read path would refuse to
// load, instead of silently writing something that fails open on the next
// read. WriteFile and WriteFileInRoot reject over-cap content before the
// temp file is created; WriteReader and WriteReaderInRoot reject the copy
// chunk that would cross the cap; a PendingFile rejects the
// Write/WriteString/ReadFrom call that would cross it, whole, so the staged
// temp never holds an over-cap prefix (see PendingFile.Write). The barrier
// additionally verifies the staged file's actual size before publication, so
// bytes staged outside those interfaces (WriteAt, Write after Seek, a reopen
// of the temp by path) cannot publish an over-cap file. Every rejection
// matches ErrFileTooLarge and leaves the previous file at the target path
// intact. n <= 0 means no cap (the default).
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
