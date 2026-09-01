package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ProbeStage identifies which step of a writability probe failed, in the
// order a real write walks: a directory can accept the entry yet reject the
// first write, fail only at Sync or Close, or accept everything and deny the
// unlink.
//
// The order is load-bearing: ProbeStageClose or later means real bytes were
// flushed and only teardown failed (see ProbeResult.Writable); the probe
// reports the stage and leaves the pass/fail policy to the caller.
type ProbeStage int

const (
	// ProbeStageNone is the zero value: every stage passed.
	ProbeStageNone ProbeStage = iota
	// ProbeStageMkdir indicates the directory itself could not be created.
	// Only reachable with WithMkdirMode; without it a missing directory
	// fails at ProbeStageCreate, exactly as a write to a missing parent does.
	ProbeStageMkdir
	// ProbeStageCreate indicates the probe file could not be created: the
	// directory is missing, is not a directory, is read-only, or denies the
	// running UID. It also covers a name that escapes the *os.Root in
	// ProbeWritableInRoot.
	ProbeStageCreate
	// ProbeStageWrite indicates the first data write to the probe file failed
	// (a quota or a full filesystem, which the create alone does not reveal).
	ProbeStageWrite
	// ProbeStageSync indicates flushing the probe file failed, the delayed
	// error a network filesystem reports at fsync rather than at write.
	ProbeStageSync
	// ProbeStageClose indicates closing the probe file failed, the other
	// place a deferred write error surfaces.
	ProbeStageClose
	// ProbeStageRemove indicates the probe file could not be unlinked, so the
	// directory accepts writes but refuses cleanup. ProbeResult.Leaked is then
	// true and the leftover is reclaimable by the package's stale-temp sweep.
	ProbeStageRemove
)

func (s ProbeStage) String() string {
	switch s {
	case ProbeStageNone:
		return "no failure"
	case ProbeStageMkdir:
		return "create directory"
	case ProbeStageCreate:
		return "create probe file"
	case ProbeStageWrite:
		return "write probe file"
	case ProbeStageSync:
		return "sync probe file"
	case ProbeStageClose:
		return "close probe file"
	case ProbeStageRemove:
		return "remove probe file"
	default:
		return "unknown stage"
	}
}

// ProbeResult reports the outcome of one directory writability probe.
//
// It is an outcome record, not an error: a stage failure arrives here, never
// as the probe function's error return, so the caller decides which stages
// are fatal. A caller that wants pass/fail asks OK once.
type ProbeResult struct {
	// Err is the failure from Stage, unwrapped to the underlying filesystem
	// error so errors.Is against fs.ErrPermission, fs.ErrNotExist or
	// syscall.ENOSPC works. It is nil exactly when OK reports true.
	Err error
	// Dir is the directory that was probed: the cleaned argument for
	// ProbeWritable, or root.Name() joined with the cleaned relative name for
	// ProbeWritableInRoot (the Result.Path convention).
	Dir string
	// Name is the probe file's base name once it was created, and "" when the
	// probe never got that far. It always satisfies IsPackageTemp, so a
	// leftover is reclaimable by CleanupStaleTemps.
	Name string
	// Stage is the first stage that failed, or ProbeStageNone when all passed.
	Stage ProbeStage
	// Leaked reports whether the probe file is still on disk. It can be true
	// only alongside ProbeStageRemove, or alongside ProbeStageWrite,
	// ProbeStageSync or ProbeStageClose when the follow-up unlink also failed
	// (that secondary error is logged at Debug, since Err holds the first and
	// more informative failure).
	Leaked bool
}

// OK reports whether every stage passed.
func (r ProbeResult) OK() bool { return r.Stage == ProbeStageNone }

// Writable reports whether the directory accepted a real write — created,
// written and flushed — so at worst only teardown (close, unlink) failed.
func (r ProbeResult) Writable() bool {
	return r.OK() || r.Stage >= ProbeStageClose
}

