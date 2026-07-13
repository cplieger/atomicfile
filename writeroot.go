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

// randomTempName returns a temp base name of the exact shape CleanupStaleTemps
// recognises (".atomicfile-<digits>.tmp"), drawing the digit run from
// crypto/rand. Matching os.CreateTemp's name shape keeps root-confined temps
// reapable by the same stale-temp sweep as every other temp this package makes.
func randomTempName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return tempPrefix + strconv.FormatUint(binary.LittleEndian.Uint64(b[:]), 10) + tempSuffix, nil
}

// createTempInRoot creates an exclusive temp file in dir (relative to root),
// retrying on the rare random-name collision the way os.CreateTemp does. It
// returns the open file and its root-relative name. An escaping dir is refused
// by root.OpenFile and surfaced as a PhaseTempCreate WriteError.
func createTempInRoot(root *os.Root, dir string) (*os.File, string, error) {
	for try := 0; ; try++ {
		base, err := randomTempName()
		if err != nil {
			return nil, "", &WriteError{Phase: PhaseTempCreate, Err: err}
		}
		name := filepath.Join(dir, base)
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, name, nil
		}
		if errors.Is(err, fs.ErrExist) && try < 10000 {
			continue
		}
		return nil, "", &WriteError{Phase: PhaseTempCreate, Err: err}
	}
}

// checkSymlinkInRoot refuses a symlink target by default, mirroring the
// absolute-path write contract, unless WithAllowSymlinkTarget was set. A
// missing target is fine. Because the eventual rename replaces the final name
// rather than following it, the worst case under a racing attacker who plants a
// symlink is replacing that link — and an *os.Root forbids the link from
// pointing outside the tree regardless.
//
// As with the absolute-path checkSymlink, WithPreserveMode and
// WithPreserveOwnership read the target via a symlink-following
// root.Stat (resolveModeInRoot / applyOwnershipInRoot), so within
// this window a symlink planted inside the root can influence the
// result file's mode or owner -- never its content, location, or
// anything outside the root. Keep the directory non-attacker-writable
// to close the window entirely.
func checkSymlinkInRoot(root *os.Root, name string, c *cfg) error {
	if c.allowSymlinkTarget {
		return nil
	}
	fi, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("atomicfile: stat target %q: %w", name, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkTarget, name)
	}
	return nil
}

// resolveModeInRoot mirrors resolveMode for a confined write: honor
// WithPreserveMode by reusing the target's current permission, falling back to
// the configured mode when the target is absent or cannot be stat-ed.
func resolveModeInRoot(root *os.Root, name string, c *cfg) os.FileMode {
	if c.preserveMode {
		fi, err := root.Stat(name)
		if err == nil {
			return fi.Mode().Perm()
		}
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("atomicfile: preserve-mode stat failed; using explicit mode",
				"target", name, "error", err)
		}
	}
	return c.mode
}

// applyOwnershipInRoot mirrors applyOwnership for a confined write: when
// WithPreserveOwnership is set, chown the temp to the target's uid/gid. No-op
// when the target is absent or its FileInfo.Sys() is not a *syscall.Stat_t.
// Best-effort: a failed chown is logged at Warn and the write proceeds with the
// writer's ownership; it never aborts the write.
func applyOwnershipInRoot(root *os.Root, tmpName, target string, c *cfg) {
	if !c.preserveOwnership {
		return
	}
	fi, err := root.Stat(target)
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
	if err := root.Chown(tmpName, int(stat.Uid), int(stat.Gid)); err != nil {
		c.logger.Warn("atomicfile: preserve-ownership chown failed; keeping writer ownership",
			"target", target, "uid", stat.Uid, "gid", stat.Gid, "error", err)
	}
}

// fsyncRootDir fsyncs a directory inside root so a prior rename survives a
// crash. It is a package var so tests can inject a failure, mirroring fsyncDir;
// a real directory fsync is impractical to fail on a healthy filesystem.
// Production never reassigns it.
var fsyncRootDir = func(root *os.Root, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// removeTempInRoot deletes a temp file best-effort, logging at Debug when
// removal fails for a reason other than the file already being gone.
func removeTempInRoot(root *os.Root, tmpName string, logger *slog.Logger) {
	if rmErr := root.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		logger.Debug("atomicfile: temp file cleanup failed", "path", tmpName, "error", rmErr)
	}
}

