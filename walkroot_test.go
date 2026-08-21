package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// collectWalk records what a WalkDirInRoot callback was handed, which is the whole
// observable surface of the walk: which paths, in a set, and which of them arrived as a
// failure.
type collectWalk struct {
	visited []string
	failed  []string
}

func (c *collectWalk) visit(rel string, _ fs.DirEntry, err error) error {
	c.visited = append(c.visited, rel)
	if err != nil {
		c.failed = append(c.failed, rel)
	}
	return nil
}

// has reports whether rel was visited.
func (c *collectWalk) has(rel string) bool { return slices.Contains(c.visited, rel) }

// countVisits returns how many times rel was handed to the callback.
func (c *collectWalk) countVisits(rel string) int {
	n := 0
	for _, got := range c.visited {
		if got == rel {
			n++
		}
	}
	return n
}

// TestWalkDirInRoot_streams_a_directory_larger_than_one_batch pins the property the
// batched read exists for: a directory bigger than one ReadDir batch must still be
// enumerated completely and exactly once, at every depth.
//
// Batching is what keeps a large or hostile directory from being materialized (and
// sorted) before the caller's callback can refuse anything, and it only helps if the
// loop actually spans batches without dropping or repeating an entry.
func TestWalkDirInRoot_streams_a_directory_larger_than_one_batch(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	const flat = walkReadDirBatch + 37
	for i := range flat {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%04d", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: WriteFile: %v", err)
		}
	}
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatalf("setup: Mkdir: %v", err)
	}
	const deep = 5
	for i := range deep {
		if err := os.WriteFile(filepath.Join(nested, fmt.Sprintf("deep-%d", i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: WriteFile: %v", err)
		}
	}

	var got collectWalk
	if err := WalkDirInRoot(t.Context(), root, got.visit); err != nil {
		t.Fatalf("WalkDirInRoot(tree spanning several read batches) = %v, want nil", err)
	}
	// The root, every flat entry, the nested directory, and everything inside it.
	if want := 1 + flat + 1 + deep; len(got.visited) != want {
		t.Errorf("WalkDirInRoot visited %d paths, want %d: a batched read must reach every entry of"+
			" every directory exactly once", len(got.visited), want)
	}
	if len(got.failed) != 0 {
		t.Errorf("WalkDirInRoot reported failures %v on a clean tree, want none", got.failed)
	}
	for _, rel := range []string{".", "entry-0000", "entry-0292", "nested", filepath.Join("nested", "deep-4")} {
		if n := got.countVisits(rel); n != 1 {
			t.Errorf("WalkDirInRoot visited %q %d times, want exactly 1", rel, n)
		}
	}
}

// TestWalkDirInRoot_does_not_descend_a_symlinked_directory pins the confinement half a
// caller cannot see: a symlink to a directory is REPORTED as an entry and never entered,
// so nothing under it is enumerated under a path that does not physically hold it.
//
// It matters most for a sweep: a walk that followed the link would report the same file
// under two paths, and the second one is a path whose ancestors are not what they look
// like — exactly the redirection OpenParentInRoot exists to refuse. Queuing on the
// DIRENT type (never fs.Stat) is what buys it.
func TestWalkDirInRoot_does_not_descend_a_symlinked_directory(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatalf("setup: Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatalf("setup: Symlink: %v", err)
	}

	var got collectWalk
	if err := WalkDirInRoot(t.Context(), root, got.visit); err != nil {
		t.Fatalf("WalkDirInRoot(tree with a symlinked directory) = %v, want nil", err)
	}
	if !got.has("link") {
		t.Errorf("WalkDirInRoot did not report the symlink itself; visited %v", got.visited)
	}
	if !got.has(filepath.Join("real", "inside")) {
		t.Errorf("WalkDirInRoot did not reach the real directory's entry; visited %v", got.visited)
	}
	if got.has(filepath.Join("link", "inside")) {
		t.Errorf("WalkDirInRoot descended a symlinked directory and reported %q: a file must never be"+
			" enumerated under a path whose ancestors do not physically hold it; visited %v",
			filepath.Join("link", "inside"), got.visited)
	}
}

// TestWalkDirInRoot_refuses_a_fifo_in_a_directory_position pins the directory half of
// the guarantee ReadBoundedInRoot gives every file: a reader-less FIFO must be REFUSED,
// never waited on.
//
// The walk queues a subdirectory on the readdir dirent type, so a hostile occupant only
// has to be swapped in between that readdir and the open — a window available to anything
// with write access to a co-mounted tree. os.Root.Open is a plain O_RDONLY openat and
// open(2) on a reader-less FIFO blocks forever, wedging the caller's goroutine for as
// long as nobody opens the other end. O_DIRECTORY makes the kernel answer ENOTDIR first,
// so the path is reported through the callback like any other directory the walk cannot
// enter.
//
// The walk itself would never queue a FIFO, so streamDir is driven directly with its
// path: that IS the state the race produces, a name an earlier readdir classified as a
// directory.
func TestWalkDirInRoot_refuses_a_fifo_in_a_directory_position(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatalf("setup: Mkfifo: %v", err)
	}

	type outcome struct {
		got *collectWalk
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		var got collectWalk
		w := &rootWalk{ctx: t.Context(), root: root, fn: got.visit}
		_, streamErr := w.streamDir("pipe")
		done <- outcome{&got, streamErr}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("streamDir(FIFO) = %v, want nil: the callback swallowed the failure, so the walk continues", res.err)
		}
		if len(res.got.failed) != 1 || res.got.failed[0] != "pipe" {
			t.Errorf("streamDir(FIFO) reported failures %v, want exactly [\"pipe\"]: a non-directory"+
				" occupant is one unenterable sub-path", res.got.failed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streamDir blocked on a reader-less FIFO: the O_DIRECTORY open regressed, and a planted" +
			" pipe now wedges the calling goroutine indefinitely")
	}
}

