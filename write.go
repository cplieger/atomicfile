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
	// absolute for the package-level write functions; for the *InRoot
	// functions it is root.Name() joined with the cleaned relative name.
	Path string
	// Durable reports whether the write is guaranteed to survive a crash:
	// true only when the file and every directory its path depends on were
	// fsynced. False means the data is present at Path but may not survive
	// an immediate power loss, and the failure is logged at Warn.
	Durable bool
}

// openParentRoot validates and cleans path, honors ctx, optionally creates
// the parent directory chain, and opens the parent directory as an *os.Root
// so every subsequent operation runs through the root-confined engine
// (openTempForRoot / writeAtomicInRoot / commitTempInRoot).
//
// An OpenRoot failure is tagged PhaseTempCreate, the same class a
// temp-creation failure surfaces. The caller owns the returned root and must
// close it.
//
// dirSyncFailed reports that a directory the mkdir step created could not be
// fsynced into its own parent (see mkdirAllInRoot); the engine folds this
// into Result.Durable.
func openParentRoot(ctx context.Context, path string, c *cfg) (root *os.Root, base string, dirSyncFailed bool, err error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return nil, "", false, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, "", false, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	dir := filepath.Dir(cleanPath)
	if c.mkdirMode != 0 {
		failed, mkErr := mkdirAllAbs(dir, c.mkdirMode, c.logger)
		if mkErr != nil {
			return nil, "", false, fmt.Errorf("atomicfile: create parent directory %q: %w", dir, mkErr)
		}
		dirSyncFailed = failed
	}
	root, err = os.OpenRoot(dir)
	if err != nil {
		return nil, "", false, &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	return root, filepath.Base(cleanPath), dirSyncFailed, nil
}

// mkdirAllAbs creates the absolute directory dir and every missing ancestor
// at mode, running the creation inside an *os.Root opened on the deepest
// ancestor that already exists.
//
// os.MkdirAll cannot serve here: it reports only success, and the durability
// fix needs to know which levels are new. Ancestors that already existed
// when deepestExistingDir ran are resolved ambiently (symlinks included);
// every level BELOW that point runs through the root, so a name found
// missing that a racing writer then plants as an escaping symlink is
// refused instead of followed.
//
// dirSyncFailed mirrors mkdirAllInRoot's inverted durable: true when a
// created directory's parent could not be fsynced.
func mkdirAllAbs(dir string, mode os.FileMode, logger *slog.Logger) (dirSyncFailed bool, err error) {
	base, rel, err := deepestExistingDir(dir)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return false, nil
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return false, err
	}
	defer root.Close()
	durable, err := mkdirAllInRoot(root, rel, mode, logger)
	return !durable, err
}

// deepestExistingDir splits the absolute, cleaned dir into the deepest
// ancestor that already exists and the relative remainder still to be
// created. rel is "." when dir itself exists. A non-directory on the way up
// is ENOTDIR, matching os.MkdirAll.
//
// This is the one ambient path resolution the absolute-path family cannot
// avoid — read-only (os.Stat only), and the directories it hands back are
// re-resolved once by os.OpenRoot, after which every operation is
// openat-relative.
func deepestExistingDir(dir string) (base, rel string, err error) {
	rel = "."
	for cur := dir; ; {
		fi, statErr := os.Stat(cur)
		if statErr == nil {
			if !fi.IsDir() {
				return "", "", &fs.PathError{Op: "mkdir", Path: cur, Err: syscall.ENOTDIR}
			}
			return cur, rel, nil
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", "", statErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", "", statErr
		}
		if rel == "." {
			rel = filepath.Base(cur)
		} else {
			rel = filepath.Join(filepath.Base(cur), rel)
		}
		cur = parent
	}
}

