// Package atomicfile provides crash-safe atomic file writes via the
// temp → fsync → rename → dir-fsync sequence, with path validation and
// bounded reads. Standard-library only.
//
// # Result and durability
//
// The write primitives (WriteFile, WriteReader, and PendingFile.Commit)
// return a Result alongside an error. A nil error means the data reached its
// final path; the write either fully succeeded or, at worst, was renamed into
// place but the parent-directory fsync failed. Result.Durable distinguishes
// those two outcomes: it is true only when both the file and its parent
// directory were fsynced, so a caller that cares about crash durability
// inspects Result.Durable rather than decoding error types. A non-nil error
// always means the data did NOT reach its final path.
//
// # Temp files
//
// Every temp file this package creates is named ".atomicfile-<digits>.tmp"
// (os.CreateTemp replaces the pattern's "*" with a decimal random string).
// CleanupStaleTemps reclaims orphaned temps of exactly that shape and nothing
// else, so it never deletes a caller-owned file.
package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// Sentinel errors.
var (
	// ErrEmptyPath is returned when a path argument is empty.
	ErrEmptyPath = errors.New("atomicfile: empty path")
	// ErrUnsafePath is returned when a path fails the local safety check.
	ErrUnsafePath = errors.New("atomicfile: unsafe path")
	// ErrFileTooLarge is returned when a file exceeds the read size limit.
	ErrFileTooLarge = errors.New("atomicfile: file too large")
	// ErrSymlinkTarget is returned when the target path is a symlink and
	// WithAllowSymlinkTarget was not set.
	ErrSymlinkTarget = errors.New("atomicfile: target is a symlink")
)

// Result reports the outcome of an atomic write that reached its final path.
type Result struct {
	// Path is the cleaned, absolute final path the data was written to.
	Path string
	// Durable reports whether the write is guaranteed to survive a crash:
	// true only when the file and its parent directory were both fsynced.
	// It is false when WithNoSync was set, or when the parent-directory fsync
	// failed after the rename — in that case the data is present at Path but
	// may not survive an immediate power loss, and the fsync failure is logged
	// at Warn.
	Durable bool
}

// WritePhase identifies which step of an atomic write failed. Each value
// appears only on a WriteError, which is returned exclusively for hard failures
// (the data did not reach its final path). Two outcomes are deliberately absent
// from this enum because they are not hard failures: a parent-directory fsync
// failure (surfaced via Result.Durable) and a preserve-ownership chown failure
// (best-effort, logged at Warn).
type WritePhase int

const (
	// PhaseTempCreate indicates failure creating the temp file.
	PhaseTempCreate WritePhase = iota + 1
	// PhaseTempWrite indicates failure writing to the temp file.
	PhaseTempWrite
	// PhaseTempChmod indicates failure setting permissions on the temp file.
	PhaseTempChmod
	// PhaseTempSync indicates failure syncing the temp file.
	PhaseTempSync
	// PhaseTempClose indicates failure closing the temp file.
	PhaseTempClose
	// PhaseRename indicates failure renaming temp to the final path.
	PhaseRename
)

func (p WritePhase) String() string {
	switch p {
	case PhaseTempCreate:
		return "create temp file"
	case PhaseTempWrite:
		return "write temp file"
	case PhaseTempChmod:
		return "chmod temp file"
	case PhaseTempSync:
		return "sync temp file"
	case PhaseTempClose:
		return "close temp file"
	case PhaseRename:
		return "rename to final path"
	default:
		return "unknown phase"
	}
}

// WriteError wraps a hard atomic-write failure with the phase that failed.
// A WriteError always means the data did NOT reach its final path.
type WriteError struct {
	Err   error
	Phase WritePhase
}

func (e *WriteError) Error() string { return "atomicfile: " + e.Phase.String() + ": " + e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

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

// validateAbsClean checks that path is non-empty, free of null bytes and ".."
// traversal, and absolute, returning the cleaned form.
func validateAbsClean(path string) (string, error) {
	if path == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: contains null byte", ErrUnsafePath)
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: not absolute: %q", ErrUnsafePath, path)
	}
	if strings.Contains(clean, ".."+string(filepath.Separator)) ||
		strings.HasSuffix(clean, string(filepath.Separator)+"..") ||
		clean == ".." {
		return "", fmt.Errorf("%w: contains traversal: %q", ErrUnsafePath, path)
	}
	return clean, nil
}

