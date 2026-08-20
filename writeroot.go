package atomicfile

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// validateRootName checks that name is a non-empty, relative, null-byte-free
// path for use as a key into an *os.Root, returning the cleaned form. Unlike
// validateAbsClean (the absolute-path write contract) it does NOT reject "..":
// an *os.Root already confines every operation to its tree and rejects names
// that escape it, so "a/../b" (which stays inside) is allowed while
// "../escape" is refused by the Root itself when the operation runs.
//
// It accepts a name that cleans to "." because that is a legitimate DIRECTORY
// name inside a root, which ProbeWritableInRoot takes. An operation that needs
// name to be an ENTRY uses validateRootEntry.
func validateRootName(name string) (string, error) {
	if name == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: contains null byte", ErrUnsafePath)
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: not relative: %q", ErrUnsafePath, name)
	}
	return filepath.Clean(name), nil
}

// validateRootEntry is validateRootName for an operation that must name an ENTRY
// inside root rather than a directory: it additionally refuses a name whose final
// element is ".", ".." or the separator, none of which an operation can create,
// rename onto or unlink.
//
// The refusal is shared so the two families answer the same input the same way.
// Without it a write to "sub/.." staged a temp, wrote it, chmod'ed it, fsynced it
// and closed it before failing at PhaseRename with "file exists" — a phase that
// names the wrong step — while OpenParentInRoot refused the identical name with
// ErrUnsafePath before touching the filesystem.
func validateRootEntry(name string) (string, error) {
	clean, err := validateRootName(name)
	if err != nil {
		return "", err
	}
	switch filepath.Base(clean) {
	case ".", "..", string(filepath.Separator):
		return "", fmt.Errorf("%w: names no entry: %q", ErrUnsafePath, name)
	}
	return clean, nil
}

// randomTempName returns a temp base name of the exact shape CleanupStaleTemps
// recognises (".atomicfile-<digits>.tmp"), drawing the digit run from
// crypto/rand. Every temp this package creates — absolute-path entry points
// included, since they adapt onto this engine — carries this one name shape,
// so a single stale-temp sweep reaps every orphan.
//
// It cannot fail: crypto/rand.Read is documented never to return an error (it
// crashes the program if the system source is unusable), so there is no error
// leg to propagate and TempName can export this without a dead error return.
func randomTempName() string {
	var b [8]byte
	rand.Read(b[:])
	return tempPrefix + strconv.FormatUint(binary.LittleEndian.Uint64(b[:]), 10) + tempSuffix
}

// createTempInRoot creates an exclusive temp file in dir (relative to root),
// retrying on the rare random-name collision the way os.CreateTemp does. It
// returns the open file and its root-relative name. An escaping dir is refused
// by root.OpenFile and surfaced as a PhaseTempCreate WriteError.
//
// It creates the staging file owner-only and PROVES it is owner-only before
// returning it to be written into.
//
// The 0o600 in the open is only a request, and on a filesystem carrying an
// inheritable group ACL the kernel stores something wider: measured on a ZFS
// nfs4acl dataset, this exact O_CREATE|O_EXCL with 0o600 stores 0770. The temp
// lives in the TARGET's parent directory — it has to, because publishing is a
// same-filesystem rename — so that parent is as reachable as the target is, and
// a wider mode is not a cosmetic detail: the caller's payload is written into
// this descriptor AFTER this function returns. Without the enforcement here, a
// secret written through WithMode(0o600) sits group-readable AND group-writable
// for the entire duration of the write, and is only narrowed afterwards by
// finalizeTempFile. Group-writable is the worse half — the bytes that get
// renamed into place are then not necessarily the bytes the caller wrote.
//
// So the mode is enforced on the handle at creation, before any data exists,
// and the caller's own mode is enforced again at the end of the write. The
// random name is not a substitute: it makes the temp hard to GUESS, while the
// mode is what makes it unreachable once found in a listable directory.
func createTempInRoot(root *os.Root, dir string) (*os.File, string, error) {
	for try := 0; ; try++ {
		name := filepath.Join(dir, randomTempName())
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, mErr := EnforceMode(f, 0o600); mErr != nil {
				f.Close()
				// The temp is this function's to clean up: the caller never saw
				// it, so nothing else will remove it.
				_ = root.Remove(name)
				return nil, "", &WriteError{Phase: PhaseTempCreate, Err: mErr}
			}
			return f, name, nil
		}
		if errors.Is(err, fs.ErrExist) && try < 10000 {
			continue
		}
		return nil, "", &WriteError{Phase: PhaseTempCreate, Err: err}
	}
}