// finalizeTempFile runs the temp-side durability barrier on an open temp
// file that already holds its content: verify a WithMaxBytes cap against
// the staged file's actual size (fstat, so bytes staged outside the
// streaming interfaces can never publish an over-cap file), chmod to the
// configured mode, fsync, then close. On any error before close it closes
// the file and returns; the caller's deferred cleanup removes the temp.
func finalizeTempFile(ctx context.Context, tmp *os.File, c *cfg) error {
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: %w", err)
	}
	if c.maxBytes > 0 {
		fi, err := tmp.Stat()
		if err != nil {
			tmp.Close()
			return fmt.Errorf("atomicfile: stat staged file for WithMaxBytes check: %w", err)
		}
		if fi.Size() > c.maxBytes {
			tmp.Close()
			return fmt.Errorf("%w: staged file is %d bytes (max %d)", ErrFileTooLarge, fi.Size(), c.maxBytes)
		}
	}
	// EnforceMode, not tmp.Chmod: a mode argument is a REQUEST, and a
	// filesystem that refuses it must not silently publish a wider file.
	if _, err := EnforceMode(tmp, c.mode); err != nil {
		tmp.Close()
		return &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return &WriteError{Phase: PhaseTempSync, Err: err}
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

// writeAtomic adapts an absolute-path write onto the root-confined engine:
// open the parent directory as an *os.Root, then run writeAtomicInRoot on
// the base name.
func writeAtomic(ctx context.Context, path string, c *cfg, writeData func(*os.File) error) (Result, error) {
	root, base, dirSyncFailed, err := openParentRoot(ctx, path, c)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	res, err := writeAtomicInRoot(ctx, root, base, c, writeData)
	if err != nil {
		return Result{}, err
	}
	res.Durable = res.Durable && !dirSyncFailed
	return res, nil
}

// WriteFile atomically writes data to path. Mode defaults to 0o644 (override
// with WithMode). A nil error means the data is at path; check Result.Durable
// for crash durability.
func WriteFile(ctx context.Context, path string, data []byte, opts ...Option) (Result, error) {
	c := buildCfg(opts)
	if err := checkWriteCap(int64(len(data)), c.maxBytes); err != nil {
		return Result{}, err
	}
	return writeAtomic(ctx, path, c, writeBytes(data))
}

// checkWriteCap enforces WithMaxBytes for the whole-buffer entry points, so
// an over-cap write is rejected before any temp file is created.
func checkWriteCap(size, maxBytes int64) error {
	if maxBytes > 0 && size > maxBytes {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, size, maxBytes)
	}
	return nil
}

func writeBytes(data []byte) func(*os.File) error {
	return func(tmp *os.File) error {
		_, err := tmp.Write(data)
		return err
	}
}

// capWriter enforces a byte cap on writes to w. A write that would cross the
// cap is rejected whole with an error matching ErrFileTooLarge, so a staged
// temp never holds an over-cap prefix.
type capWriter struct {
	w       io.Writer
	limit   int64
	written int64
}

func (cw *capWriter) Write(p []byte) (int, error) {
	if cw.written+int64(len(p)) > cw.limit {
		return 0, fmt.Errorf("%w: write of %d bytes would grow the file to %d bytes (max %d)",
			ErrFileTooLarge, len(p), cw.written+int64(len(p)), cw.limit)
	}
	n, err := cw.w.Write(p)
	cw.written += int64(n)
	return n, err
}

// readerCtx wraps an io.Reader so each Read observes ctx cancellation.
// Sources implementing io.WriterTo bypass Read; WriteReader wraps the
// destination with writerCtx to cover that path.
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

// writerCtx wraps an io.Writer so each Write observes ctx cancellation. A
// single-shot WriterTo source that issues one Write still cannot be
// interrupted mid-write.
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
// efficient copying, so cancellation is coarse on that path (per-chunk for
// chunked sources, post-copy for single-shot sources). ctx is still honored
// at the durability barrier, so a cancelled write leaves no partial target.
func WriteReader(ctx context.Context, path string, r io.Reader, opts ...Option) (Result, error) {
	if r == nil {
		return Result{}, errors.New("atomicfile: nil reader")
	}
	c := buildCfg(opts)
	return writeAtomic(ctx, path, c, copyReader(ctx, r, c.maxBytes))
}

// copyReader returns a writeData closure that streams r into the temp file
// with per-chunk ctx observation, capping content at maxBytes when positive:
// a crossing chunk is rejected whole with an error matching ErrFileTooLarge.
func copyReader(ctx context.Context, r io.Reader, maxBytes int64) func(*os.File) error {
	return func(tmp *os.File) error {
		dst := io.Writer(tmp)
		if maxBytes > 0 {
			dst = &capWriter{w: dst, limit: maxBytes}
		}
		if wt, ok := r.(io.WriterTo); ok {
			_, err := wt.WriteTo(writerCtx{ctx: ctx, w: dst})
			return err
		}
		_, err := io.Copy(dst, readerCtx{ctx: ctx, r: r})
		return err
	}
}

// pendingState tracks a PendingFile's terminal lifecycle. A single bool
// could not distinguish a committed file from a cleaned-up one, letting a
// Commit after Cleanup replay a zero-value Result with a nil error. See
// ErrAborted.
type pendingState uint8

const (
	pendingOpen          pendingState = iota // not yet committed or cleaned up
	pendingCommitted                         // Commit was attempted; result/err are cached
	pendingCleaned                           // Cleanup removed the temp; Commit now fails
	pendingCleanupFailed                     // Cleanup closed the temp but removal failed; Commit still fails and Cleanup retries removal
)

