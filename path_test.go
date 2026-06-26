package atomicfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAbsClean(t *testing.T) {
	t.Parallel()

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Parallel()
		if _, err := validateAbsClean("relative/path"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("validateAbsClean(relative) = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("collapses_traversal_to_clean_absolute_path", func(t *testing.T) {
		t.Parallel()
		got, err := validateAbsClean("/foo/../etc/passwd")
		if err != nil {
			t.Fatalf("validateAbsClean(%q) = error %v, want nil (Clean removes \"..\")",
				"/foo/../etc/passwd", err)
		}
		if got != "/etc/passwd" {
			t.Errorf("validateAbsClean(%q) = %q, want %q", "/foo/../etc/passwd", got, "/etc/passwd")
		}
	})

	t.Run("accepts_absolute_clean_path", func(t *testing.T) {
		t.Parallel()
		got, err := validateAbsClean("/tmp/test.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/tmp/test.txt" {
			t.Errorf("got %q, want %q", got, "/tmp/test.txt")
		}
	})

	t.Run("rejects_empty", func(t *testing.T) {
		t.Parallel()
		if _, err := validateAbsClean(""); !errors.Is(err, ErrEmptyPath) {
			t.Fatalf("validateAbsClean(empty) = %v, want ErrEmptyPath", err)
		}
	})

	t.Run("rejects_null_byte", func(t *testing.T) {
		t.Parallel()
		_, err := validateAbsClean("/tmp/foo\x00bar")
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("validateAbsClean(null) = %v, want ErrUnsafePath", err)
		}
		if !strings.Contains(err.Error(), "null byte") {
			t.Errorf("error = %q, want mention of null byte", err.Error())
		}
	})

	t.Run("rejects_null_byte_suffix", func(t *testing.T) {
		t.Parallel()
		if _, err := validateAbsClean("/tmp/test.txt\x00ignored"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("validateAbsClean(null suffix) = %v, want ErrUnsafePath", err)
		}
	})
}

func TestSymlinkTarget(t *testing.T) {
	t.Parallel()

	t.Run("refuses_symlink_target_by_default", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		_, err := WriteFile(context.Background(), link, []byte("new"))
		if !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("WriteFile(symlink) = %v, want ErrSymlinkTarget", err)
		}
		got, _ := os.ReadFile(real)
		if string(got) != "original" {
			t.Errorf("original file modified: %q", got)
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("allows_symlink_target_with_option", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := WriteFile(context.Background(), link, []byte("new"), WithAllowSymlinkTarget()); err != nil {
			t.Fatalf("WriteFile with AllowSymlinkTarget: %v", err)
		}
		got, _ := os.ReadFile(link)
		if string(got) != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("no_error_for_nonexistent_target", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "new.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("WriteReader_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := WriteReader(context.Background(), link, strings.NewReader("new")); !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("WriteReader(symlink) = %v, want ErrSymlinkTarget", err)
		}
	})

	t.Run("PendingFile_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := NewPendingFile(context.Background(), link); !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("NewPendingFile(symlink) = %v, want ErrSymlinkTarget", err)
		}
	})
}

func TestWriteFile_SymlinkInParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	realDir := filepath.Join(dir, "realdir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	linkDir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// The target file itself is not a symlink, only an ancestor directory is,
	// so the write is permitted and lands in the real directory.
	path := filepath.Join(linkDir, "file.txt")
	if _, err := WriteFile(context.Background(), path, []byte("through symlink parent")); err != nil {
		t.Fatalf("WriteFile through symlink parent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(realDir, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile from realdir: %v", err)
	}
	if string(got) != "through symlink parent" {
		t.Errorf("got %q", got)
	}
}

func TestNullByte_AllEntryPoints(t *testing.T) {
	t.Parallel()
	nullPath := "/tmp/test\x00evil"

	t.Run("WriteFile", func(t *testing.T) {
		if _, err := WriteFile(context.Background(), nullPath, []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
	t.Run("WriteReader", func(t *testing.T) {
		if _, err := WriteReader(context.Background(), nullPath, strings.NewReader("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
	t.Run("NewPendingFile", func(t *testing.T) {
		if _, err := NewPendingFile(context.Background(), nullPath); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
	t.Run("ReadBounded", func(t *testing.T) {
		if _, err := ReadBounded(context.Background(), nullPath, 1024); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
}

func TestEmptyPath_AllEntryPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := WriteFile(ctx, "", []byte("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteFile empty: %v", err)
	}
	if _, err := WriteReader(ctx, "", strings.NewReader("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteReader empty: %v", err)
	}
	if _, err := NewPendingFile(ctx, ""); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("NewPendingFile empty: %v", err)
	}
	if _, err := ReadBounded(ctx, "", 1024); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("ReadBounded empty: %v", err)
	}
}