// TestWalkDirInRoot_reports_an_unreadable_directory_and_continues pins the two-level
// error contract: a directory that cannot be opened is reported through the callback for
// ITS OWN path, and a callback that answers nil keeps the walk running over the rest of
// the tree. A partial enumeration is strictly better than none, and the callback is what
// decides that.
func TestWalkDirInRoot_reports_an_unreadable_directory_and_continues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	root, dir := openTestRoot(t)
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o000); err != nil {
		t.Fatalf("setup: Mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })
	if err := os.WriteFile(filepath.Join(dir, "readable"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}

	var got collectWalk
	if err := WalkDirInRoot(t.Context(), root, got.visit); err != nil {
		t.Fatalf("WalkDirInRoot(tree with an unreadable directory) = %v, want nil", err)
	}
	if len(got.failed) != 1 || got.failed[0] != "blocked" {
		t.Errorf("WalkDirInRoot reported failures %v, want exactly [\"blocked\"]", got.failed)
	}
	// Reported once for its own path, and visited once as its parent's entry.
	if n := got.countVisits("blocked"); n != 2 {
		t.Errorf("WalkDirInRoot visited %q %d times, want 2 (once as an entry, once carrying the open"+
			" failure): a caller counting sub-paths must be able to tell those apart by the error", "blocked", n)
	}
	if !got.has("readable") {
		t.Errorf("WalkDirInRoot stopped at the unreadable directory; visited %v", got.visited)
	}
}

// twoSubdirTree builds a root holding two subdirectories, the second with one file in it,
// so a walk that loses the first must still be able to reach the second. It returns the
// root and the ambient path of the subdirectory a test is expected to remove.
func twoSubdirTree(t *testing.T) (root *os.Root, doomed string) {
	t.Helper()
	root, dir := openTestRoot(t)
	for _, sub := range []string{"a-dir", "b-dir"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatalf("setup: Mkdir(%s) = %v, want nil", sub, err)
		}
	}
	inside := filepath.Join(dir, "b-dir", "inside")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile(%s) = %v, want nil", inside, err)
	}
	return root, filepath.Join(dir, "a-dir")
}