// checkSymlink returns ErrSymlinkTarget if target is a symlink and
// WithAllowSymlinkTarget was not set. Best-effort: the Lstat precedes the
// eventual os.Rename, so an attacker who can write the parent directory may
// create a symlink afterward. Because os.Rename does not follow the final
// component, the worst case is replacing the attacker's symlink rather than
// writing through it.
func checkSymlink(target string, c *cfg) error {
	if c.allowSymlinkTarget {
		return nil
	}
	fi, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("atomicfile: stat target %q: %w", target, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkTarget, target)
	}
	return nil
}

// checkWritePath runs the shared pre-write path-safety preamble: validate and
// clean path, honor ctx cancellation, and refuse symlink targets. It is the
// single source of truth for the write-side path-safety contract.
func checkWritePath(ctx context.Context, path string, c *cfg) (string, error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return "", err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("atomicfile: %w", ctxErr)
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return "", symErr
	}
	return cleanPath, nil
}

// resolveMode determines the mode to apply, honoring WithPreserveMode.
func resolveMode(target string, c *cfg) os.FileMode {
	if c.preserveMode {
		fi, err := os.Stat(target)
		if err == nil {
			return fi.Mode().Perm()
		}
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Debug("atomicfile: preserve-mode stat failed; using explicit mode",
				"target", target, "error", err)
		}
	}
	return c.mode
}

// osChown changes the ownership of a file. It is a package var so tests can
// inject a chown failure; a real EPERM is impractical to force from a
// same-owner test (and root cannot fail it at all). Production never reassigns it.
var osChown = os.Chown

// applyOwnership chowns tmpName to match target's uid/gid when
// WithPreserveOwnership is set. No-op when the target is absent or the
// platform's FileInfo.Sys() is not a *syscall.Stat_t (e.g. non-Unix).
// Best-effort: a failed chown is logged at Warn and the write proceeds with
// the writer's ownership; it never aborts the write.
func applyOwnership(tmpName, target string, c *cfg) {
	if !c.preserveOwnership {
		return
	}
	fi, err := os.Stat(target)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Debug("atomicfile: preserve-ownership stat failed; keeping writer ownership",
				"target", target, "error", err)
		}
		return
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	if err := osChown(tmpName, int(stat.Uid), int(stat.Gid)); err != nil {
		c.logger.Warn("atomicfile: preserve-ownership chown failed; keeping writer ownership",
			"target", target, "error", err)
	}
}

// removeTemp deletes a temp file best-effort, logging at Debug when removal
// fails for a reason other than the file already being gone.
func removeTemp(tmpName string, logger *slog.Logger) {
	if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		logger.Debug("atomicfile: temp file cleanup failed", "path", tmpName, "error", rmErr)
	}
}

// openTempForPath runs the pre-barrier preamble shared by every write entry
// point: validate path, honor ctx, refuse symlinks, optionally create the
// parent directory, and create the temp file. It is the single place that
// enforces the path-safety contract; add new pre-write checks here.
func openTempForPath(ctx context.Context, path string, c *cfg) (tmp *os.File, cleanPath, dir string, err error) {
	cleanPath, err = checkWritePath(ctx, path, c)
	if err != nil {
		return nil, "", "", err
	}
	dir = filepath.Dir(cleanPath)
	if c.mkdirMode != 0 {
		if mkErr := os.MkdirAll(dir, c.mkdirMode); mkErr != nil {
			return nil, "", "", mkErr
		}
	}
	tmp, err = os.CreateTemp(dir, tempPattern)
	if err != nil {
		return nil, "", "", &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	return tmp, cleanPath, dir, nil
}

// finalizeTempFile runs the temp-side durability barrier on an open temp file
// that already holds its content: chmod, optional fsync (skipped under
// WithNoSync), then close. A ctx check brackets the fsync on both sides so a
// cancelled context aborts before and after the most expensive step. On any
// error before close it closes the file and returns; the caller's deferred
// cleanup removes the temp. This is the single source of truth for the
// temp-side barrier — a barrier change must land here and nowhere else.
func finalizeTempFile(ctx context.Context, tmp *os.File, mode os.FileMode, noSync bool) error {
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if !noSync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return &WriteError{Phase: PhaseTempSync, Err: err}
		}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return &WriteError{Phase: PhaseTempClose, Err: err}
	}
	return nil
}

