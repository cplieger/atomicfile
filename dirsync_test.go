package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A parent-directory fsync failure is NOT a hard error in the new API: the
// rename already succeeded, so the data is at the final path. The write
// reports Result{Durable:false} with a nil error, and logs a Warn. This must
// hold for every durable write entry point.
func TestDirSyncFailure_ReportsNotDurableNotError(t *testing.T) {
	sentinel := errors.New("injected dir fsync failure")

	t.Run("WriteFile", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		res, err := WriteFile(context.Background(), path, []byte("payload"), WithMode(0o644))
		if err != nil {
			t.Fatalf("WriteFile(dir-fsync fail) = %v, want nil error", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false after dir-fsync failure")
		}
		assertContent(t, path, "payload")
	})

	t.Run("WriteReader", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		res, err := WriteReader(context.Background(), path, strings.NewReader("payload"))
		if err != nil {
			t.Fatalf("WriteReader(dir-fsync fail) = %v, want nil error", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false after dir-fsync failure")
		}
		assertContent(t, path, "payload")
	})

	t.Run("PendingFile.Commit", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		pf, err := NewPendingFile(context.Background(), path)
		if err != nil {
			t.Fatalf("NewPendingFile() = %v", err)
		}
		if _, err := pf.WriteString("payload"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		res, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("Commit(dir-fsync fail) = %v, want nil error", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false after dir-fsync failure")
		}
		assertContent(t, path, "payload")
	})
}

// A dir-fsync failure must emit a Warn-level log so operators can detect the
// degraded-durability condition.
func TestDirSyncFailure_LogsWarn(t *testing.T) {
	stubFsyncDir(t, errors.New("injected dir fsync failure"))
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	res, err := WriteFile(context.Background(), path, []byte("payload"), WithLogger(logger))
	if err != nil {
		t.Fatalf("WriteFile = %v, want nil", err)
	}
	if res.Durable {
		t.Errorf("Result.Durable = true, want false")
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("expected a WARN log, got: %q", buf.String())
	}
}

// WithNoSync skips the parent-dir fsync entirely, so an injected dir-fsync
// failure must never be reached: the write succeeds and reports not-durable.
func TestDirSyncFailure_SkippedWithNoSync(t *testing.T) {
	sentinel := errors.New("dir fsync must not be called under WithNoSync")

	t.Run("WriteFile", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		res, err := WriteFile(context.Background(), path, []byte("payload"), WithNoSync())
		if err != nil {
			t.Fatalf("WriteFile(WithNoSync) = %v, want nil", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false under WithNoSync")
		}
		assertContent(t, path, "payload")
	})

	t.Run("WriteReader", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		res, err := WriteReader(context.Background(), path, strings.NewReader("payload"), WithNoSync())
		if err != nil {
			t.Fatalf("WriteReader(WithNoSync) = %v, want nil", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false under WithNoSync")
		}
		assertContent(t, path, "payload")
	})

	t.Run("PendingFile.Commit", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		pf, err := NewPendingFile(context.Background(), path, WithNoSync())
		if err != nil {
			t.Fatalf("NewPendingFile() = %v", err)
		}
		if _, err := pf.WriteString("payload"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		res, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("Commit(WithNoSync) = %v, want nil", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false under WithNoSync")
		}
		assertContent(t, path, "payload")
	})
}

// The production fsyncDir is replaced by a stub in every other dir-sync test.
// This pins the real implementation directly: a valid directory syncs cleanly,
// and a missing directory surfaces the open error. Serial (no t.Parallel) so it
// never races the stub swappers.
func TestFsyncDir_Production(t *testing.T) {
	t.Run("valid_dir_returns_nil", func(t *testing.T) {
		if err := fsyncDir(t.TempDir()); err != nil {
			t.Errorf("fsyncDir(validDir) = %v, want nil", err)
		}
	})

	t.Run("missing_dir_returns_open_error", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "does-not-exist")
		err := fsyncDir(missing)
		if err == nil {
			t.Fatal("fsyncDir(missingDir) = nil, want open error")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("fsyncDir(missingDir) = %v, want errors.Is os.ErrNotExist", err)
		}
	})
}

// Commit is idempotent even when the first call hit a dir-fsync failure: the
// recorded (not-durable, nil-error) Result is returned again, and the content
// remains at the final path.
func TestPendingFile_Commit_IdempotentAfterDirSyncFailure(t *testing.T) {
	stubFsyncDir(t, errors.New("injected dir fsync failure"))
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile() = %v", err)
	}
	if _, err := pf.WriteString("payload"); err != nil {
		t.Fatalf("WriteString() = %v", err)
	}
	first, firstErr := pf.Commit(context.Background())
	if firstErr != nil {
		t.Fatalf("first Commit() = %v, want nil", firstErr)
	}
	if first.Durable {
		t.Errorf("first Result.Durable = true, want false")
	}
	second, secondErr := pf.Commit(context.Background())
	if secondErr != nil {
		t.Fatalf("second Commit() = %v, want nil", secondErr)
	}
	if second != first {
		t.Fatalf("second Commit() = %+v, want identical recorded result %+v", second, first)
	}
	assertContent(t, path, "payload")
}
