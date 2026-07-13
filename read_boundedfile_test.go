package atomicfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadBoundedFile(t *testing.T) {
	t.Parallel()

	t.Run("reads_open_file_within_limit", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "f.txt")
		if err := os.WriteFile(path, []byte("hello handle"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		got, err := ReadBoundedFile(context.Background(), f, 1024)
		if err != nil {
			t.Fatalf("ReadBoundedFile: %v", err)
		}
		if string(got) != "hello handle" {
			t.Errorf("got %q, want %q", got, "hello handle")
		}
	})

	t.Run("rejects_file_exceeding_limit", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "big.txt")
		if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		if _, err := ReadBoundedFile(context.Background(), f, 50); !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("ReadBoundedFile(over) = %v, want ErrFileTooLarge", err)
		}
	})

	t.Run("honors_cancelled_context", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "c.txt")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ReadBoundedFile(ctx, f, 1024); !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadBoundedFile(cancelled) = %v, want context.Canceled", err)
		}
	})

	t.Run("rejects_nil_file_without_panic", func(t *testing.T) {
		t.Parallel()
		got, err := ReadBoundedFile(context.Background(), nil, 1024)
		if err == nil {
			t.Fatalf("ReadBoundedFile(nil) = %v, want non-nil error", got)
		}
		if got != nil {
			t.Errorf("ReadBoundedFile(nil) data = %q, want nil", got)
		}
	})

	t.Run("does_not_close_caller_handle", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "twice.txt")
		if err := os.WriteFile(path, []byte("reuse"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		if _, err := ReadBoundedFile(context.Background(), f, 1024); err != nil {
			t.Fatalf("first read: %v", err)
		}
		// The caller owns f; ReadBoundedFile must not close it.
		if _, err := f.Stat(); err != nil {
			t.Fatalf("handle closed by ReadBoundedFile: %v", err)
		}
	})

	t.Run("reads_through_os_root_handle", func(t *testing.T) {
		t.Parallel()
		// The intended seam: open the file through an *os.Root (which confines
		// the path) and read it with ReadBoundedFile for identical bounds.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "in.pem"), []byte("rooted"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		defer root.Close()
		f, err := root.Open("in.pem")
		if err != nil {
			t.Fatalf("root.Open: %v", err)
		}
		defer f.Close()
		got, err := ReadBoundedFile(context.Background(), f, 1024)
		if err != nil {
			t.Fatalf("ReadBoundedFile via root: %v", err)
		}
		if string(got) != "rooted" {
			t.Errorf("got %q, want %q", got, "rooted")
		}
	})
}
