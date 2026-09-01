package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// walkReadDirBatch bounds one ReadDir call's memory: fs.WalkDir reads and
// sorts a directory's whole inventory before the first callback, so a large
// or hostile directory is resident before the caller can refuse it. Not
// configurable — a caller that wants to stop early returns fs.SkipAll from fn.
const walkReadDirBatch = 256

// WalkDirInRoot enumerates root's tree and hands every entry to fn,
// root-relative, confined to root's tree: every step is an openat-relative
// syscall, no ambient path is ever constructed, and a symlinked directory is
// never descended.
//
// Compared to fs.WalkDir over root.FS(): entries are read in fixed batches
// (walkReadDirBatch) rather than one materialized+sorted inventory per
// directory; exactly ONE directory handle is open at a time (a queue of
// NAMES between directories, so descriptor use cannot exhaust on a deep
// tree); and each directory is opened O_DIRECTORY, so a named pipe swapped
// in for a directory is refused with ENOTDIR instead of blocking the calling
// goroutine forever in open(2).
//
// # Callback contract
//
// fn is an fs.WalkDirFunc, called exactly as fs.WalkDir calls one: the root
// itself first as ".", then each entry pre-order, with a directory that
// cannot be opened or finished reported through fn for ITS OWN path (d nil,
// err set). fs.SkipDir and fs.SkipAll mean what they mean in fs.WalkDir.
// Whether a read failure below the root is fatal is the callback's call:
// return the error to abort, or nil to count it and continue.
//
// # Two deliberate differences from fs.WalkDir
//
// Entries arrive in DIRECTORY order, sorted only within a batch — do not
// build a decision on the order. A directory's own entries are all visited
// before the walk descends into its subdirectories.
//
// # Cancellation
//
// The walk stops between batches once ctx is done (at most one batch of
// entries); a caller wanting per-entry cancellation checks ctx in fn.
//
// The caller owns root; WalkDirInRoot does not close it.
func WalkDirInRoot(ctx context.Context, root *os.Root, fn fs.WalkDirFunc) error {
	if root == nil {
		return errors.New("atomicfile: nil root")
	}
	if fn == nil {
		return errors.New("atomicfile: nil walk function")
	}
	w := &rootWalk{ctx: ctx, root: root, fn: fn}
	err := w.run()
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

// rootWalk carries one WalkDirInRoot traversal.
type rootWalk struct {
	ctx     context.Context
	root    *os.Root
	fn      fs.WalkDirFunc
	stopped bool
}

// walkAction is what one callback answer means for the traversal.
type walkAction int

const (
	walkContinue walkAction = iota
	walkSkipDir
	walkStop
)

// classifyWalkAnswer splits a callback's return into a traversal action and
// a real error. Only the second is ever propagated to WalkDirInRoot's caller.
func classifyWalkAnswer(err error) (walkAction, error) {
	switch {
	case err == nil:
		return walkContinue, nil
	case errors.Is(err, fs.SkipAll):
		return walkStop, nil
	case errors.Is(err, fs.SkipDir):
		return walkSkipDir, nil
	default:
		return walkContinue, err
	}
}

func cmpEntryName(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) }

// run visits the root and then streams every directory reachable from it,
// keeping the pending set as a stack of NAMES so no ancestor handle stays
// open during the descent.
func (w *rootWalk) run() error {
	if err := w.ctx.Err(); err != nil {
		return fmt.Errorf("atomicfile: %w", err)
	}
	fi, err := w.root.Stat(".")
	if err != nil {
		return w.fn(".", nil, err)
	}
	act, cbErr := classifyWalkAnswer(w.fn(".", fs.FileInfoToDirEntry(fi), nil))
	if cbErr != nil {
		return cbErr
	}
	if act != walkContinue {
		return nil
	}

	pending := []string{"."}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		subdirs, streamErr := w.streamDir(dir)
		if streamErr != nil {
			return streamErr
		}
		if w.stopped {
			return nil
		}
		// Reversed, so the stack pops subdirectories in the order they were read.
		slices.Reverse(subdirs)
		pending = append(pending, subdirs...)
	}
	return nil
}

// streamDir visits one directory's entries in fixed-size batches and reports
// the subdirectories found under it. The handle is closed before any of
// them is opened.
func (w *rootWalk) streamDir(dir string) ([]string, error) {
	// O_DIRECTORY, not root.Open: a reader-less FIFO would block a plain
	// O_RDONLY open(2) forever. O_NONBLOCK covers the same hazard again.
	handle, err := w.root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, w.reportDirFailure(dir, err)
	}
	defer func() { _ = handle.Close() }()

	var subdirs []string
	for {
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			return subdirs, fmt.Errorf("atomicfile: %w", ctxErr)
		}
		entries, readErr := handle.ReadDir(walkReadDirBatch)
		slices.SortFunc(entries, cmpEntryName)
		batch, endDir, visitErr := w.visitBatch(dir, entries)
		subdirs = append(subdirs, batch...)
		if visitErr != nil {
			return nil, visitErr
		}
		if endDir {
			return subdirs, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return subdirs, nil
			}
			return subdirs, w.reportDirFailure(dir, readErr)
		}
	}
}

// visitBatch hands one batch of directory entries to the callback,
// root-relative, and reports the subdirectories among them. endDir is true
// when nothing further in this directory is to be read: fs.SkipAll,
// fs.SkipDir on a non-directory entry, or a callback error.
func (w *rootWalk) visitBatch(dir string, entries []fs.DirEntry) (subdirs []string, endDir bool, err error) {
	for _, entry := range entries {
		rel := entry.Name()
		if dir != "." {
			rel = filepath.Join(dir, entry.Name())
		}
		act, cbErr := classifyWalkAnswer(w.fn(rel, entry, nil))
		switch {
		case cbErr != nil:
			return subdirs, true, cbErr
		case act == walkStop:
			w.stopped = true
			return subdirs, true, nil
		case act == walkSkipDir && !entry.IsDir():
			return subdirs, true, nil
		case act == walkSkipDir:
			continue
		case entry.IsDir():
			// Queued on the DIRENT type, so a symlinked directory is never descended.
			subdirs = append(subdirs, rel)
		}
	}
	return subdirs, false, nil
}

// reportDirFailure hands a directory-level failure to the callback for the
// path it belongs to and translates the answer: nil (or fs.SkipDir) leaves
// the walk running, fs.SkipAll ends it, anything else aborts it.
func (w *rootWalk) reportDirFailure(dir string, err error) error {
	act, cbErr := classifyWalkAnswer(w.fn(dir, nil, err))
	if cbErr != nil {
		return cbErr
	}
	if act == walkStop {
		w.stopped = true
	}
	return nil
}