// checkWriteTargetInRoot refuses a target name already occupied by something an
// atomic write must not replace. A missing target is fine — that is the ordinary
// case.
//
// A symlink is refused with ErrSymlinkTarget. The rename would replace the link
// rather than follow it, so the worst case under a racing attacker who plants one
// is replacing that link, and an *os.Root forbids it pointing outside the tree
// regardless; the refusal is still worth having, because a caller that wrote a
// file and finds a link at its name has lost track of the path.
//
// Anything else that is not a REGULAR file is refused with ErrNotRegular, the
// same verdict ReadBoundedInRoot gives what it will read and RemoveFileInRoot
// gives what it will unlink. Measured before the refusal existed: a write whose
// target was a FIFO SUCCEEDED and reported Durable, because rename(2) happily
// replaces a pipe, a socket or a device node with a regular file — so a co-mounting
// writer could get this package to destroy an object it never created. A directory
// target failed instead, but only at PhaseRename, after a complete staged write
// and both fsyncs, under a phase that named the wrong step.
//
// The check is a check-then-act and cannot be otherwise: rename(2) has no "only
// if the destination is a regular file" mode. Its value is refusing the states
// that are there to be found, not winning a race — and the race it can lose is
// bounded by rename never following a final-component symlink.
func checkWriteTargetInRoot(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("atomicfile: stat target %q: %w", name, err)
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s", ErrSymlinkTarget, name)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%w: %s (type %s)", ErrNotRegular, name, fi.Mode().Type())
	}
	return nil
}

// fsyncRootDir fsyncs a directory inside root so a prior rename survives a
// crash. It is a package var so tests can inject a failure; a real directory
// fsync is impractical to fail on a healthy filesystem. Production never
// reassigns it.
//
// O_DIRECTORY, not root.Open, for the reason WalkDirInRoot records: root.Open
// is a plain O_RDONLY openat, and a reader-less FIFO blocks that open(2)
// indefinitely. Reaching this with a FIFO at dir needs a co-mounting writer to
// have replaced the directory the rename just landed in, which is narrow — but
// the cost of being wrong is a hung goroutine, and the flag is free.
// O_NONBLOCK has no effect on a directory and covers the same hazard twice.
var fsyncRootDir = func(root *os.Root, dir string) error {
	d, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// mkdirAllInRoot creates dir and every missing ancestor inside root at mode,
// enforcing the mode on each directory it creates and fsyncing that directory's
// PARENT so the new directory entry is durable rather than merely present.
//
// os.Root.MkdirAll creates the same chain, and reporting only success is exactly
// why it is not enough here: a directory entry that has never been fsynced into
// its own parent can vanish in a crash, taking every descendant with it —
// including the file this write is about to fsync and rename. Fsyncing the file
// and its immediate parent makes the file durable only when that parent already
// existed; when the write created the parent too, the chain has to be made
// durable level by level. Without this, Result.Durable was true for a write
// whose whole path could disappear.
//
// The mode is ENFORCED, not requested, for the reason EnforceMode records: a
// mode passed to mkdir(2) is narrowed by umask and can be widened by an
// inheritable ACL, so WithMkdirMode(0o700) would otherwise be a suggestion in
// the one package that spent a primitive establishing that it must not be. Only
// directories THIS call created are touched — mkdir(2) gives a new directory to
// its creator, so nothing else has ever held that name — and a pre-existing
// directory is left exactly as found, matching EnsurePrivateDir's rule.
//
// durable is false when a created directory's parent could not be fsynced. That
// degrades Result.Durable instead of failing the write, matching what the
// commit-side barrier does with the identical failure: a filesystem that refuses
// to fsync a directory must not make every WithMkdirMode write an error while
// the same write into an existing directory succeeds.
func mkdirAllInRoot(root *os.Root, dir string, mode os.FileMode, logger *slog.Logger) (durable bool, err error) {
	if dir == "." {
		return true, nil // root itself, nothing to create
	}
	durable = true
	prefix := ""
	for component := range strings.SplitSeq(dir, string(filepath.Separator)) {
		prefix = filepath.Join(prefix, component)
		created, levelErr := mkdirLevelInRoot(root, prefix, mode)
		if levelErr != nil {
			return false, levelErr
		}
		if !created {
			continue
		}
		if syncErr := fsyncRootDir(root, filepath.Dir(prefix)); syncErr != nil {
			logger.Warn("atomicfile: fsync of a created directory's parent failed; write is not durable",
				"root", root.Name(), "dir", prefix, "error", syncErr)
			durable = false
		}
	}
	return durable, nil
}

// mkdirLevelInRoot creates one level of the chain inside root and reports whether
// THIS call created it, which is what licenses the mode enforcement and asks for
// the parent fsync. A level that already exists as a directory is left untouched.
//
// The ErrExist arm is os.MkdirAll's own tiebreak, kept so the error a caller sees
// for a non-directory in the chain does not change: a Stat that resolves to a
// directory is fine, one that resolves to anything else is ENOTDIR, and a Stat
// that fails at all (a dangling or escaping symlink wearing the name) reports the
// original ErrExist.
func mkdirLevelInRoot(root *os.Root, prefix string, mode os.FileMode) (created bool, err error) {
	mkErr := root.Mkdir(prefix, mode)
	switch {
	case mkErr == nil:
		if enfErr := enforceDirMode(root, prefix, mode); enfErr != nil {
			return false, enfErr
		}
		return true, nil
	case errors.Is(mkErr, fs.ErrExist):
		fi, statErr := root.Stat(prefix)
		if statErr != nil {
			return false, mkErr
		}
		if !fi.IsDir() {
			return false, &fs.PathError{Op: "mkdir", Path: prefix, Err: syscall.ENOTDIR}
		}
		return false, nil
	default:
		return false, mkErr
	}
}

// enforceDirMode opens a directory this process just created inside root and
// proves the filesystem stored the mode that was asked for. O_DIRECTORY keeps
// the handle on a directory and O_NONBLOCK keeps the open from parking on
// anything else; EnforceMode then does fchmod and fstat on that one descriptor,
// so no pathname is observed twice.
func enforceDirMode(root *os.Root, dir string, mode os.FileMode) error {
	d, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = EnforceMode(d, mode)
	return err
}

// removeTempInRoot deletes a temp file best-effort, logging at Debug when
// removal fails for a reason other than the file already being gone.
func removeTempInRoot(root *os.Root, tmpName string, logger *slog.Logger) {
	if rmErr := root.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		logger.Debug("atomicfile: temp file cleanup failed", "path", tmpName, "error", rmErr)
	}
}

