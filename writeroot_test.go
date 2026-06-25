package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileInRoot(t *testing.T) {
	t.Parallel()

	t.Run("writes_and_renames_within_root", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		res, err := WriteFileInRoot(context.Background(), root, "out.pfx", []byte("payload"))
		if err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		assertContent(t, filepath.Join(dir, "out.pfx"), "payload")
		if !res.Durable {
			t.Errorf("Durable = false, want true on a healthy filesystem")
		}
		if res.Path != filepath.Join(dir, "out.pfx") {
			t.Errorf("Path = %q, want %q", res.Path, filepath.Join(dir, "out.pfx"))
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("cleans_internal_dotdot_to_in_root_target", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		// "a/../b.txt" cleans to "b.txt"; the write stays in root.
		if _, err := WriteFileInRoot(context.Background(), root, "a/../b.txt", []byte("hi")); err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		assertContent(t, filepath.Join(dir, "b.txt"), "hi")
		assertNoTempLeak(t, dir)
	})

	t.Run("creates_parent_with_mkdir_mode", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		_, err := WriteFileInRoot(context.Background(), root, "nested/deep/out.pfx",
			[]byte("p"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		assertContent(t, filepath.Join(dir, "nested", "deep", "out.pfx"), "p")
	})

	t.Run("missing_parent_without_mkdir_is_error", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		if _, err := WriteFileInRoot(context.Background(), root, "nope/out.pfx", []byte("p")); err == nil {
			t.Fatal("WriteFileInRoot into missing dir = nil, want error")
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("applies_with_mode", func(t *testing.T) {
		if isWindows() {
			t.Skip("POSIX mode bits not meaningful on Windows")
		}
		t.Parallel()
		root, dir := openTestRoot(t)
		if _, err := WriteFileInRoot(context.Background(), root, "secret",
			[]byte("s"), WithMode(0o600)); err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		fi, err := os.Stat(filepath.Join(dir, "secret"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("no_sync_is_not_durable", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		res, err := WriteFileInRoot(context.Background(), root, "f", []byte("x"), WithNoSync())
		if err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		if res.Durable {
			t.Errorf("Durable = true under WithNoSync, want false")
		}
		assertContent(t, filepath.Join(dir, "f"), "x")
	})

	t.Run("preserve_mode_reuses_existing_target_mode", func(t *testing.T) {
		if isWindows() {
			t.Skip("POSIX mode bits not meaningful on Windows")
		}
		t.Parallel()
		root, dir := openTestRoot(t)
		if err := os.WriteFile(filepath.Join(dir, "t"), []byte("old"), 0o640); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := WriteFileInRoot(context.Background(), root, "t",
			[]byte("new"), WithMode(0o600), WithPreserveMode()); err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		fi, err := os.Stat(filepath.Join(dir, "t"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("mode = %v, want preserved 0640", fi.Mode().Perm())
		}
	})
}

func TestWriteReaderInRoot(t *testing.T) {
	t.Parallel()

	t.Run("writer_to_fast_path", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		// bytes.Reader implements io.WriterTo.
		if _, err := WriteReaderInRoot(context.Background(), root, "wt", bytes.NewReader([]byte("fast"))); err != nil {
			t.Fatalf("WriteReaderInRoot: %v", err)
		}
		assertContent(t, filepath.Join(dir, "wt"), "fast")
	})

	t.Run("plain_reader_copy_path", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		// plainReader hides io.WriterTo, forcing the io.Copy path.
		r := plainReader{r: bytes.NewReader([]byte("copied"))}
		if _, err := WriteReaderInRoot(context.Background(), root, "cp", r); err != nil {
			t.Fatalf("WriteReaderInRoot: %v", err)
		}
		assertContent(t, filepath.Join(dir, "cp"), "copied")
	})

	t.Run("reader_error_removes_temp_and_leaves_no_target", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		r := plainReader{r: &errReader{n: 4, err: errors.New("simulated IO error")}}
		_, err := WriteReaderInRoot(context.Background(), root, "broken", r)
		var we *WriteError
		if !errors.As(err, &we) || we.Phase != PhaseTempWrite {
			t.Fatalf("err = %v, want WriteError{PhaseTempWrite}", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "broken")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("target exists after failed write: %v", statErr)
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("writer_to_error_removes_temp", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		_, err := WriteReaderInRoot(context.Background(), root, "broken", &errWriterTo{err: errors.New("WriterTo failure")})
		var we *WriteError
		if !errors.As(err, &we) || we.Phase != PhaseTempWrite {
			t.Fatalf("err = %v, want WriteError{PhaseTempWrite}", err)
		}
		assertNoTempLeak(t, dir)
	})
}

// TestWriteInRoot_Confinement is the security heart of the seam: a name that
// escapes the root — via parent traversal or a planted symlink that points
// outside — must be refused, and nothing may land outside the tree.
func TestWriteInRoot_Confinement(t *testing.T) {
	t.Parallel()

	t.Run("rejects_parent_traversal_escape", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		if _, err := WriteFileInRoot(context.Background(), root, "../pwned", []byte("x")); err == nil {
			t.Fatal("WriteFileInRoot(\"../pwned\") = nil, want confinement error")
		}
	})

	t.Run("rejects_write_through_symlink_escaping_root", func(t *testing.T) {
		if isWindows() {
			t.Skip("symlink semantics differ on Windows")
		}
		t.Parallel()
		outside := t.TempDir()
		root, dir := openTestRoot(t)
		if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		_, err := WriteFileInRoot(context.Background(), root, "evil/pwned",
			[]byte("x"), WithMkdirMode(0o755))
		if err == nil {
			t.Fatal("write through escaping symlink = nil, want confinement error")
		}
		if _, statErr := os.Stat(filepath.Join(outside, "pwned")); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("data escaped the root into %q: %v", outside, statErr)
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
		_, err := WriteFileInRoot(context.Background(), root, "link", []byte("x"))
		if !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("err = %v, want ErrSymlinkTarget", err)
		}
		// The symlink target must be untouched.
		assertContent(t, filepath.Join(dir, "real"), "real")
	})

	t.Run("allows_symlink_target_with_option", func(t *testing.T) {
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
		if _, err := WriteFileInRoot(context.Background(), root, "link",
			[]byte("replaced"), WithAllowSymlinkTarget()); err != nil {
			t.Fatalf("WriteFileInRoot: %v", err)
		}
		// The rename replaced the link with a regular file; the target is intact.
		assertContent(t, filepath.Join(dir, "link"), "replaced")
		assertContent(t, filepath.Join(dir, "real"), "real")
	})
}

func TestWriteInRoot_Validation(t *testing.T) {
	t.Parallel()

	t.Run("nil_root", func(t *testing.T) {
		t.Parallel()
		if _, err := WriteFileInRoot(context.Background(), nil, "f", []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("empty_name", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		if _, err := WriteFileInRoot(context.Background(), root, "", []byte("x")); !errors.Is(err, ErrEmptyPath) {
			t.Fatalf("err = %v, want ErrEmptyPath", err)
		}
	})

	t.Run("absolute_name", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		if _, err := WriteFileInRoot(context.Background(), root, "/etc/passwd", []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("null_byte_name", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		if _, err := WriteFileInRoot(context.Background(), root, "a\x00b", []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("err = %v, want ErrUnsafePath", err)
		}
	})
}

func TestWriteInRoot_CancelledContextLeavesNoTarget(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := WriteFileInRoot(ctx, root, "c", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "c")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("target exists after cancelled write: %v", statErr)
	}
	assertNoTempLeak(t, dir)
}

// TestWriteFileInRoot_DirFsyncFailureNotDurable mutates the fsyncRootDir seam,
// so it must not run in parallel.
func TestWriteFileInRoot_DirFsyncFailureNotDurable(t *testing.T) {
	stubFsyncRootDir(t, errors.New("injected dir fsync failure"))
	root, dir := openTestRoot(t)
	res, err := WriteFileInRoot(context.Background(), root, "f", []byte("x"))
	if err != nil {
		t.Fatalf("WriteFileInRoot = %v; a post-rename fsync failure is not a hard error", err)
	}
	if res.Durable {
		t.Errorf("Durable = true, want false when the parent-dir fsync fails")
	}
	// The data still landed despite the non-durable fsync.
	assertContent(t, filepath.Join(dir, "f"), "x")
}

// TestRandomTempName_ReapableShape ties the root-confined temp names to the
// stale-temp sweep: every name createTempInRoot would use must match the shape
// CleanupStaleTemps reaps, or crash orphans from a root write would leak.
func TestRandomTempName_ReapableShape(t *testing.T) {
	t.Parallel()
	for range 1000 {
		name, err := randomTempName()
		if err != nil {
			t.Fatalf("randomTempName: %v", err)
		}
		if !isStaleTempName(name) {
			t.Fatalf("randomTempName produced %q, which CleanupStaleTemps would not reap", name)
		}
	}
}

func TestWriteFileInRoot_PreserveOwnershipSameOwnerSucceeds(t *testing.T) {
	if isWindows() {
		t.Skip("POSIX ownership not meaningful on Windows")
	}
	root, dir := openTestRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "t"), []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := WriteFileInRoot(context.Background(), root, "t", []byte("new"), WithPreserveOwnership())
	if err != nil {
		t.Fatalf("WriteFileInRoot(WithPreserveOwnership) = %v, want nil (same-owner chown is a no-op success)", err)
	}

	assertContent(t, res.Path, "new")
}