// commitTempInRoot finalizes a synced, closed temp file inside root: apply
// ownership, atomically rename it to name, then fsync the parent directory. It
// is the root-confined analogue of commitTemp. A pre-rename failure removes the
// temp and returns an error (the data did not land). Once the rename succeeds
// the data is at name; a subsequent parent-dir fsync failure is logged at Warn
// and reported as durable=false with a nil error, never a hard failure.
func commitTempInRoot(root *os.Root, tmpName, name, dir string, c *cfg) (durable bool, err error) {
	applyOwnershipInRoot(root, tmpName, name, c)
	if rnErr := root.Rename(tmpName, name); rnErr != nil {
		removeTempInRoot(root, tmpName, c.logger)
		return false, &WriteError{Phase: PhaseRename, Err: rnErr}
	}
	if c.noSync {
		return false, nil
	}
	if syncErr := fsyncRootDir(root, dir); syncErr != nil {
		c.logger.Warn("atomicfile: parent-directory fsync failed; write is not durable",
			"root", root.Name(), "path", name, "error", syncErr)
		return false, nil
	}
	return true, nil
}

// openTempForRoot runs the pre-barrier preamble for a root-confined write,
// mirroring openTempForPath on the absolute-path side: it validates the
// relative name, honors ctx, refuses a symlink target, optionally creates the
// parent directory, and creates the temp file inside root. The guard sequence
// (nil-root -> validateRootName -> ctx -> checkSymlinkInRoot -> mkdir ->
// createTempInRoot) is the root-confinement contract and must not be reordered.
// On any error it returns zero values and the error; on success it returns the
// open temp file plus the cleaned name, parent dir, and root-relative temp name.
func openTempForRoot(ctx context.Context, root *os.Root, name string, c *cfg) (tmp *os.File, cleanName, dir, tmpName string, err error) {
	if root == nil {
		return nil, "", "", "", fmt.Errorf("%w: nil root", ErrUnsafePath)
	}
	cleanName, err = validateRootName(name)
	if err != nil {
		return nil, "", "", "", err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, "", "", "", fmt.Errorf("atomicfile: %w", ctxErr)
	}
	if symErr := checkSymlinkInRoot(root, cleanName, c); symErr != nil {
		return nil, "", "", "", symErr
	}
	dir = filepath.Dir(cleanName)
	if c.mkdirMode != 0 {
		if mkErr := root.MkdirAll(dir, c.mkdirMode); mkErr != nil {
			return nil, "", "", "", fmt.Errorf("atomicfile: create parent directory %q: %w", dir, mkErr)
		}
	}
	tmp, tmpName, err = createTempInRoot(root, dir)
	if err != nil {
		return nil, "", "", "", err
	}
	return tmp, cleanName, dir, tmpName, nil
}

// writeAtomicInRoot is the root-confined analogue of writeAtomic: validate the
// relative name, honor ctx, refuse symlink targets, optionally create the
// parent directory, create the temp inside root, run the caller's writeData
// step, then hand off to the shared temp-side barrier (finalizeTempFile) and
// the root commit. Every filesystem operation runs through the *os.Root, so a
// symlink or ".." component can never cause a write outside root's tree.
func writeAtomicInRoot(ctx context.Context, root *os.Root, name string, c *cfg, writeData func(*os.File) error) (Result, error) {
	tmp, cleanName, dir, tmpName, err := openTempForRoot(ctx, root, name, c)
	if err != nil {
		return Result{}, err
	}
	committed := false
	defer func() {
		if !committed {
			removeTempInRoot(root, tmpName, c.logger)
		}
	}()
	mode := resolveModeInRoot(root, cleanName, c)
	if wErr := writeData(tmp); wErr != nil {
		tmp.Close()
		return Result{}, &WriteError{Phase: PhaseTempWrite, Err: wErr}
	}
	if fErr := finalizeTempFile(ctx, tmp, mode, c.noSync); fErr != nil {
		return Result{}, fErr
	}
	committed = true
	durable, cErr := commitTempInRoot(root, tmpName, cleanName, dir, c)
	if cErr != nil {
		return Result{}, cErr
	}
	return Result{Path: filepath.Join(root.Name(), cleanName), Durable: durable}, nil
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
	return writeAtomicInRoot(ctx, root, name, buildCfg(opts), func(tmp *os.File) error {
		_, err := tmp.Write(data)
		return err
	})
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
	return writeAtomicInRoot(ctx, root, name, buildCfg(opts), func(tmp *os.File) error {
		if wt, ok := r.(io.WriterTo); ok {
			_, err := wt.WriteTo(writerCtx{ctx: ctx, w: tmp})
			return err
		}
		_, err := io.Copy(tmp, readerCtx{ctx: ctx, r: r})
		return err
	})
}