// fsyncDir fsyncs a directory so a prior rename survives a crash. It is a
// package var so tests can inject a failure; a real fsync(2) on a directory fd
// is impractical to force on a healthy filesystem. Production never reassigns it.
var fsyncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// commitTemp finalizes a synced, closed temp file: apply ownership, atomically
// rename to cleanPath, then fsync the parent directory. It returns whether the
// write is durable. A pre-rename failure removes the temp and returns an error
// (the data did not land). Once the rename succeeds the data is at cleanPath;
// a subsequent parent-dir fsync failure is logged at Warn and reported as
// durable=false with a nil error, never as a hard failure.
func commitTemp(tmpName, cleanPath, dir string, c *cfg) (durable bool, err error) {
	applyOwnership(tmpName, cleanPath, c)
	if rnErr := os.Rename(tmpName, cleanPath); rnErr != nil {
		removeTemp(tmpName, c.logger)
		return false, &WriteError{Phase: PhaseRename, Err: rnErr}
	}
	if c.noSync {
		return false, nil
	}
	if syncErr := fsyncDir(dir); syncErr != nil {
		c.logger.Warn("atomicfile: parent-directory fsync failed; write is not durable",
			"path", cleanPath, "error", syncErr)
		return false, nil
	}
	return true, nil
}

// WriteFile atomically writes data to path. Mode defaults to 0o644 (override
// with WithMode). A nil error means the data is at path; check Result.Durable
// for crash durability.
func WriteFile(ctx context.Context, path string, data []byte, opts ...Option) (Result, error) {
	c := buildCfg(opts)
	tmp, cleanPath, dir, err := openTempForPath(ctx, path, c)
	if err != nil {
		return Result{}, err
	}
	mode := resolveMode(cleanPath, c)
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			removeTemp(tmpName, c.logger)
		}
	}()
	if _, wErr := tmp.Write(data); wErr != nil {
		tmp.Close()
		return Result{}, &WriteError{Phase: PhaseTempWrite, Err: wErr}
	}
	if fErr := finalizeTempFile(ctx, tmp, mode, c.noSync); fErr != nil {
		return Result{}, fErr
	}
	committed = true
	durable, cErr := commitTemp(tmpName, cleanPath, dir, c)
	if cErr != nil {
		return Result{}, cErr
	}
	return Result{Path: cleanPath, Durable: durable}, nil
}

// readerCtx wraps an io.Reader so each Read observes ctx cancellation, making
// an in-flight io.Copy interruptible. Sources implementing io.WriterTo bypass
// Read; WriteReader wraps the destination with writerCtx to cover that path.
type readerCtx struct {
	ctx context.Context
	r   io.Reader
}

func (rc readerCtx) Read(p []byte) (int, error) {
	if err := rc.ctx.Err(); err != nil {
		return 0, err
	}
	return rc.r.Read(p)
}

// writerCtx wraps an io.Writer so each Write observes ctx cancellation,
// restoring per-chunk cancellation on the io.WriterTo fast path. A single-shot
// WriterTo source that issues one Write still cannot be interrupted mid-write.
type writerCtx struct {
	ctx context.Context
	w   io.Writer
}

func (wc writerCtx) Write(p []byte) (int, error) {
	if err := wc.ctx.Err(); err != nil {
		return 0, err
	}
	return wc.w.Write(p)
}

// WriteReader atomically writes the contents of r to path. Mode defaults to
// 0o644 (override with WithMode). If r implements io.WriterTo it is used for
// efficient copying; that fast path bypasses the per-Read context check, so
// cancellation is coarse (per-chunk for chunked sources, post-copy for
// single-shot sources). ctx is still honored at the durability barrier, so a
// cancelled write leaves no partial target.
func WriteReader(ctx context.Context, path string, r io.Reader, opts ...Option) (Result, error) {
	c := buildCfg(opts)
	tmp, cleanPath, dir, err := openTempForPath(ctx, path, c)
	if err != nil {
		return Result{}, err
	}
	mode := resolveMode(cleanPath, c)
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			removeTemp(tmpName, c.logger)
		}
	}()
	var writeErr error
	if wt, ok := r.(io.WriterTo); ok {
		_, writeErr = wt.WriteTo(writerCtx{ctx: ctx, w: tmp})
	} else {
		_, writeErr = io.Copy(tmp, readerCtx{ctx: ctx, r: r})
	}
	if writeErr != nil {
		tmp.Close()
		return Result{}, &WriteError{Phase: PhaseTempWrite, Err: writeErr}
	}
	if fErr := finalizeTempFile(ctx, tmp, mode, c.noSync); fErr != nil {
		return Result{}, fErr
	}
	committed = true
	durable, cErr := commitTemp(tmpName, cleanPath, dir, c)
	if cErr != nil {
		return Result{}, cErr
	}
	return Result{Path: cleanPath, Durable: durable}, nil
}

