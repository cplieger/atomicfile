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
// validateAbsClean it does NOT reject "..": an *os.Root already confines
// every operation to its tree and rejects names that escape it, so
// "a/../b" (which stays inside) is allowed while "../escape" is refused by
// the Root itself when the operation runs.
//
// It accepts a name that cleans to "." — a legitimate DIRECTORY name inside
// a root, which ProbeWritableInRoot takes. An operation that needs name to
// be an ENTRY uses validateRootEntry.
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

// validateRootEntry is validateRootName for an operation that must name an
// ENTRY inside root rather than a directory: it additionally refuses a name
// whose final element is ".", ".." or the separator, none of which an
// operation can create, rename onto or unlink.
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
// crypto/rand.
//
// It cannot fail: crypto/rand.Read is documented never to return an error.
func randomTempName() string {
	var b [8]byte
	rand.Read(b[:])
	return tempPrefix + strconv.FormatUint(binary.LittleEndian.Uint64(b[:]), 10) + tempSuffix
}

// createTempInRoot creates an exclusive temp file in dir (relative to root),
// retrying on a random-name collision the way os.CreateTemp does. It
// returns the open file and its root-relative name. An escaping dir is
// refused by root.OpenFile and surfaced as a PhaseTempCreate WriteError.
//
// It creates the staging file owner-only and PROVES it is owner-only before
// returning it to be written into: the 0o600 passed to open(2) is only a
// request, and on a filesystem carrying an inheritable group ACL the kernel
// can store something wider (measured: a ZFS nfs4acl dataset stores 0770 for
// this exact open). The temp lives in the target's own parent directory
// (publishing is a same-filesystem rename), so a wider mode there leaves the
// caller's payload group-writable for the whole duration of the write —
// meaning the bytes renamed into place are not necessarily the bytes the
// caller wrote. The random name only makes the temp hard to guess; the mode
// enforcement is what makes it unreachable once found.
func createTempInRoot(root *os.Root, dir string) (*os.File, string, error) {
	for try := 0; ; try++ {
		name := filepath.Join(dir, randomTempName())
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, mErr := EnforceMode(f, 0o600); mErr != nil {
				f.Close()
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

// checkWriteTargetInRoot refuses a target name already occupied by
// something an atomic write must not replace. A missing target is fine.
//
// A symlink is refused with ErrSymlinkTarget: the rename would replace the
// link rather than follow it. Anything else that is not a REGULAR file is
// refused with ErrNotRegular, the same verdict ReadBoundedInRoot and
// RemoveFileInRoot give. Without this check, a write whose target was a
// FIFO succeeded and reported Durable, because rename(2) happily replaces a
// pipe, socket or device node with a regular file — so a co-mounting writer
// could get this package to destroy an object it never created.
//
// This is a check-then-act and cannot be otherwise: rename(2) has no "only
// if the destination is a regular file" mode. Its value is refusing the
// states that are there to be found, not winning a race.
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
// crash. It is a package var so tests can inject a failure.
//
// O_DIRECTORY, not root.Open: a reader-less FIFO would block a plain
// O_RDONLY open(2) indefinitely. O_NONBLOCK covers the same hazard again.
var fsyncRootDir = func(root *os.Root, dir string) error {
	d, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// mkdirAllInRoot creates dir and every missing ancestor inside root at mode,
// enforcing the mode on each directory it creates and fsyncing that
// directory's PARENT so the new directory entry is durable rather than
// merely present.
//
// os.Root.MkdirAll is not enough here: a directory entry never fsynced into
// its own parent can vanish in a crash, taking every descendant with it —
// including the file this write is about to fsync and rename. So the chain
// is made durable level by level, and only directories THIS call created
// are touched; a pre-existing directory is left exactly as found.
//
// durable is false when a created directory's parent could not be fsynced,
// which degrades Result.Durable instead of failing the write.
func mkdirAllInRoot(root *os.Root, dir string, mode os.FileMode, logger *slog.Logger) (durable bool, err error) {
	if dir == "." {
		return true, nil
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

// mkdirLevelInRoot creates one level of the chain inside root and reports
// whether THIS call created it, which licenses the mode enforcement and the
// parent fsync. A level that already exists as a directory is left
// untouched.
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

// enforceDirMode opens a directory this process just created inside root
// and proves the filesystem stored the requested mode.
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
// atomically rename it to name, then fsync the parent directory. A
// pre-rename failure removes the temp and returns an error. Once the rename
// succeeds the data is at name; a subsequent parent-dir fsync failure is
// logged at Warn and reported as durable=false with a nil error.
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

// stagedTemp is what the pre-barrier preamble produces: the open temp file
// plus the names and durability state the two barriers need to publish it.
type stagedTemp struct {
	// file is the open temp file, created owner-only and proved owner-only.
	file *os.File
	// name is the cleaned final name, relative to root.
	name string
	// dir is name's parent inside root, the commit-side dir-fsync target.
	dir string
	// tmpName is the temp file's name, relative to root.
	tmpName string
	// dirSyncFailed records that a directory this write CREATED could not
	// be fsynced into its own parent.
	dirSyncFailed bool
}

// openTempForRoot runs the pre-barrier preamble for every write: validate
// the relative name, honor ctx, refuse a target already occupied by
// something a write must not replace, optionally create the parent
// directory chain, and create the temp file inside root. The guard sequence
// (nil-root -> validateRootEntry -> ctx -> checkWriteTargetInRoot -> mkdir
// -> createTempInRoot) must not be reordered.
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
// (commitTempInRoot). Every filesystem operation runs through the
// *os.Root, so a symlink or ".." component can never cause a write outside
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

// WriteFileInRoot atomically writes data to name, a path relative to root,
// with the same temp-then-rename durability and symlink refusal as
// WriteFile but confined to root: every filesystem operation runs through
// the *os.Root (Go 1.24+), so a symlink or ".." component in name can never
// write outside root's tree. Mode defaults to 0o644 (override with
// WithMode). A nil error means the data is at name; check Result.Durable
// for crash durability. Result.Path is root's directory joined with the
// cleaned relative name. A nil root returns ErrUnsafePath.
func WriteFileInRoot(ctx context.Context, root *os.Root, name string, data []byte, opts ...Option) (Result, error) {
	c := buildCfg(opts)
	if err := checkWriteCap(int64(len(data)), c.maxBytes); err != nil {
		return Result{}, err
	}
	return writeAtomicInRoot(ctx, root, name, c, writeBytes(data))
}

// WriteReaderInRoot atomically writes the contents of r to name, a path
// relative to root, confined to root's tree (see WriteFileInRoot). If r
// implements io.WriterTo it is used for efficient copying, so cancellation
// is coarse on that path. Mode defaults to 0o644 (override with WithMode).
// A nil root returns ErrUnsafePath.
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
// name, a path relative to root, with the same confinement as
// WriteFileInRoot. Write to the returned PendingFile, then call Commit to
// finalize or Cleanup to abort; the lifecycle is identical to
// NewPendingFile. Mode defaults to 0o644 (override with WithMode). A nil
// root returns ErrUnsafePath.
//
// The caller owns root and must keep it open for the PendingFile's
// lifetime; the PendingFile never closes a caller-provided root.
// Result.Path is root's directory joined with the cleaned relative name.
func NewPendingFileInRoot(ctx context.Context, root *os.Root, name string, opts ...Option) (*PendingFile, error) {
	return newPendingFromRoot(ctx, root, name, false, buildCfg(opts))
}
