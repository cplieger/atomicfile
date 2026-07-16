package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewPendingFileInRoot(t *testing.T) {
	t.Parallel()

	t.Run("commit_writes_and_renames_within_root", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		pf, err := NewPendingFileInRoot(context.Background(), root, "out.txt")
		if err != nil {
			t.Fatalf("NewPendingFileInRoot: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.WriteString("pending in root"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		res, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if want := filepath.Join(dir, "out.txt"); res.Path != want {
			t.Errorf("Result.Path = %q, want %q", res.Path, want)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true on a healthy filesystem")
		}
		assertContent(t, filepath.Join(dir, "out.txt"), "pending in root")
		assertNoTempLeak(t, dir)
	})

	t.Run("temp_name_matches_janitor_shape", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		pf, err := NewPendingFileInRoot(context.Background(), root, "shaped.txt")
		if err != nil {
			t.Fatalf("NewPendingFileInRoot: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		base := filepath.Base(pf.Name())
		if !isStaleTempName(base) {
			t.Errorf("temp base name %q would not be reaped by CleanupStaleTemps", base)
		}
		// The embedded Name() must point at the real staged file so external
		// verifiers can inspect it before Commit (the pg-autodump pattern).
		if _, statErr := os.Stat(pf.Name()); statErr != nil {
			t.Errorf("Stat(pf.Name()) = %v, want the staged temp visible at %q", statErr, pf.Name())
		}
		_ = dir
	})

	t.Run("cleanup_removes_temp_and_commit_aborts", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		pf, err := NewPendingFileInRoot(context.Background(), root, "aborted.txt")
		if err != nil {
			t.Fatalf("NewPendingFileInRoot: %v", err)
		}
		if _, err := pf.WriteString("doomed"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		if err := pf.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if res, commitErr := pf.Commit(context.Background()); !errors.Is(commitErr, ErrAborted) || res != (Result{}) {
			t.Fatalf("Commit after Cleanup = (%+v, %v), want (zero, ErrAborted)", res, commitErr)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "aborted.txt")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("final file exists after Cleanup: %v", statErr)
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("caller_root_stays_open_after_commit_and_cleanup", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)

		pf, err := NewPendingFileInRoot(context.Background(), root, "first.txt")
		if err != nil {
			t.Fatalf("NewPendingFileInRoot: %v", err)
		}
		if _, err := pf.WriteString("one"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		pf2, err := NewPendingFileInRoot(context.Background(), root, "second.txt")
		if err != nil {
			t.Fatalf("NewPendingFileInRoot after Commit: %v (caller root must stay open)", err)
		}
		if err := pf2.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}

		// Still usable after a Cleanup terminal state too.
		if _, err := WriteFileInRoot(context.Background(), root, "third.txt", []byte("three")); err != nil {
			t.Fatalf("WriteFileInRoot after pending Cleanup: %v (caller root must stay open)", err)
		}
		assertContent(t, filepath.Join(dir, "first.txt"), "one")
		assertContent(t, filepath.Join(dir, "third.txt"), "three")
	})

	t.Run("rejects_escaping_name", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		if _, err := NewPendingFileInRoot(context.Background(), root, "../pwned"); err == nil {
			t.Fatal("NewPendingFileInRoot(\"../pwned\") = nil, want confinement error")
		}
	})

	t.Run("rejects_absolute_name", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		if _, err := NewPendingFileInRoot(context.Background(), root, "/etc/passwd"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("nil_root_returns_ErrUnsafePath", func(t *testing.T) {
		t.Parallel()
		if _, err := NewPendingFileInRoot(context.Background(), nil, "f"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("refuses_symlink_target_by_default", func(t *testing.T) {
		if isWindows() {
			t.Skip("symlink semantics differ on Windows")
		}
		t.Parallel()
		root, dir := openTestRoot(t)
		if err := os.WriteFile(filepath.Join(dir, "real"), []byte("real"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		if _, err := NewPendingFileInRoot(context.Background(), root, "link"); !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("err = %v, want ErrSymlinkTarget", err)
		}
	})

	t.Run("mkdir_mode_creates_parents_inside_root", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		pf, err := NewPendingFileInRoot(context.Background(), root, "nested/deep/out.txt", WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("NewPendingFileInRoot: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.WriteString("nested"); err != nil {
			t.Fatalf("WriteString: %v", err)
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		assertContent(t, filepath.Join(dir, "nested", "deep", "out.txt"), "nested")
	})

	t.Run("streaming_readfrom_then_verify_then_commit", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		pf, err := NewPendingFileInRoot(context.Background(), root, "streamed.txt")
		if err != nil {
			t.Fatalf("NewPendingFileInRoot: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.ReadFrom(strings.NewReader("streamed payload")); err != nil {
			t.Fatalf("ReadFrom: %v", err)
		}
		// Verify the staged bytes through the embedded file before committing —
		// the verify-before-publish pattern PendingFile exists for.
		if _, err := pf.Seek(0, 0); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		staged, err := ReadBoundedFile(context.Background(), pf.File, 1<<10)
		if err != nil {
			t.Fatalf("ReadBoundedFile(staged): %v", err)
		}
		if string(staged) != "streamed payload" {
			t.Fatalf("staged content = %q, want %q", staged, "streamed payload")
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		assertContent(t, filepath.Join(dir, "streamed.txt"), "streamed payload")
	})
}