// commitTempInRoot finalizes a synced, closed temp file inside root:
// atomically rename it to name, then fsync the parent directory. It is the
// single commit-side barrier — every write entry point (absolute-path
// adapters included) commits through it, so a barrier change lands here and
// nowhere else. A pre-rename failure removes the temp and returns an error
// (the data did not land). Once the rename succeeds the data is at name; a
// subsequent parent-dir fsync failure is logged at Warn and reported as
// durable=false with a nil error, never a hard failure.
func commitTempInRoot(root *os.Root, tmpName, name, dir string, c *cfg) (durable bool, err error) {
	if rnErr := root.Rename(tmpName, name); rnErr != nil {
		removeTempInRoot(root, tmpName, c.logger)
		return false, &WriteError{Phase: PhaseRename, Err: rnErr}
	}
	if syncErr := fsyncRootDir(root, dir); syncErr != nil {
		c.logger.Warn("atomicfile: parent-directory fsync failed; write is not durable",
			"root", root.Name(), "path", name, "error", syncErr)
		return false, nil
	}
	return true, nil
}

// stagedTemp is what the pre-barrier preamble produces: the open temp file plus
// the names and the durability state the two barriers need in order to publish
// it. It replaces five positional return values, four of which every caller
// destructured identically.
type stagedTemp struct {
	// file is the open temp file, created owner-only and proved owner-only.
	file *os.File
	// name is the cleaned final name, relative to root.
	name string
	// dir is name's parent inside root, the commit-side dir-fsync target.
	dir string
	// tmpName is the temp file's name, relative to root.
	tmpName string
	// dirSyncFailed records that a directory this write CREATED could not be
	// fsynced into its own parent, so the published file will be reachable but
	// its path not yet durable. The zero value is the healthy one: a write that
	// created no directory has nothing to make durable.
	dirSyncFailed bool
}

