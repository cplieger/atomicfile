package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
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