// ProbeWritable proves dir is genuinely writable by doing what a real atomic
// write does — create a temp file, write and flush a byte, close it, unlink
// it — and reporting which stage failed.
//
// A mode-bit stat lies on an NFS/FUSE mount, a read-only bind mount, or a
// Docker volume owned by another UID; this exercises the real path instead.
//
// `err != nil` means the probe could not be attempted at all (empty dir, or
// a done context), never "not writable" — every filesystem outcome is
// reported in the ProbeResult. See ProbeStage and ProbeResult.Writable.
//
// The probe file carries this package's temp-name shape (TempName), so a
// leftover from a crash or a denied unlink is reclaimed by
// CleanupStaleTemps.
//
// A missing dir fails at ProbeStageCreate; pass WithMkdirMode for
// ProbeStageMkdir instead. dir may be relative, matching CleanupStaleTemps
// rather than the write functions' absolute-path contract.
//
// ctx is checked once, before anything is created; a probe that has begun
// always runs its own cleanup rather than abandoning a file.
func ProbeWritable(ctx context.Context, dir string, opts ...Option) (ProbeResult, error) {
	c := buildCfg(opts)
	if dir == "" {
		return ProbeResult{}, ErrEmptyPath
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ProbeResult{}, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	clean := filepath.Clean(dir)
	if c.mkdirMode != 0 {
		if mkErr := os.MkdirAll(clean, c.mkdirMode); mkErr != nil {
			return ProbeResult{Dir: clean, Stage: ProbeStageMkdir, Err: mkErr}, nil
		}
	}
	root, err := os.OpenRoot(clean)
	if err != nil {
		return ProbeResult{Dir: clean, Stage: ProbeStageCreate, Err: err}, nil
	}
	defer root.Close()
	res := probeInRoot(root, ".", c)
	res.Dir = clean // report the caller's own directory, not root.Name()+"/.".
	return res, nil
}

// ProbeWritableInRoot is ProbeWritable confined to root's tree: name is
// relative to root ("." for root itself), and every operation runs through
// the *os.Root, so a symlink or ".." component cannot escape it. An escaping
// name is refused and reported as ProbeStageCreate (or ProbeStageMkdir under
// WithMkdirMode).
//
// Outcome model, options, cancellation and reclaimability are identical to
// ProbeWritable. The caller owns root; ProbeWritableInRoot does not close
// it. A nil root returns ErrUnsafePath.
func ProbeWritableInRoot(ctx context.Context, root *os.Root, name string, opts ...Option) (ProbeResult, error) {
	c := buildCfg(opts)
	if root == nil {
		return ProbeResult{}, fmt.Errorf("%w: nil root", ErrUnsafePath)
	}
	clean, err := validateRootName(name)
	if err != nil {
		return ProbeResult{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ProbeResult{}, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	return probeInRoot(root, clean, c), nil
}

// probeInRoot is the probe engine both entry points run: optionally create
// the directory, create a package-shaped temp inside it, write and flush a
// byte, then always close and always unlink. dir is relative to root.
//
// Once the temp exists teardown runs unconditionally, so an earlier stage
// failure never leaks a file the directory would happily remove. Only the
// first failure is reported; a secondary teardown failure goes to Debug.
func probeInRoot(root *os.Root, dir string, c *cfg) ProbeResult {
	if c.mkdirMode != 0 {
		if mkErr := root.MkdirAll(dir, c.mkdirMode); mkErr != nil {
			return ProbeResult{Dir: filepath.Join(root.Name(), dir), Stage: ProbeStageMkdir, Err: mkErr}
		}
	}
	f, tmpName, err := createTempInRoot(root, dir)
	if err != nil {
		return ProbeResult{Dir: filepath.Join(root.Name(), dir), Stage: ProbeStageCreate, Err: probeCause(err)}
	}
	res := ProbeResult{
		Dir:    filepath.Join(root.Name(), dir),
		Name:   filepath.Base(tmpName),
		Leaked: true,
	}
	if stage, sErr := probeData(f); sErr != nil {
		res.Stage, res.Err = stage, sErr
	}
	res.tearDown(root, f, tmpName, c)
	return res
}

// probeData runs the stages that need the probe file open: the first data
// write and the flush that surfaces a deferred filesystem error. One byte is
// enough to force an allocation.
func probeData(f *os.File) (ProbeStage, error) {
	if _, err := f.Write([]byte{0}); err != nil {
		return ProbeStageWrite, err
	}
	if err := f.Sync(); err != nil {
		return ProbeStageSync, err
	}
	return ProbeStageNone, nil
}

// tearDown closes and unlinks the probe file: records a close or remove
// failure only when no earlier stage failed, clears Leaked once the file is
// gone, and logs any teardown failure it isn't reporting at Debug. A remove
// of an already-gone file is success.
func (r *ProbeResult) tearDown(root *os.Root, f *os.File, tmpName string, c *cfg) {
	if clErr := f.Close(); clErr != nil {
		r.recordTeardown(ProbeStageClose, clErr, c)
	}
	rmErr := root.Remove(tmpName)
	if rmErr == nil || errors.Is(rmErr, fs.ErrNotExist) {
		r.Leaked = false
		return
	}
	r.recordTeardown(ProbeStageRemove, rmErr, c)
}

// recordTeardown adopts a teardown failure as the reported outcome only when
// nothing has failed yet; a later one goes to Debug.
func (r *ProbeResult) recordTeardown(stage ProbeStage, err error, c *cfg) {
	if r.OK() {
		r.Stage, r.Err = stage, err
		return
	}
	c.logger.Debug("atomicfile: writability probe teardown failed",
		"dir", r.Dir, "name", r.Name, "stage", stage.String(), "error", err)
}

// probeCause unwraps the *WriteError createTempInRoot returns so a probe
// reports the filesystem error itself rather than the write-path wrapper;
// ProbeResult.Stage already carries the stage.
func probeCause(err error) error {
	if we, ok := errors.AsType[*WriteError](err); ok {
		return we.Err
	}
	return err
}