// PendingFile is a temp file open for writing, destined to atomically
// replace a target on Commit. It embeds *os.File, so callers get the full
// io.Writer/io.ReaderFrom/fmt.Fprintf surface, and the embedded Name()
// reports the temp's path so the staged file can be handed to external
// verifiers before Commit.
//
// A PendingFile is written as an append-only stream: Write, WriteString, and
// ReadFrom maintain a byte count (BytesWritten) that Truncate re-syncs, and
// a WithMaxBytes cap is enforced on exactly that stream. Operations on the
// embedded *os.File that step outside the stream model (WriteAt, Write
// after Seek) bypass the accounting, but not the cap: Commit re-verifies the
// staged file's actual size against the cap at the durability barrier.
//
// Every filesystem operation runs through an *os.Root: the caller's root
// for NewPendingFileInRoot, or a root of the target's parent directory that
// NewPendingFile opens and owns. An owned root is closed when the
// PendingFile reaches a terminal state (Commit called, or Cleanup
// succeeded).
//
// A PendingFile is not safe for concurrent use: Commit and Cleanup mutate
// unsynchronized lifecycle state. Confine each PendingFile to a single
// goroutine.
type PendingFile struct {
	*os.File
	cfg     *cfg
	root    *os.Root
	err     error
	path    string // Result.Path value: root.Name() joined with the final name
	name    string // final name, relative to root
	dir     string // parent of name inside root, for the commit-side dir fsync
	tmpName string // temp name, relative to root
	result  Result
	limit   int64 // WithMaxBytes cap; <= 0 means uncapped
	written int64 // bytes staged under the append-stream model; see BytesWritten
	ownRoot bool  // NewPendingFile opened root and must close it at a terminal state
	// dirSyncFailed records that a directory this write CREATED could not be
	// fsynced into its own parent, so Commit reports Durable=false even when
	// the commit-side directory fsync succeeds. See mkdirAllInRoot.
	dirSyncFailed bool
	state         pendingState
}

// newPendingFromRoot creates the staged temp inside root via the engine
// preamble and assembles the PendingFile. ownRoot records whether the
// PendingFile must close root when it reaches a terminal state.
func newPendingFromRoot(ctx context.Context, root *os.Root, name string, ownRoot bool, c *cfg) (*PendingFile, error) {
	st, err := openTempForRoot(ctx, root, name, c)
	if err != nil {
		return nil, err
	}
	return &PendingFile{
		File:          st.file,
		cfg:           c,
		root:          root,
		path:          filepath.Join(root.Name(), st.name),
		name:          st.name,
		dir:           st.dir,
		tmpName:       st.tmpName,
		limit:         c.maxBytes,
		ownRoot:       ownRoot,
		dirSyncFailed: st.dirSyncFailed,
	}, nil
}

// NewPendingFile creates a temp file destined to atomically replace path.
// Write to it, then call Commit to finalize or Cleanup to abort. Mode
// defaults to 0o644 (override with WithMode). ctx is checked before the
// temp is created.
func NewPendingFile(ctx context.Context, path string, opts ...Option) (*PendingFile, error) {
	c := buildCfg(opts)
	root, base, dirSyncFailed, err := openParentRoot(ctx, path, c)
	if err != nil {
		return nil, err
	}
	pf, err := newPendingFromRoot(ctx, root, base, true, c)
	if err != nil {
		root.Close()
		return nil, err
	}
	// The mkdir ran before this PendingFile's own root existed, so its
	// durability verdict is folded in here.
	pf.dirSyncFailed = pf.dirSyncFailed || dirSyncFailed
	return pf, nil
}

// checkCap rejects a write of n more bytes when it would cross the
// WithMaxBytes cap, before any byte lands.
func (p *PendingFile) checkCap(n int) error {
	if p.limit > 0 && p.written+int64(n) > p.limit {
		return fmt.Errorf("%w: write of %d bytes would grow the staged file to %d bytes (max %d)",
			ErrFileTooLarge, n, p.written+int64(n), p.limit)
	}
	return nil
}

// Write stages p, enforcing the WithMaxBytes cap when one is set: a write
// that would cross the cap is rejected whole with an error matching
// ErrFileTooLarge. Accepted bytes advance BytesWritten.
func (p *PendingFile) Write(b []byte) (int, error) {
	if err := p.checkCap(len(b)); err != nil {
		return 0, err
	}
	n, err := p.File.Write(b)
	p.written += int64(n)
	return n, err
}

// WriteString applies the same WithMaxBytes cap and byte accounting as
// Write (the embedded *os.File's own WriteString would bypass both).
func (p *PendingFile) WriteString(s string) (int, error) {
	if err := p.checkCap(len(s)); err != nil {
		return 0, err
	}
	n, err := p.File.WriteString(s)
	p.written += int64(n)
	return n, err
}

