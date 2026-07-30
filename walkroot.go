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

// walkReadDirBatch is how many directory entries one ReadDir call takes at a time.
//
// It is the whole point of WalkDirInRoot: fs.WalkDir reads and SORTS a directory's
// complete inventory before it calls back even once, so a caller sweeping a tree it
// does not own has that inventory in memory before its own callback can refuse
// anything. A fixed batch bounds the per-directory allocation to this many entries
// whatever the directory holds, and lets the callback act on the entries actually
// taken.
//
// Not configurable. The number is a memory bound, not a policy: a caller that wants
// to stop early returns an error (or fs.SkipAll) from its callback, which is where
// that decision belongs.
const walkReadDirBatch = 256

// WalkDirInRoot enumerates root's tree and hands every entry to fn, root-relative,
// confining the traversal to root's tree: every step is an openat-relative syscall,
// no ambient path is ever constructed, and a symlinked directory is never descended.
//
// It is the traversal counterpart to WriteFileInRoot and ReadBoundedInRoot. Without
// it, a caller that writes and reads through a confined root has to hand-roll the
// enumeration half — fs.WalkDir over root.FS(), or its own ReadDir loop — and both
// spellings re-derive the same three non-obvious details:
//
//   - Memory. fs.WalkDir materializes and sorts each directory's whole inventory
//     before the first callback, so a hostile or merely large directory is already
//     resident by the time the caller could refuse it. This reads in fixed batches
//     (walkReadDirBatch) and calls back on each one.
//   - Descriptors. Exactly ONE directory handle is open at a time: a directory's own
//     entries are all visited, and its handle closed, before any subdirectory of it
//     is opened. What the walk carries between directories is a queue of NAMES, which
//     cannot exhaust the process's descriptors on a deep tree.
//   - Occupants. Each directory is opened O_DIRECTORY, so a named pipe swapped in for
//     a directory between the readdir that classified it and this open is REFUSED with
//     ENOTDIR instead of blocking: open(2) on a FIFO with no writer blocks
//     indefinitely, which for a single-goroutine sweep is a permanent hang. It is the
//     same guarantee ReadBoundedInRoot gives every file it opens; this is the
//     directory half.
//
// # Callback contract
//
// fn is an fs.WalkDirFunc and is called exactly as fs.WalkDir calls one: the root
// itself first as ".", then each entry pre-order, with a directory that cannot be
// opened or finished reported through fn for ITS OWN path (d nil, err set). The
// fs.SkipDir and fs.SkipAll sentinels mean what they mean in fs.WalkDir — SkipDir on a
// directory skips its contents, SkipDir on a non-directory skips the rest of the
// containing directory, SkipAll ends the walk without an error — and any other error
// from fn ends the walk and is returned. Whether a read failure below the root is
// fatal is likewise the callback's call: return the error to abort, or nil to count an
// unreadable sub-path and carry on with the rest of the tree.
//
// # Two deliberate differences from fs.WalkDir
//
// Entries arrive in DIRECTORY order, sorted only within a batch, because a global sort
// is exactly the materialization this avoids. Do not build a decision on the order; a
// caller that needs a total order sorts what it collected. And a directory's own
// entries are all visited before the walk descends into its subdirectories, which is
// what buys the single-descriptor property above.
//
// # Cancellation
//
// The walk stops between batches once ctx is done, and an already-cancelled context
// does no work, so a walk of a large tree cannot hold up shutdown. That bound is at
// most one batch of entries; a caller that wants per-entry cancellation checks ctx in
// fn, which every visitor already positioned to abort on shutdown does anyway.
//
// The caller owns root; WalkDirInRoot does not close it. Pass root.OpenRoot(sub) to
// walk a subtree.
func WalkDirInRoot(ctx context.Context, root *os.Root, fn fs.WalkDirFunc) error {
	if root == nil {
		return errors.New("atomicfile: nil root")
	}
	if fn == nil {
		return errors.New("atomicfile: nil walk function")
	}
	w := &rootWalk{ctx: ctx, root: root, fn: fn}
	err := w.run()
	// Normalized exactly as fs.WalkDir normalizes its own return: the sentinels are
	// instructions to the walk, never outcomes a caller has to handle.
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

// rootWalk carries one WalkDirInRoot traversal: the confined root, the callback, and
// the one bit of state a nested helper has to report upward (fs.SkipAll was asked
// for), so each step is a named method rather than a closure over five variables.
type rootWalk struct {
	ctx     context.Context
	root    *os.Root
	fn      fs.WalkDirFunc
	stopped bool
}

// walkAction is what one callback answer means for the traversal, so each call site
// reads as a decision rather than as a chain of sentinel comparisons.
type walkAction int

const (
	// walkContinue: the callback accepted the entry.
	walkContinue walkAction = iota
	// walkSkipDir: fs.SkipDir. On a directory entry, do not descend; on anything
	// else, stop reading the containing directory.
	walkSkipDir
	// walkStop: fs.SkipAll. End the walk with no error.
	walkStop
)

// classifyWalkAnswer splits a callback's return into a traversal action and a real
// error. Only the second is ever propagated to the caller of WalkDirInRoot.
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

// cmpEntryName orders two directory entries by name, the comparison
// slices.SortFunc needs for the per-batch sort.
func cmpEntryName(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) }

