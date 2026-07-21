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
}

// Option configures an atomic write operation.
type Option func(*cfg)

// WithLogger sets a custom logger. If not provided, slog.Default() is used.
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
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
// temp never holds an over-cap prefix (see PendingFile.Write). Every
// rejection matches ErrFileTooLarge and leaves the previous file at the
// target path intact. n <= 0 means no cap (the default).
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