// TestWalkDirInRoot_continues_past_a_directory_that_vanished_mid_walk pins the same
// two-level error contract as the test above, through a failure the walk cannot avoid: a
// subdirectory is queued by NAME and opened only after its parent's handle is closed, so
// one removed in that window is reported through the callback for ITS OWN path, and a
// callback that answers nil keeps the rest of the tree running.
//
// It is the witness that holds whatever UID the suite runs as. The permission fixture
// above is skipped for root, which is what every container in this fleet runs as, so
// without this the contract is unguarded exactly where it is measured. A directory that
// disappears underneath the walk is also the likelier production shape: a co-mounting
// writer reorganizing a tree while a sweep enumerates it.
func TestWalkDirInRoot_continues_past_a_directory_that_vanished_mid_walk(t *testing.T) {
	t.Parallel()
	root, doomed := twoSubdirTree(t)

	var got collectWalk
	err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
		if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
			return visitErr
		}
		if rel == "a-dir" && walkErr == nil {
			if rmErr := os.Remove(doomed); rmErr != nil {
				t.Errorf("remove %q between its readdir and its open = %v, want nil", doomed, rmErr)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDirInRoot(subdirectory removed mid-walk) = %v, want nil: the callback answered"+
			" nil, so the walk owns the rest of the tree", err)
	}
	if len(got.failed) != 1 || got.failed[0] != "a-dir" {
		t.Errorf("WalkDirInRoot reported failures %v, want exactly [\"a-dir\"]", got.failed)
	}
	if !got.has(filepath.Join("b-dir", "inside")) {
		t.Errorf("WalkDirInRoot visited %v after a sub-path it could not enter, want the rest of the"+
			" tree too: a partial enumeration is what the callback asked for by answering nil", got.visited)
	}
}

// TestWalkDirInRoot_aborts_when_the_callback_refuses_a_directory_failure pins the other
// half of that same decision. Whether an unenterable sub-path is fatal belongs to the
// callback, so an error returned from the failure report ends the walk and reaches the
// caller unchanged — a caller that treats a missed directory as fatal must not be handed
// a nil error and a partial enumeration it cannot tell from a complete one.
func TestWalkDirInRoot_aborts_when_the_callback_refuses_a_directory_failure(t *testing.T) {
	t.Parallel()
	root, doomed := twoSubdirTree(t)
	sentinel := errors.New("this sub-path is not optional")

	var got collectWalk
	err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
		if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
			return visitErr
		}
		if walkErr != nil {
			return sentinel
		}
		if rel == "a-dir" {
			if rmErr := os.Remove(doomed); rmErr != nil {
				t.Errorf("remove %q between its readdir and its open = %v, want nil", doomed, rmErr)
			}
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("WalkDirInRoot(callback refused a directory failure) = %v, want the callback's own error", err)
	}
	if got.has(filepath.Join("b-dir", "inside")) {
		t.Errorf("WalkDirInRoot visited %v after the callback refused the failure, want the walk"+
			" stopped there", got.visited)
	}
}

