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
	// Path is the cleaned final path the data was written to. It is
	// absolute for the package-level write functions (which require an
	// absolute path); for WriteFileInRoot and WriteReaderInRoot it is
	// root.Name() joined with the cleaned relative name, so it is relative
	// when the *os.Root was opened with a relative path.
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
			c.logger.Warn("atomicfile: preserve-mode stat failed; using explicit mode",
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
			c.logger.Warn("atomicfile: preserve-ownership stat failed; keeping writer ownership",
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
			"target", target, "uid", stat.Uid, "gid", stat.Gid, "error", err)
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
			return nil, "", "", fmt.Errorf("atomicfile: create parent directory %q: %w", dir, mkErr)
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

// writeAtomic runs the open -> write -> finalize -> commit orchestration shared
// by WriteFile and WriteReader: open the temp file, capture its name under the
// committed-flag leak-cleanup defer, run the caller's writeData step, then hand
// off to the temp-side barrier and commit. writeData is the only varying part
// (the byte slice write vs the reader copy); a write error it returns is tagged
// PhaseTempWrite here so both entry points report failures identically. Keep in
// sync with PendingFile.commit, which reuses the same barrier/commit helpers but
// skips this open/write preamble (its file is already open and written).
func writeAtomic(ctx context.Context, path string, c *cfg, writeData func(*os.File) error) (Result, error) {
	tmp, cleanPath, dir, err := openTempForPath(ctx, path, c)
	if err != nil {
		return Result{}, err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			removeTemp(tmpName, c.logger)
		}
	}()
	mode := resolveMode(cleanPath, c)
	if wErr := writeData(tmp); wErr != nil {
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

// WriteFile atomically writes data to path. Mode defaults to 0o644 (override
// with WithMode). A nil error means the data is at path; check Result.Durable
// for crash durability.
func WriteFile(ctx context.Context, path string, data []byte, opts ...Option) (Result, error) {
	return writeAtomic(ctx, path, buildCfg(opts), func(tmp *os.File) error {
		_, err := tmp.Write(data)
		return err
	})
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
	if r == nil {
		return Result{}, errors.New("atomicfile: nil reader")
	}
	return writeAtomic(ctx, path, buildCfg(opts), func(tmp *os.File) error {
		if wt, ok := r.(io.WriterTo); ok {
			_, err := wt.WriteTo(writerCtx{ctx: ctx, w: tmp})
			return err
		}
		_, err := io.Copy(tmp, readerCtx{ctx: ctx, r: r})
		return err
	})
}

// pendingState tracks a PendingFile's terminal lifecycle. A single bool could
// not distinguish a committed file from a cleaned-up one, so a Commit after
// Cleanup replayed a zero-value Result with a nil error — a false success. The
// explicit states let Commit return ErrAborted after Cleanup instead. See
// ErrAborted.
type pendingState uint8

const (
	pendingOpen          pendingState = iota // not yet committed or cleaned up
	pendingCommitted                         // Commit was attempted; result/err are cached
	pendingCleaned                           // Cleanup removed the temp; Commit now fails
	pendingCleanupFailed                     // Cleanup closed the temp but removal failed; Commit still fails and Cleanup retries removal
)

// PendingFile is a temp file open for writing, destined to atomically replace
// path on Commit. It embeds *os.File, so callers get the full
// io.Writer/io.ReaderFrom/fmt.Fprintf surface. The configuration is captured at
// construction and reused at Commit, so durability options cannot drift between
// the two calls.
//
// A PendingFile is not safe for concurrent use: Commit and Cleanup mutate
// unsynchronized lifecycle state (state, result, err). Confine each PendingFile
// to a single goroutine. The package-level WriteFile, WriteReader, ReadBounded,
// and CleanupStaleTemps are stateless and safe to call concurrently on distinct
// paths.
type PendingFile struct {
	*os.File
	cfg    *cfg
	err    error
	path   string
	dir    string
	result Result
	mode   os.FileMode
	state  pendingState
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
// directory. Commit is idempotent: repeated calls return the first result.
// Calling Commit after Cleanup returns ErrAborted — the temp was already
// removed, so there is nothing to commit, and a nil error would falsely signal
// that the data reached its final path. After a successful Commit, Cleanup is a
// no-op. ctx is checked through the temp-side barrier (before chmod, around the
// fsync, and before close); a context cancelled at or before close aborts and
// removes the temp. Once the barrier passes, ownership and the rename run
// without a further ctx check, so a context cancelled in the narrow window
// between close and rename still commits.
func (p *PendingFile) Commit(ctx context.Context) (Result, error) {
	switch p.state {
	case pendingCommitted:
		return p.result, p.err
	case pendingCleaned, pendingCleanupFailed:
		return Result{}, ErrAborted
	}
	p.state = pendingCommitted
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

// Cleanup closes and removes the temp file, aborting the pending write. It is a
// no-op once Commit has been called — a successful Commit already moved the data
// into place, and a failed Commit already removed the temp. After a successful
// Cleanup, a subsequent Commit returns ErrAborted.
//
// If the temp removal fails (for a reason other than the file already being
// gone), Cleanup returns that error WITHOUT marking the write cleaned, so a
// later Cleanup retries the removal — a failed Cleanup never falsely reports the
// temp gone, and a subsequent Commit still returns ErrAborted. Repeated Cleanup
// calls are therefore a no-op after success but a retry after a removal failure.
// Safe to defer immediately after NewPendingFile.
func (p *PendingFile) Cleanup() error {
	switch p.state {
	case pendingCommitted, pendingCleaned:
		return nil
	case pendingCleanupFailed:
		if err := os.Remove(p.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		p.state = pendingCleaned
		return nil
	}
	// pendingOpen: first cleanup attempt. Close the fd, then remove the temp.
	tmpName := p.Name()
	if clErr := p.Close(); clErr != nil {
		p.cfg.logger.Debug("atomicfile: pending file close during cleanup failed",
			"path", tmpName, "error", clErr)
	}
	if err := os.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.state = pendingCleanupFailed
		return err
	}
	p.state = pendingCleaned
	return nil
}
