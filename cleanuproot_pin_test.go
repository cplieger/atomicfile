package atomicfile

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAgedTemp writes a file of this package's own temp shape under dir and backdates
// it past any plausible cutoff, returning its full path.
func writeAgedTemp(t *testing.T, dir, name string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("orphan"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile(%q): %v", full, err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatalf("setup: Chtimes(%q): %v", full, err)
	}
	return full
}

// TestReapStaleTempInRoot_refuses_an_ancestor_swapped_for_a_symlink pins the window the
// pinned parent closes: an *os.Root confines a path but FOLLOWS an in-tree symlink
// component, so stat-then-unlink on a multi-component name (Lstat(rel) then Remove(rel))
// can be redirected, between those two lookups, at a different file inside the tree if an
// ancestor directory is swapped for a symlink. The victim only needs a temp-shaped name.
//
// The swap is staged where the race puts it: after the walk enumerates old/<temp> as a
// regular file, before the reap runs.
//
// Red-green: against the pre-pin body (Lstat(rel) then Remove(rel)) the Lstat resolves
// through the swapped ancestor and unlinks live/<temp> — both assertions below fail.
func TestReapStaleTempInRoot_refuses_an_ancestor_swapped_for_a_symlink(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	const temp = ".atomicfile-1234512345.tmp"

	// live/ is a directory the sweep never enumerated, holding a file that
	// happens to wear a temp name.
	live := filepath.Join(dir, "live")
	if err := os.Mkdir(live, 0o750); err != nil {
		t.Fatalf("setup: Mkdir(live): %v", err)
	}
	victim := writeAgedTemp(t, live, temp)

	// old/ is the directory the walk enumerated the candidate in; it and its
	// temp are replaced by a symlink to live/, the state a co-mounting writer
	// can stage between the readdir and this reap.
	old := filepath.Join(dir, "old")
	if err := os.Mkdir(old, 0o750); err != nil {
		t.Fatalf("setup: Mkdir(old): %v", err)
	}
	writeAgedTemp(t, old, temp)
	if err := os.RemoveAll(old); err != nil {
		t.Fatalf("setup: RemoveAll(old): %v", err)
	}
	if err := os.Symlink("live", old); err != nil {
		t.Fatalf("setup: Symlink(live -> old): %v", err)
	}

	handler := &captureHandler{}
	didRemove, didFail := reapStaleTempInRoot(root, filepath.Join("old", temp), temp,
		time.Now().Add(-time.Hour), slog.New(handler))

	if didRemove {
		t.Error("reapStaleTempInRoot(ancestor swapped for a symlink) removed something: the unlink was" +
			" redirected through an ancestor the sweep never inspected")
	}
	if !didFail {
		t.Error("reapStaleTempInRoot(ancestor swapped for a symlink) reported no failure: a candidate the" +
			" sweep cannot account for must reach SweepResult.Failed, or orphans accumulate unnoticed")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Errorf("the file behind the swapped ancestor was unlinked (%v): a temp-shaped name under a"+
			" directory the sweep never enumerated must survive", err)
	}
	if n := handler.CountLevelExact(slog.LevelDebug,
		"atomicfile.CleanupStaleTempsInRoot: could not pin the parent directory"); n != 1 {
		t.Errorf("refused pin logged %d times at Debug, want exactly 1: the per-path reason is all the"+
			" Failed count cannot carry", n)
	}
}

// TestReapStaleTempInRoot_ignores_a_vanished_ancestor pins the benign half of the same
// descent: a directory that disappeared with its temp between the walk and the reap is
// an ordinary race and counts as neither a removal nor a failure.
func TestReapStaleTempInRoot_ignores_a_vanished_ancestor(t *testing.T) {
	t.Parallel()
	root, _ := openTestRoot(t)
	const temp = ".atomicfile-5432154321.tmp"

	handler := &captureHandler{}
	didRemove, didFail := reapStaleTempInRoot(root, filepath.Join("gone", temp), temp,
		time.Now().Add(-time.Hour), slog.New(handler))

	if didRemove || didFail {
		t.Errorf("reapStaleTempInRoot(vanished ancestor) = (%v, %v), want (false, false): a path that is"+
			" already gone is neither reclaimed nor a failure", didRemove, didFail)
	}
	if n := len(handler.Records()); n != 0 {
		t.Errorf("reapStaleTempInRoot(vanished ancestor) logged %d records, want none", n)
	}
}

// TestCleanupStaleTempsInRoot_reaps_a_nested_temp_through_the_pin is the end-to-end half:
// the pinned descent must not cost the recursive sweep its reach. A temp nested two
// directories deep is still reclaimed, while a temp-shaped file under a SYMLINKED
// directory is left alone since the walk never descends a symlink.
func TestCleanupStaleTempsInRoot_reaps_a_nested_temp_through_the_pin(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	nested := filepath.Join(dir, "example.com", "deeper")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("setup: MkdirAll: %v", err)
	}
	reaped := writeAgedTemp(t, nested, ".atomicfile-2233445566.tmp")

	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o750); err != nil {
		t.Fatalf("setup: Mkdir(elsewhere): %v", err)
	}
	spared := writeAgedTemp(t, elsewhere, ".atomicfile-6655443322.tmp")
	if err := os.Symlink("elsewhere", filepath.Join(dir, "linked")); err != nil {
		t.Fatalf("setup: Symlink: %v", err)
	}

	got, err := CleanupStaleTempsInRoot(t.Context(), root, time.Hour, WithRecursive(true))
	if err != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", err)
	}
	if got.Removed != 2 {
		t.Errorf("Removed = %d, want 2: the nested temp and the one under the real directory the symlink"+
			" points at, each enumerated exactly once", got.Removed)
	}
	if got.Failed != 0 || got.Unreadable != 0 {
		t.Errorf("Failed = %d, Unreadable = %d, want zeros on a tree the sweep can read whole",
			got.Failed, got.Unreadable)
	}
	for _, gone := range []string{reaped, spared} {
		if _, statErr := os.Lstat(gone); statErr == nil {
			t.Errorf("%s survived a recursive sweep", gone)
		}
	}
	// The link itself is not a candidate and must still be there, unfollowed.
	fi, statErr := os.Lstat(filepath.Join(dir, "linked"))
	if statErr != nil {
		t.Fatalf("os.Lstat(linked) = %v, want the symlink left in place", statErr)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("linked mode = %s, want a symlink: the sweep must not replace or resolve it", fi.Mode())
	}
}
