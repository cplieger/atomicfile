package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// gk_atomicfile_u1_replaceWithNonEmptyDir deletes the file at path and puts a
// non-empty directory in its place. os.Remove of a non-empty directory fails
// with a non-ErrNotExist error (ENOTEMPTY) on every platform and is NOT
// bypassed by root, so it forces Cleanup's temp-removal to fail without any
// permission tricks.
func gk_atomicfile_u1_replaceWithNonEmptyDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("gk_atomicfile_u1: remove temp %q = %v, want nil", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("gk_atomicfile_u1: mkdir %q = %v, want nil", path, err)
	}
	child := filepath.Join(path, "child")
	if err := os.WriteFile(child, []byte("x"), 0o644); err != nil {
		t.Fatalf("gk_atomicfile_u1: write child %q = %v, want nil", child, err)
	}
}

// TestPendingFileCleanup_gk_atomicfile_u1_RemoveFailureSurfacesError pins that
// (*PendingFile).Cleanup surfaces a temp-removal failure (other than
// ErrNotExist) as a non-nil error. It kills the CONDITIONALS_NEGATION mutant at
// write.go:355:36 — flipping `err != nil` to `err == nil` in
//
//	if err := os.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist)
//
// makes a genuine removal failure (err non-nil) fail the `err == nil` guard,
// short-circuiting the branch so Cleanup returns nil instead of the error.
//
// The existing mutants_test.go kill of this guard relies on a write-blocked
// parent dir (EACCES) and skips under root; this session runs as root, so that
// path is inert. Replacing the temp with a NON-EMPTY directory triggers
// ENOTEMPTY, which root cannot bypass, making the kill root-safe.
func TestPendingFileCleanup_gk_atomicfile_u1_RemoveFailureSurfacesError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")

	pf, err := NewPendingFile(context.Background(), target)
	if err != nil {
		t.Fatalf("NewPendingFile(%q) = %v, want nil", target, err)
	}
	tmpName := pf.Name()

	// Swap the temp file for a non-empty directory at the same path so
	// Cleanup's os.Remove(tmpName) fails with ENOTEMPTY (the live code's
	// pf.Close() of the now-anonymous inode is harmless).
	gk_atomicfile_u1_replaceWithNonEmptyDir(t, tmpName)

	gotErr := pf.Cleanup()

	if gotErr == nil {
		t.Fatalf("Cleanup() (os.Remove hits non-empty dir) = nil, want non-nil error")
	}
	if errors.Is(gotErr, fs.ErrNotExist) {
		t.Fatalf("Cleanup() = %v, want a non-ErrNotExist error (ENOTEMPTY expected)", gotErr)
	}
}

// TestPendingFileCleanup_gk_atomicfile_u1_SuccessReturnsNil pins the opposite
// branch of the same guard: when os.Remove succeeds (err == nil), Cleanup
// returns nil. Paired with the failure test above, it nails both sides of the
// write.go:355 `err != nil` conditional so neither the original nor the mutant
// could satisfy both expectations.
func TestPendingFileCleanup_gk_atomicfile_u1_SuccessReturnsNil(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ok.txt")

	pf, err := NewPendingFile(context.Background(), target)
	if err != nil {
		t.Fatalf("NewPendingFile(%q) = %v, want nil", target, err)
	}
	tmpName := pf.Name()

	if gotErr := pf.Cleanup(); gotErr != nil {
		t.Fatalf("Cleanup() (temp removable) = %v, want nil", gotErr)
	}
	if _, statErr := os.Stat(tmpName); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("Cleanup() left temp %q: Stat err = %v, want ErrNotExist", tmpName, statErr)
	}
}