// run visits the root and then streams every directory reachable from it, keeping the
// pending set as a stack of NAMES so no ancestor handle stays open during the descent.
func (w *rootWalk) run() error {
	if err := w.ctx.Err(); err != nil {
		return fmt.Errorf("atomicfile: %w", err)
	}
	fi, err := w.root.Stat(".")
	if err != nil {
		// Reported for the root's own path and returned as the callback answers it,
		// the same contract fs.WalkDir gives a failed Stat of its starting point.
		return w.fn(".", nil, err)
	}
	act, cbErr := classifyWalkAnswer(w.fn(".", fs.FileInfoToDirEntry(fi), nil))
	if cbErr != nil {
		return cbErr
	}
	if act != walkContinue {
		// SkipDir on the root skips the whole tree, and SkipAll ends the walk; either
		// way nothing else is left to visit.
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
		// Reversed, so the stack pops the subdirectories in the order they were read.
		slices.Reverse(subdirs)
		pending = append(pending, subdirs...)
	}
	return nil
}

// streamDir visits one directory's entries in fixed-size batches and reports the
// subdirectories found under it, for run to stream in turn. The handle is closed
// before any of them is opened.
//
// A directory that cannot be opened or read is reported through the callback for its
// OWN path, which is where fs.WalkDir reports it too, and the callback decides whether
// that ends the walk.
func (w *rootWalk) streamDir(dir string) ([]string, error) {
	// O_DIRECTORY, not root.Open: Open is a plain O_RDONLY openat, and a reader-less
	// FIFO blocks that open(2) forever. O_NONBLOCK is belt and braces on the same
	// hazard and has no effect on a directory. See WalkDirInRoot's "Occupants".
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
		// Sorted within the batch only: it costs nothing at this size and keeps a
		// single directory's per-entry diagnostics stable from walk to walk.
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
			// The entries already read are kept: they were visited, and the failure
			// costs the REST of this directory, not what the walk has seen of it.
			return subdirs, w.reportDirFailure(dir, readErr)
		}
	}
}

// visitBatch hands one batch of directory entries to the callback, root-relative, and
// reports the subdirectories among them for the caller to stream once this directory's
// handle is closed. endDir is true when nothing further in this directory is to be
// read: fs.SkipAll, fs.SkipDir on a non-directory entry, or a callback error.
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
			// fs.WalkDir's contract for SkipDir on a non-directory: the rest of the
			// containing directory is skipped, which here also means its remaining
			// subdirectories are never queued.
			return subdirs, true, nil
		case act == walkSkipDir:
			continue
		case entry.IsDir():
			// Queued on the DIRENT type, which is false for a symlink, so a symlinked
			// directory is never descended.
			subdirs = append(subdirs, rel)
		}
	}
	return subdirs, false, nil
}

// reportDirFailure hands a directory-level failure to the callback for the path it
// belongs to and translates the answer: nil (or fs.SkipDir) leaves the walk running
// with the rest of the tree, fs.SkipAll ends it, anything else aborts it.
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
