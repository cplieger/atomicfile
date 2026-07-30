package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ProbeStage identifies which step of a writability probe failed. The stages
// are the whole ladder a real write walks, in order, because a directory can
// fail any one of them alone: a filesystem can accept the directory entry yet
// reject the first data write, surface a delayed failure only at Sync or
// Close, or accept everything and deny the unlink.
//
// The order is load-bearing, and it is what lets a caller pick its own policy
// (see ProbeResult.Writable): a failure at or after ProbeStageClose means the
// directory did accept and flush real bytes and only teardown failed, while a
// failure before it means nothing was durably written. Callers legitimately
// differ here — one warns and keeps running on a teardown failure, another
// treats every stage as fatal — so the probe reports the stage and never
// decides.
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
	// Skipped under WithNoSync.
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
// It is an outcome record, not an error: a stage failure arrives here, never as
// the probe function's error return, so the caller decides which stages are
// fatal, which deserve a warning, and how to word each one. A caller that wants
// the whole thing to be pass/fail asks OK once.
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

// OK reports whether every stage passed, which is the only outcome that proves
// the directory usable end to end.
func (r ProbeResult) OK() bool { return r.Stage == ProbeStageNone }

// Writable reports whether the directory accepted a real write: the probe file
// was created, written and flushed, so at worst only teardown (close, unlink)
// failed. It is the split a caller needs to warn about a leftover while still
// treating the directory as usable, without hard-coding the stage order at the
// call site.
//
// Under WithNoSync the bytes were written but not flushed, so this reports what
// the caller asked to be checked, no more.
func (r ProbeResult) Writable() bool {
	return r.OK() || r.Stage >= ProbeStageClose
}

// ProbeWritable proves dir is genuinely writable by the running process, by
// doing what a real atomic write does — create a temp file, write and flush a
// byte, close it, unlink it — and reporting which stage failed.
//
// Reach for it in a preflight, where the alternative is a stat of the mode
// bits, and those lie: an NFS or FUSE mount, a read-only bind mount, and a
// Docker volume owned by another UID can all present a writable-looking
// directory that refuses the first write. A hand-rolled create-close-remove
// probe is the usual answer and the usual bug too, because the close and the
// unlink are the errors callers discard — a directory that accepts a create
// and refuses cleanup then passes a preflight silently, and its leftover file
// is named nothing anyone sweeps.
//
// Nothing about policy is decided here. Every filesystem outcome, including a
// total refusal, is reported in the ProbeResult; the error return is non-nil
// only when the probe could not be attempted at all (an empty dir, or a context
// already done), so `err != nil` never means "not writable". See ProbeStage for
// the ladder and ProbeResult.Writable for the usual warn-versus-fail split.
//
// The probe file carries this package's own temp-name shape (see TempName), so
// a leftover from a crash — or from a directory that denies the unlink — is
// reclaimed by CleanupStaleTemps like any other orphaned temp, by construction
// rather than by a naming convention the caller has to reproduce.
//
// A missing dir fails at ProbeStageCreate; pass WithMkdirMode to create it
// first and get ProbeStageMkdir instead. WithNoSync skips the flush stage.
// dir may be relative, matching CleanupStaleTemps' dir argument rather than the
// write functions' absolute-path contract.
//
// ctx is checked once, before anything is created. The stages are single
// filesystem calls the OS does not make interruptible, and a probe that has
// begun always runs its own cleanup rather than abandoning a file, so a caller
// guarding against a wedged mount needs its own timeout around the call.
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
	// Opening dir as an *os.Root runs the probe on the same confined engine
	// every write uses, and its failure is the same "destination not present
	// or not usable" class a temp-creation failure reports (the reasoning
	// openParentRoot records for PhaseTempCreate).
	root, err := os.OpenRoot(clean)
	if err != nil {
		return ProbeResult{Dir: clean, Stage: ProbeStageCreate, Err: err}, nil
	}
	defer root.Close()
	res := probeInRoot(root, ".", c)
	// Report the caller's own directory rather than root.Name() + "/.".
	res.Dir = clean
	return res, nil
}

// ProbeWritableInRoot is ProbeWritable confined to root's tree: name is the
// directory to probe relative to root ("." for root itself), and every
// filesystem operation runs through the *os.Root, so a symlink or ".."
// component can never let the probe create or unlink anything outside it. An
// escaping name is refused by the root and reported as ProbeStageCreate (or
// ProbeStageMkdir under WithMkdirMode), the same way a root-confined write
// surfaces it.
//
// It is the twin for a caller that already holds a root over the directory it
// is about to write to: probing through that handle checks the same object the
// writes will use, instead of re-resolving the path and probing whatever it
// resolves to this time.
//
// Outcome model, options, cancellation and reclaimability are identical to
// ProbeWritable, including the rule that a stage failure is never returned as
// an error. The caller owns root; ProbeWritableInRoot does not close it. A nil
// root returns ErrUnsafePath.
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

// probeInRoot is the probe engine both entry points run: optionally create the
// directory, create a package-shaped temp inside it, write and flush a byte,
// then always close and always unlink. dir is relative to root.
//
// Once the temp exists the teardown runs unconditionally, so an earlier stage
// failure never turns into a leaked file on a directory that would happily
// remove it. Only the FIRST failure is reported (a close failure is the reason
// a write failed to reach disk far more often than the reverse), and a
// secondary teardown failure goes to the logger at Debug.
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
	if stage, sErr := probeData(f, c); sErr != nil {
		res.Stage, res.Err = stage, sErr
	}
	res.tearDown(root, f, tmpName, c)
	return res
}

// probeData runs the stages that need the probe file open: the first data write
// and, unless WithNoSync was set, the flush that makes a deferred filesystem
// error surface. One byte is enough — it forces an allocation, which is what
// separates a directory that accepts an entry from one that accepts data.
func probeData(f *os.File, c *cfg) (ProbeStage, error) {
	if _, err := f.Write([]byte{0}); err != nil {
		return ProbeStageWrite, err
	}
	if c.noSync {
		return ProbeStageNone, nil
	}
	if err := f.Sync(); err != nil {
		return ProbeStageSync, err
	}
	return ProbeStageNone, nil
}

// tearDown closes and unlinks the probe file, folding both outcomes into r: it
// records a close or remove failure only when no earlier stage failed, clears
// Leaked once the file is gone, and logs a teardown failure it is not reporting
// at Debug so it is not lost entirely. A remove of an already-gone file is
// success, matching the rest of the package.
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

// recordTeardown adopts a teardown failure as the reported outcome when nothing
// has failed yet, and otherwise logs it at Debug: the first failure is the one
// that explains the directory, and a caller cannot act on two.
func (r *ProbeResult) recordTeardown(stage ProbeStage, err error, c *cfg) {
	if r.OK() {
		r.Stage, r.Err = stage, err
		return
	}
	c.logger.Debug("atomicfile: writability probe teardown failed",
		"dir", r.Dir, "name", r.Name, "stage", stage.String(), "error", err)
}

// probeCause unwraps the *WriteError createTempInRoot returns so a probe
// reports the filesystem error itself. A WriteError means "the data did not
// reach its final path", which is a claim about a write the probe never
// performed; ProbeResult.Stage already carries the stage.
func probeCause(err error) error {
	var we *WriteError
	if errors.As(err, &we) {
		return we.Err
	}
	return err
}
