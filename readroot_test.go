package atomicfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v3"
)

func TestReadBoundedInRoot(t *testing.T) {
	t.Parallel()

	t.Run("reads a file inside the root", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "in.pem"), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		got, err := atomicfile.ReadBoundedInRoot(t.Context(), root, "in.pem", 1024)
		if err != nil {
			t.Fatalf("ReadBoundedInRoot = %v, want nil", err)
		}
		if string(got) != "payload" {
			t.Errorf("read %q, want %q", got, "payload")
		}
	})

	t.Run("enforces the size bound", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "big.pem"), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		_, err = atomicfile.ReadBoundedInRoot(t.Context(), root, "big.pem", 1024)
		if !errors.Is(err, atomicfile.ErrFileTooLarge) {
			t.Errorf("ReadBoundedInRoot(oversized) = %v, want ErrFileTooLarge", err)
		}
	})

	t.Run("refuses a symlink that escapes the root", func(t *testing.T) {
		t.Parallel()
		dir, outside := t.TempDir(), t.TempDir()
		secret := filepath.Join(outside, "secret.pem")
		if err := os.WriteFile(secret, []byte("do not leak"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(dir, "leak.pem")); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		got, err := atomicfile.ReadBoundedInRoot(t.Context(), root, "leak.pem", 1024)
		if err == nil {
			t.Fatalf("ReadBoundedInRoot(escaping symlink) returned %q, want a confinement refusal", got)
		}
	})

	t.Run("refuses a parent-directory traversal", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "secret.pem"), []byte("nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(parent, "inner")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		got, err := atomicfile.ReadBoundedInRoot(t.Context(), root, "../secret.pem", 1024)
		if err == nil {
			t.Fatalf("ReadBoundedInRoot(traversal) returned %q, want a refusal", got)
		}
	})

	t.Run("rejects a non-regular file without blocking", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fifo := filepath.Join(dir, "pipe.pem")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		// O_NONBLOCK: opening a FIFO with no writer blocks forever otherwise.
		done := make(chan error, 1)
		go func() {
			_, readErr := atomicfile.ReadBoundedInRoot(t.Context(), root, "pipe.pem", 1024)
			done <- readErr
		}()
		select {
		case readErr := <-done:
			if readErr == nil {
				t.Error("ReadBoundedInRoot(fifo) = nil error, want a non-regular-file rejection")
			} else if !errors.Is(readErr, atomicfile.ErrNotRegular) {
				t.Errorf("ReadBoundedInRoot(fifo) = %v, want ErrNotRegular", readErr)
			} else if !strings.Contains(readErr.Error(), "type") {
				t.Errorf("ReadBoundedInRoot(fifo) = %v, want it to name the actual file mode", readErr)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("ReadBoundedInRoot blocked on a FIFO; the open must be non-blocking")
		}
	})

	t.Run("rejects an empty name and a nil root", func(t *testing.T) {
		t.Parallel()
		root, err := os.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		if _, err := atomicfile.ReadBoundedInRoot(t.Context(), root, "", 1024); !errors.Is(err, atomicfile.ErrEmptyPath) {
			t.Errorf("empty name = %v, want ErrEmptyPath", err)
		}
		if _, err := atomicfile.ReadBoundedInRoot(t.Context(), nil, "x", 1024); err == nil {
			t.Error("nil root = nil error, want a rejection")
		}
	})
}