// TestWalkDirInRoot_honours_the_walk_sentinels pins that fs.SkipDir and fs.SkipAll mean
// what they mean in fs.WalkDir. The callback type IS fs.WalkDirFunc, so a caller reusing
// a visitor written for fs.WalkDir must not have its sentinel silently treated as a
// fatal error (or, worse, ignored).
func TestWalkDirInRoot_honours_the_walk_sentinels(t *testing.T) {
	t.Parallel()

	// A tree with two subdirectories, each holding one file, plus two files at the top.
	setup := func(t *testing.T) *os.Root {
		t.Helper()
		root, dir := openTestRoot(t)
		for _, sub := range []string{"a-dir", "b-dir"} {
			if err := os.Mkdir(filepath.Join(dir, sub), 0o750); err != nil {
				t.Fatalf("setup: Mkdir(%s): %v", sub, err)
			}
			if err := os.WriteFile(filepath.Join(dir, sub, "child"), []byte("x"), 0o600); err != nil {
				t.Fatalf("setup: WriteFile(%s/child): %v", sub, err)
			}
		}
		for _, name := range []string{"c-file", "d-file"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
				t.Fatalf("setup: WriteFile(%s): %v", name, err)
			}
		}
		return root
	}

	t.Run("SkipDir on a directory skips its contents", func(t *testing.T) {
		t.Parallel()
		root := setup(t)
		var got collectWalk
		err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
			if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
				return visitErr
			}
			if rel == "a-dir" {
				return fs.SkipDir
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDirInRoot(SkipDir on a directory) = %v, want nil", err)
		}
		if got.has(filepath.Join("a-dir", "child")) {
			t.Errorf("SkipDir on %q still descended into it; visited %v", "a-dir", got.visited)
		}
		if !got.has(filepath.Join("b-dir", "child")) {
			t.Errorf("SkipDir on %q also skipped a sibling subtree; visited %v", "a-dir", got.visited)
		}
	})

	t.Run("SkipDir on a file skips the rest of its directory", func(t *testing.T) {
		t.Parallel()
		root := setup(t)
		var got collectWalk
		err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
			if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
				return visitErr
			}
			if rel == "c-file" {
				return fs.SkipDir
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDirInRoot(SkipDir on a file) = %v, want nil", err)
		}
		if got.has("d-file") {
			t.Errorf("SkipDir on a non-directory did not skip the rest of the containing directory;"+
				" visited %v", got.visited)
		}
	})

	t.Run("SkipAll ends the walk without an error", func(t *testing.T) {
		t.Parallel()
		root := setup(t)
		var got collectWalk
		err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
			if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
				return visitErr
			}
			if rel == "a-dir" {
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDirInRoot(SkipAll) = %v, want nil: the sentinel is an instruction, not a failure", err)
		}
		if got.has(filepath.Join("a-dir", "child")) || got.has("d-file") {
			t.Errorf("SkipAll did not stop the walk; visited %v", got.visited)
		}
	})

	t.Run("SkipDir on the root skips the whole tree", func(t *testing.T) {
		t.Parallel()
		root := setup(t)
		var got collectWalk
		err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
			if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
				return visitErr
			}
			return fs.SkipDir
		})
		if err != nil {
			t.Fatalf("WalkDirInRoot(SkipDir on the root) = %v, want nil", err)
		}
		if len(got.visited) != 1 || got.visited[0] != "." {
			t.Errorf("WalkDirInRoot visited %v, want exactly [\".\"]", got.visited)
		}
	})
}

// TestWalkDirInRoot_propagates_a_callback_error pins that a real error from the callback
// — the shape every cancellation and every budget abort takes — stops the walk and
// reaches the caller unchanged, so a caller cannot mistake an aborted enumeration for a
// complete one.
func TestWalkDirInRoot_propagates_a_callback_error(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("setup: Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}
	sentinel := errors.New("stop here")

	var got collectWalk
	err := WalkDirInRoot(t.Context(), root, func(rel string, d fs.DirEntry, walkErr error) error {
		if visitErr := got.visit(rel, d, walkErr); visitErr != nil {
			return visitErr
		}
		if rel == "sub" {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("WalkDirInRoot(callback error) = %v, want the callback's own error", err)
	}
	if got.has(filepath.Join("sub", "child")) {
		t.Errorf("WalkDirInRoot kept walking after the callback failed; visited %v", got.visited)
	}
}

// TestWalkDirInRoot_cancelled pins that an already-cancelled context does no work: the
// callback is never called, so a walk started on the way out cannot traverse a large
// tree while shutdown waits.
func TestWalkDirInRoot_cancelled(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var got collectWalk
	err := WalkDirInRoot(ctx, root, got.visit)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WalkDirInRoot(cancelled) = %v, want context.Canceled", err)
	}
	if len(got.visited) != 0 {
		t.Errorf("WalkDirInRoot(cancelled) visited %v, want nothing", got.visited)
	}
}

func TestWalkDirInRoot_rejects_a_nil_argument(t *testing.T) {
	t.Parallel()
	root, _ := openTestRoot(t)
	var got collectWalk
	if err := WalkDirInRoot(t.Context(), nil, got.visit); err == nil {
		t.Error("WalkDirInRoot(nil root) = nil, want a rejection")
	}
	if err := WalkDirInRoot(t.Context(), root, nil); err == nil {
		t.Error("WalkDirInRoot(nil callback) = nil, want a rejection")
	}
}