// ReadFrom streams r into the staged temp. With a WithMaxBytes cap set it
// routes every chunk through Write; uncapped it delegates to the embedded
// *os.File's ReadFrom (keeping the OS copy fast path) and records the
// copied size.
func (p *PendingFile) ReadFrom(r io.Reader) (int64, error) {
	if p.limit <= 0 {
		n, err := p.File.ReadFrom(r)
		p.written += n
		return n, err
	}
	// The anonymous struct hides this method from io.Copy, forcing the
	// generic loop through the capped Write.
	return io.Copy(struct{ io.Writer }{p}, r)
}

// Truncate resizes the staged temp and re-syncs the byte accounting to the
// new size. Growing the file beyond a WithMaxBytes cap is rejected with an
// error matching ErrFileTooLarge.
func (p *PendingFile) Truncate(size int64) error {
	if p.limit > 0 && size > p.limit {
		return fmt.Errorf("%w: truncate to %d bytes (max %d)", ErrFileTooLarge, size, p.limit)
	}
	if err := p.File.Truncate(size); err != nil {
		return err
	}
	p.written = size
	return nil
}

// BytesWritten reports the staged file's size under the append-stream
// model: bytes accepted through Write, WriteString, and ReadFrom, re-synced
// by Truncate. Operations on the embedded *os.File that step outside the
// stream model are not tracked; a WithMaxBytes cap still catches them at
// Commit, which verifies the staged file's actual size.
func (p *PendingFile) BytesWritten() int64 { return p.written }

// closeOwnedRoot closes the parent-directory root when this PendingFile
// opened it (NewPendingFile). Idempotent. A caller-provided root
// (NewPendingFileInRoot) is never closed.
func (p *PendingFile) closeOwnedRoot() {
	if !p.ownRoot {
		return
	}
	p.ownRoot = false
	if err := p.root.Close(); err != nil {
		p.cfg.logger.Debug("atomicfile: pending parent root close failed",
			"path", p.path, "error", err)
	}
}

// Commit runs the durability barrier (WithMaxBytes size verification,
// chmod, fsync, close), atomically renames the temp into place, and fsyncs
// the parent directory. Commit is idempotent: repeated calls return the
// first result. Calling Commit after Cleanup returns ErrAborted. After a
// successful Commit, Cleanup is a no-op. ctx is checked through the
// temp-side barrier; a context cancelled at or before close aborts and
// removes the temp. Once the barrier passes, the rename runs without a
// further ctx check, so a context cancelled in the narrow window between
// close and rename still commits.
func (p *PendingFile) Commit(ctx context.Context) (Result, error) {
	switch p.state {
	case pendingCommitted:
		return p.result, p.err
	case pendingCleaned, pendingCleanupFailed:
		return Result{}, ErrAborted
	}
	p.state = pendingCommitted
	p.result, p.err = p.commit(ctx)
	p.closeOwnedRoot()
	return p.result, p.err
}

func (p *PendingFile) commit(ctx context.Context) (Result, error) {
	committed := false
	defer func() {
		if !committed {
			removeTempInRoot(p.root, p.tmpName, p.cfg.logger)
		}
	}()
	if fErr := finalizeTempFile(ctx, p.File, p.cfg); fErr != nil {
		return Result{}, fErr
	}
	committed = true
	durable, cErr := commitTempInRoot(p.root, p.tmpName, p.name, p.dir, p.cfg)
	if cErr != nil {
		return Result{}, cErr
	}
	return Result{Path: p.path, Durable: durable && !p.dirSyncFailed}, nil
}

// Cleanup closes and removes the temp file, aborting the pending write. It
// is a no-op once Commit has been called. After a successful Cleanup, a
// subsequent Commit returns ErrAborted.
//
// If the temp removal fails for a reason other than the file already being
// gone, Cleanup returns that error WITHOUT marking the write cleaned, so a
// later Cleanup retries the removal. Safe to defer immediately after
// NewPendingFile.
func (p *PendingFile) Cleanup() error {
	switch p.state {
	case pendingCommitted, pendingCleaned:
		return nil
	case pendingCleanupFailed:
		if err := p.root.Remove(p.tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		p.state = pendingCleaned
		p.closeOwnedRoot()
		return nil
	}
	if clErr := p.Close(); clErr != nil {
		p.cfg.logger.Debug("atomicfile: pending file close during cleanup failed",
			"path", p.Name(), "error", clErr)
	}
	if err := p.root.Remove(p.tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.state = pendingCleanupFailed
		return err
	}
	p.state = pendingCleaned
	p.closeOwnedRoot()
	return nil
}
