package atomicfile

import (
	"log/slog"
	"os"
)

// cfg holds resolved configuration for an atomic write.
type cfg struct {
	logger             *slog.Logger
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
// not exist.
func WithPreserveMode() Option {
	return func(c *cfg) { c.preserveMode = true }
}

// WithPreserveOwnership stats the target before writing and chowns the temp
// file to match the target's uid/gid. Requires CAP_CHOWN or root; a no-op when
// the target does not exist or off Unix. Best-effort: the chown runs after the
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