// PendingFile is a temp file open for writing, destined to atomically replace
// path on Commit. It embeds *os.File, so callers get the full
// io.Writer/io.ReaderFrom/fmt.Fprintf surface. The configuration is captured at
// construction and reused at Commit, so durability options cannot drift between
// the two calls.
type PendingFile struct {
	*os.File
	cfg    *cfg
	err    error
	path   string
	dir    string
	result Result
	mode   os.FileMode
	done   bool
}

// NewPendingFile creates a temp file destined to atomically replace path. Write
// to it, then call Commit to finalize or Cleanup to abort. Mode defaults to
// 0o644 (override with WithMode). ctx is checked before the temp is created.
func NewPendingFile(ctx context.Context, path string, opts ...Option) (*PendingFile, error) {
	c := buildCfg(opts)
	tmp, cleanPath, dir, err := openTempForPath(ctx, path, c)
	if err != nil {
		return nil, err
	}
	return &PendingFile{
		File: tmp,
		cfg:  c,
		path: cleanPath,
		dir:  dir,
		mode: resolveMode(cleanPath, c),
	}, nil
}

// Commit runs the durability barrier (chmod, optional fsync, close), applies
// ownership, atomically renames the temp into place, and fsyncs the parent
// directory. After Commit, Cleanup is a no-op. Commit is idempotent: repeated
// calls return the first result. ctx is checked through the temp-side
// barrier (before chmod, around the fsync, and before close); a context
// cancelled at or before close aborts and removes the temp. Once the barrier
// passes, ownership and the rename run without a further ctx check, so a
// context cancelled in the narrow window between close and rename still
// commits.
func (p *PendingFile) Commit(ctx context.Context) (Result, error) {
	if p.done {
		return p.result, p.err
	}
	p.done = true
	p.result, p.err = p.commit(ctx)
	return p.result, p.err
}

func (p *PendingFile) commit(ctx context.Context) (Result, error) {
	tmpName := p.Name()
	committed := false
	defer func() {
		if !committed {
			removeTemp(tmpName, p.cfg.logger)
		}
	}()
	if fErr := finalizeTempFile(ctx, p.File, p.mode, p.cfg.noSync); fErr != nil {
		return Result{}, fErr
	}
	committed = true
	durable, cErr := commitTemp(tmpName, p.path, p.dir, p.cfg)
	if cErr != nil {
		return Result{}, cErr
	}
	return Result{Path: p.path, Durable: durable}, nil
}

// Cleanup closes and removes the temp file. It is a no-op once Commit has been
// called. Safe to defer immediately after NewPendingFile.
func (p *PendingFile) Cleanup() error {
	if p.done {
		return nil
	}
	p.done = true
	tmpName := p.Name()
	p.Close()
	if err := os.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// saturateAdd returns a + b clamped to math.MaxInt64 on overflow.
func saturateAdd(a, b int64) int64 {
	sum := a + b
	if sum < a {
		return math.MaxInt64
	}
	return sum
}

// ReadBounded opens path, validates its size against maxBytes, and reads it via
// an io.LimitReader. Returns ErrFileTooLarge if the file exceeds maxBytes
// (including if it grows past the limit during the read). ctx is checked before
// the open and before the read.
//
// Unlike the write primitives, ReadBounded does NOT refuse symlink targets:
// os.Open follows symlinks, so a symlink at path is resolved and its target is
// read. Callers reading from a directory writable by a less-trusted principal
// should resolve and confine the path themselves (e.g. filepath.EvalSymlinks
// plus a root check) before calling.
func ReadBounded(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	f, err := os.Open(cleanPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, fi.Size(), maxBytes)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	data, err := io.ReadAll(io.LimitReader(f, saturateAdd(maxBytes, 1)))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: file grew past %d byte limit during read", ErrFileTooLarge, maxBytes)
	}
	return data, nil
}

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
