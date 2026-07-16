package atomicfile

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPreserveMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}

	t.Run("preserves_existing_mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "preserve.txt")
		if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := WriteFile(context.Background(), path, []byte("new"), WithPreserveMode()); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("mode = %o, want 0755 (preserved)", fi.Mode().Perm())
		}
	})

	t.Run("falls_back_to_explicit_if_no_target", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "new.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data"), WithMode(0o600), WithPreserveMode()); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 0600 (fallback)", fi.Mode().Perm())
		}
	})

	t.Run("preserve_mode_with_WriteReader", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "reader.txt")
		if err := os.WriteFile(path, []byte("old"), 0o750); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := WriteReader(context.Background(), path, strings.NewReader("new"), WithPreserveMode()); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o750 {
			t.Errorf("mode = %o, want 0750 (preserved)", fi.Mode().Perm())
		}
	})

	t.Run("preserve_mode_with_PendingFile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "pf.txt")
		if err := os.WriteFile(path, []byte("old"), 0o750); err != nil {
			t.Fatalf("seed: %v", err)
		}
		pf, err := NewPendingFile(context.Background(), path, WithMode(0o644), WithPreserveMode())
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("new")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o750 {
			t.Errorf("mode = %o, want 0750 (preserved)", fi.Mode().Perm())
		}
	})
}

func TestPreserveMode_OverridesExplicitMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pm.txt")
	if err := os.WriteFile(path, []byte("old"), 0o751); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// WithPreserveMode takes priority over an explicit WithMode for an existing target.
	if _, err := WriteFile(context.Background(), path, []byte("new"),
		WithMode(0o600), WithPreserveMode()); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o751 {
		t.Fatalf("PreserveMode should override WithMode; got %o, want 0751", fi.Mode().Perm())
	}
}

func TestPreserveOwnership(t *testing.T) {
	t.Parallel()

	t.Run("noop_if_target_missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "noexist.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data"), WithPreserveOwnership()); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "data" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("same_user_succeeds", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("ownership not meaningful on Windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "owned.txt")
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Chowning to the current owner is a no-op that succeeds for any user.
		if _, err := WriteFile(context.Background(), path, []byte("new"), WithPreserveOwnership()); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "new" {
			t.Errorf("got %q", got)
		}
	})
}

// TestWriteFile_PreserveOwnership_ChownFailure_NonFatal pins the best-effort
// contract: when the temp-file chown fails, the write still lands (data at the
// final path, Result.Durable true) and the failure surfaces as a single Warn.
// Uses the rootChown seam because a real chown failure is unforceable from a
// same-owner test. Not parallel: it mutates the package rootChown var.
func TestWriteFile_PreserveOwnership_ChownFailure_NonFatal(t *testing.T) {
	if isWindows() {
		t.Skip("preserve-ownership uses *syscall.Stat_t (Unix-only)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	// Pre-create the target so applyOwnership stats it and reaches the chown.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	sentinel := errors.New("injected chown failure")
	stubRootChown(t, sentinel)

	h := &captureHandler{}
	res, err := WriteFile(context.Background(), path, []byte("new"),
		WithPreserveOwnership(), WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("WriteFile = %v, want nil (chown failure must be non-fatal)", err)
	}
	if !res.Durable {
		t.Error("Result.Durable = false, want true (write should still be durable)")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile = %v", readErr)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q (write must land despite chown failure)", got, "new")
	}
	var warns int
	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("warn count = %d, want 1 (chown failure should log exactly one Warn)", warns)
	}
}
