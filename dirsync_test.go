package atomicfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubFsyncDir replaces the package fsyncDir seam with one that returns err,
// restoring the original when the test finishes. Tests using it must not call
// t.Parallel: they mutate package state.
func stubFsyncDir(t *testing.T, err error) {
	t.Helper()
	orig := fsyncDir
	t.Cleanup(func() { fsyncDir = orig })
	fsyncDir = func(string) error { return err }
}

// assertDirSyncError checks that err is a *WriteError carrying PhaseDirSync and
// wrapping sentinel.
func assertDirSyncError(t *testing.T, err, sentinel error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a dir-sync error, got nil")
	}
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("error = %T, want *WriteError", err)
	}
	if we.Phase != PhaseDirSync {
		t.Errorf("WriteError.Phase = %v, want PhaseDirSync", we.Phase)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false, want true (chain not preserved)")
	}
}

// A dir-fsync failure must surface as PhaseDirSync from every durable write
// entry point, AND the new content must already be at the final path: the
// rename succeeded, only the durability barrier failed.
func TestDirSyncFailure_SurfacesPhaseDirSync(t *testing.T) {
	sentinel := errors.New("injected dir fsync failure")

	t.Run("WriteFile", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		err := WriteFile(context.Background(), path, []byte("payload"), WithMode(0o644))
		assertDirSyncError(t, err, sentinel)
		assertContent(t, path, "payload")
	})

	t.Run("WriteReader", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		err := WriteReader(context.Background(), path, strings.NewReader("payload"), 0o644)
		assertDirSyncError(t, err, sentinel)
		assertContent(t, path, "payload")
	})

	t.Run("SaveBytes", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		err := SaveBytes(path, []byte("payload"), 0o644)
		assertDirSyncError(t, err, sentinel)
		assertContent(t, path, "payload")
	})

	t.Run("Commit", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("payload"))
		if err != nil {
			t.Fatalf("Prepare() = %v", err)
		}
		defer cleanup()
		err = Commit(tmpPath, path)
		assertDirSyncError(t, err, sentinel)
		assertContent(t, path, "payload")
	})

	t.Run("PendingFile.CommitFile", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		pf, err := NewPendingFile(path, 0o644)
		if err != nil {
			t.Fatalf("NewPendingFile() = %v", err)
		}
		if _, err := pf.WriteString("payload"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		err = pf.CommitFile()
		assertDirSyncError(t, err, sentinel)
		assertContent(t, path, "payload")
	})
}

// WithNoSync skips the parent-dir fsync entirely, so an injected dir-fsync
// failure must never be reached: the write returns nil.
func TestDirSyncFailure_SkippedWithNoSync(t *testing.T) {
	sentinel := errors.New("dir fsync must not be called under WithNoSync")

	t.Run("WriteFile", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := WriteFile(context.Background(), path, []byte("payload"), WithNoSync()); err != nil {
			t.Fatalf("WriteFile(WithNoSync) = %v, want nil", err)
		}
		assertContent(t, path, "payload")
	})

	t.Run("WriteReader", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := WriteReader(context.Background(), path, strings.NewReader("payload"), 0o644, WithNoSync()); err != nil {
			t.Fatalf("WriteReader(WithNoSync) = %v, want nil", err)
		}
		assertContent(t, path, "payload")
	})

	t.Run("SaveBytes", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := SaveBytes(path, []byte("payload"), 0o644, WithNoSync()); err != nil {
			t.Fatalf("SaveBytes(WithNoSync) = %v, want nil", err)
		}
		assertContent(t, path, "payload")
	})

	t.Run("Commit", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("payload"), WithNoSync())
		if err != nil {
			t.Fatalf("Prepare() = %v", err)
		}
		defer cleanup()
		if err := Commit(tmpPath, path, WithNoSync()); err != nil {
			t.Fatalf("Commit(WithNoSync) = %v, want nil", err)
		}
		assertContent(t, path, "payload")
	})

	t.Run("PendingFile.CommitFile", func(t *testing.T) {
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		pf, err := NewPendingFile(path, 0o644, WithNoSync())
		if err != nil {
			t.Fatalf("NewPendingFile() = %v", err)
		}
		if _, err := pf.WriteString("payload"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		if err := pf.CommitFile(); err != nil {
			t.Fatalf("CommitFile(WithNoSync) = %v, want nil", err)
		}
		assertContent(t, path, "payload")
	})
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v; the rename should have completed before the dir-sync step", path, err)
	}
	if string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

// Commit's dir-fsync is governed by the options passed to Commit, NOT by the
// options passed to Prepare. The godoc documents this as a footgun: passing
// WithNoSync to Commit (but not Prepare) silently drops the durability barrier.
// These cases pin that contract using the injected fsyncDir failure, which the
// real-fsyncDir NoSync tests cannot observe (a healthy tmpfs fsync never fails).
func TestDirSyncFailure_CommitOptionMismatch(t *testing.T) {
	t.Run("CommitNoSync_skips_even_when_Prepare_synced", func(t *testing.T) {
		stubFsyncDir(t, errors.New("dir fsync must not run when Commit has NoSync"))
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		// Prepare WITHOUT NoSync...
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("payload"))
		if err != nil {
			t.Fatalf("Prepare() = %v", err)
		}
		defer cleanup()
		// ...but Commit WITH NoSync drops the dir-fsync entirely.
		if err := Commit(tmpPath, path, WithNoSync()); err != nil {
			t.Fatalf("Commit(WithNoSync) = %v, want nil (Commit's options govern; dir-fsync skipped)", err)
		}
		assertContent(t, path, "payload")
	})

	t.Run("CommitSync_runs_even_when_Prepare_had_NoSync", func(t *testing.T) {
		sentinel := errors.New("injected dir fsync failure")
		stubFsyncDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		// Prepare WITH NoSync...
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("payload"), WithNoSync())
		if err != nil {
			t.Fatalf("Prepare(WithNoSync) = %v", err)
		}
		defer cleanup()
		// ...but Commit WITHOUT NoSync still runs (and fails) the dir-fsync.
		err = Commit(tmpPath, path)
		assertDirSyncError(t, err, sentinel)
		assertContent(t, path, "payload")
	})
}

// The production fsyncDir is only ever exercised indirectly (its success path)
// by ordinary writes, and is replaced by a stub in every other dir-sync test.
// This pins the real implementation directly: a valid directory syncs cleanly,
// and a missing directory surfaces the open error (which commitDirSync wraps as
// PhaseDirSync). Serial (no t.Parallel) so it never races the stub swappers.
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
		// commitDirSync must wrap that bare error as PhaseDirSync.
		orig := fsyncDir
		t.Cleanup(func() { fsyncDir = orig })
		fsyncDir = func(string) error { return err }
		var we *WriteError
		if got := commitDirSync(missing, false); !errors.As(got, &we) || we.Phase != PhaseDirSync {
			t.Errorf("commitDirSync wrapped = %v, want *WriteError{PhaseDirSync}", got)
		}
	})
}