// openTempForRoot runs the pre-barrier preamble for every write: it validates
// the relative name, honors ctx, refuses a target already occupied by something
// a write must not replace, optionally creates the parent directory chain, and
// creates the temp file inside root. It is the single place that enforces the
// pre-write guard contract; add new pre-write checks here. The guard sequence
// (nil-root -> validateRootEntry -> ctx -> checkWriteTargetInRoot -> mkdir ->
// createTempInRoot) must not be reordered.
func openTempForRoot(ctx context.Context, root *os.Root, name string, c *cfg) (stagedTemp, error) {
	if root == nil {
		return stagedTemp{}, fmt.Errorf("%w: nil root", ErrUnsafePath)
	}
	cleanName, err := validateRootEntry(name)
	if err != nil {
		return stagedTemp{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stagedTemp{}, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	if tgtErr := checkWriteTargetInRoot(root, cleanName); tgtErr != nil {
		return stagedTemp{}, tgtErr
	}
	dir := filepath.Dir(cleanName)
	dirSyncFailed := false
	if c.mkdirMode != 0 {
		durable, mkErr := mkdirAllInRoot(root, dir, c.mkdirMode, c.logger)
		if mkErr != nil {
			return stagedTemp{}, fmt.Errorf("atomicfile: create parent directory %q: %w", dir, mkErr)
		}
		dirSyncFailed = !durable
	}
	tmp, tmpName, err := createTempInRoot(root, dir)
	if err != nil {
		return stagedTemp{}, err
	}
	return stagedTemp{
		file:          tmp,
		name:          cleanName,
		dir:           dir,
		tmpName:       tmpName,
		dirSyncFailed: dirSyncFailed,
	}, nil
}

// writeAtomicInRoot is the write engine: run the pre-barrier preamble
// (openTempForRoot), run the caller's writeData step, then hand off to the
// temp-side barrier (finalizeTempFile) and the commit-side barrier
// (commitTempInRoot). Every write entry point runs through it — the *InRoot
// functions directly, the absolute-path functions via an *os.Root of the
// target's parent (see writeAtomic). Every filesystem operation runs through
// the *os.Root, so a symlink or ".." component can never cause a write outside
// root's tree.
func writeAtomicInRoot(ctx context.Context, root *os.Root, name string, c *cfg, writeData func(*os.File) error) (Result, error) {
	st, err := openTempForRoot(ctx, root, name, c)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed {
			removeTempInRoot(root, st.tmpName, c.logger)
		}
	}()
	if wErr := writeData(st.file); wErr != nil {
		st.file.Close()
		return Result{}, &WriteError{Phase: PhaseTempWrite, Err: wErr}
	}
	if fErr := finalizeTempFile(ctx, st.file, c); fErr != nil {
		return Result{}, fErr
	}
	committed = true
	durable, cErr := commitTempInRoot(root, st.tmpName, st.name, st.dir, c)
	if cErr != nil {
		return Result{}, cErr
	}
	return Result{Path: filepath.Join(root.Name(), st.name), Durable: durable && !st.dirSyncFailed}, nil
}

// WriteFileInRoot atomically writes data to name, a path relative to root, with
// the same temp-then-rename durability and symlink refusal as WriteFile but
// confined to root: every filesystem operation runs through the *os.Root
// (Go 1.24+), so a symlink or ".." component in name can never write outside
// root's tree. It is the write-side analogue of opening a file through an
// *os.Root and reading it with ReadBoundedFile. Mode defaults to 0o644
// (override with WithMode). A nil error means the data is at name; check
// Result.Durable for crash durability. Result.Path is root's directory joined
// with the cleaned relative name. A nil root returns ErrUnsafePath.
func WriteFileInRoot(ctx context.Context, root *os.Root, name string, data []byte, opts ...Option) (Result, error) {
	c := buildCfg(opts)
	if err := checkWriteCap(int64(len(data)), c.maxBytes); err != nil {
		return Result{}, err
	}
	return writeAtomicInRoot(ctx, root, name, c, writeBytes(data))
}

// WriteReaderInRoot atomically writes the contents of r to name, a path
// relative to root, confined to root's tree (see WriteFileInRoot). If r
// implements io.WriterTo it is used for efficient copying; that fast path
// bypasses the per-Read context check, so cancellation is coarse (per-chunk for
// chunked sources, post-copy for single-shot sources). ctx is still honored at
// the durability barrier, so a cancelled write leaves no partial target. Mode
// defaults to 0o644 (override with WithMode). A nil root returns ErrUnsafePath.
func WriteReaderInRoot(ctx context.Context, root *os.Root, name string, r io.Reader, opts ...Option) (Result, error) {
	if root == nil {
		return Result{}, fmt.Errorf("%w: nil root", ErrUnsafePath)
	}
	if r == nil {
		return Result{}, errors.New("atomicfile: nil reader")
	}
	c := buildCfg(opts)
	return writeAtomicInRoot(ctx, root, name, c, copyReader(ctx, r, c.maxBytes))
}

// NewPendingFileInRoot creates a temp file destined to atomically replace
// name, a path relative to root, with the same confinement as WriteFileInRoot:
// every filesystem operation (temp creation, rename, parent-dir fsync,
// removal) runs through the *os.Root, so a symlink or ".." component in name
// can never touch anything outside root's tree. Write to the returned
// PendingFile, then call Commit to finalize or Cleanup to abort; the lifecycle
// (idempotent Commit, ErrAborted after Cleanup, retryable failed Cleanup) is
// identical to NewPendingFile. Mode defaults to 0o644 (override with
// WithMode). ctx is checked before the temp is created. A nil root returns
// ErrUnsafePath.
//
// The caller owns root and must keep it open for the PendingFile's lifetime
// (through Commit or Cleanup); the PendingFile never closes a caller-provided
// root. Result.Path is root's directory joined with the cleaned relative name.
func NewPendingFileInRoot(ctx context.Context, root *os.Root, name string, opts ...Option) (*PendingFile, error) {
	return newPendingFromRoot(ctx, root, name, false, buildCfg(opts))
}
