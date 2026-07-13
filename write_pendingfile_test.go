package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestPendingFile(t *testing.T) {
	t.Parallel()

	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "pending.txt")
		pf, err := NewPendingFile(context.Background(), path)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("pending data")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		res, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if res.Path != path {
			t.Errorf("Result.Path = %q, want %q", res.Path, path)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true")
		}
		got, _ := os.ReadFile(path)
		if string(got) != "pending data" {
			t.Errorf("got %q, want %q", got, "pending data")
		}
	})

	t.Run("cleanup_removes_temp", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "cleanup.txt")
		pf, err := NewPendingFile(context.Background(), path)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		if _, err := pf.Write([]byte("will be cleaned")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		tmpName := pf.Name()
		if err := pf.Cleanup(); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		if _, err := os.Stat(tmpName); !os.IsNotExist(err) {
			t.Error("temp file not removed after Cleanup")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("final file should not exist after Cleanup")
		}
	})

	t.Run("commit_is_idempotent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "idem.txt")
		pf, err := NewPendingFile(context.Background(), path)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		if _, err := pf.Write([]byte("once")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		first, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("first Commit: %v", err)
		}
		second, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("second Commit: %v", err)
		}
		if first != second {
			t.Errorf("Commit not idempotent: first %+v, second %+v", first, second)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "once" {
			t.Errorf("got %q, want %q", got, "once")
		}
	})

	t.Run("mode_applied", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file mode not meaningful on Windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "mode.txt")
		pf, err := NewPendingFile(context.Background(), path, WithMode(0o600))
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("secret")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("invalid_path", func(t *testing.T) {
		t.Parallel()
		_, err := NewPendingFile(context.Background(), "relative/path")
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("NewPendingFile(relative) = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("mkdir_mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "pending.txt")
		pf, err := NewPendingFile(context.Background(), path, WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("nested")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "nested" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("nosync_not_durable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync.txt")
		pf, err := NewPendingFile(context.Background(), path, WithNoSync())
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("fast")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		res, err := pf.Commit(context.Background())
		if err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false under WithNoSync")
		}
	})
}

func TestPendingFile_ConcurrentSamePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-race.txt")
	var wg sync.WaitGroup
	const N = 20
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pf, err := NewPendingFile(context.Background(), path, WithNoSync())
			if err != nil {
				return
			}
			defer func() { _ = pf.Cleanup() }()
			if _, err := pf.Write([]byte(strings.Repeat("Z", idx+1))); err != nil {
				return
			}
			_, _ = pf.Commit(context.Background())
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected len=%d", len(got))
	}
	assertNoTempLeak(t, dir)
}

func TestNewPendingFile_ContextCancel_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "npf.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pf, err := NewPendingFile(ctx, path)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewPendingFile(cancelled) error = %v, want context.Canceled", err)
	}
	if pf != nil {
		t.Error("NewPendingFile should return nil PendingFile on cancellation")
	}
	assertNoTempLeak(t, dir)
}

func TestPendingFile_Commit_ContextCancel_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cf.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pf.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit(cancelled) error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final path should not exist after cancelled Commit")
	}
	assertNoTempLeak(t, dir)
}

// NewPendingFile runs on a live context, so cancelAt=1 trips the ctx guard at
// the top of finalizeTempFile during Commit.
func TestPendingFile_CancelMidBarrier_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-midbarrier.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile = %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte("data")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 1}
	if _, err := pf.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Commit(cancel-mid-barrier) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after mid-barrier cancellation")
	}
	assertNoTempLeak(t, dir)
}

func TestPendingFile_AbandonedLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "abandoned.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if _, err := pf.Write([]byte("data that will be abandoned")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tmpName := pf.Name()

	// An abandoned PendingFile keeps its temp until Cleanup/Commit.
	if _, err := os.Stat(tmpName); err != nil {
		t.Fatalf("temp file should still exist when abandoned: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(tmpName); !os.IsNotExist(err) {
		t.Error("temp not removed after Cleanup")
	}
}

func TestPendingFile_DoubleCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dblclean.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("second Cleanup should be no-op: %v", err)
	}
}

func TestPendingFile_CleanupThenCommit_ReturnsAborted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cleancommit.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// Commit after Cleanup must report ErrAborted, not a false zero-Result
	// success: the temp was already removed, so nothing reached the final path.
	res, commitErr := pf.Commit(context.Background())
	if !errors.Is(commitErr, ErrAborted) {
		t.Fatalf("Commit after Cleanup = %v, want ErrAborted", commitErr)
	}
	if res != (Result{}) {
		t.Errorf("Commit after Cleanup result = %+v, want zero Result", res)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("file should not exist after Cleanup+Commit")
	}
}

// After a successful Commit, Cleanup must be a no-op that does NOT remove the
// committed file, and a later Commit still replays the original result.
func TestPendingFile_CommitThenCleanup_KeepsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "commit-then-clean.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if _, err := pf.Write([]byte("kept")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	first, err := pf.Commit(context.Background())
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("Cleanup after Commit should be a no-op: %v", err)
	}
	assertContent(t, path, "kept") // Cleanup must not remove the committed file
	again, err := pf.Commit(context.Background())
	if err != nil {
		t.Fatalf("Commit after a no-op Cleanup: %v", err)
	}
	if again != first {
		t.Errorf("Commit not idempotent across an intervening Cleanup: %+v vs %+v", again, first)
	}
}

// A failed Commit lands in the committed state with a cached *WriteError; a
// second Commit must replay that identical error value (not nil, not ErrAborted,
// not a fresh error) and the same zero Result, without re-running the barrier or
// leaking a temp. Closing the embedded fd makes the barrier's first step (Chmod)
// fail, so this also pins the PhaseTempChmod tag and the on-barrier-failure temp
// cleanup.
func TestPendingFile_FailedCommit_ReplaysSameError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "failed-commit-replay.txt")

	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if _, err := pf.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pf.Close(); err != nil {
		t.Fatalf("File.Close: %v", err)
	}

	firstRes, firstErr := pf.Commit(context.Background())
	var we *WriteError
	if !errors.As(firstErr, &we) || we.Phase != PhaseTempChmod {
		t.Fatalf("first Commit = %v, want *WriteError{PhaseTempChmod}", firstErr)
	}
	if firstRes != (Result{}) {
		t.Fatalf("first Commit result = %+v, want zero Result", firstRes)
	}

	secondRes, secondErr := pf.Commit(context.Background())
	if secondErr != firstErr {
		t.Errorf("second Commit err = %v, want the cached first err %v (identical value)", secondErr, firstErr)
	}
	if errors.Is(secondErr, ErrAborted) {
		t.Errorf("second Commit err = %v, want the cached WriteError, not ErrAborted", secondErr)
	}
	if secondRes != firstRes {
		t.Errorf("second Commit result = %+v, want cached %+v", secondRes, firstRes)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("final file exists after replayed failed Commit, want absent")
	}
	assertNoTempLeak(t, dir)
}

// The PendingFile rename-failure branch: committing onto a NON-EMPTY directory
// forces os.Rename to fail with ENOTEMPTY on every platform without test seams.
// The first Commit must tag PhaseRename and clean the temp; a second Commit must
// replay the same cached error.
func TestPendingFile_Commit_RenameFailure_TaggedAndReplays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	pf, err := NewPendingFile(context.Background(), target)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if _, err := pf.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, firstErr := pf.Commit(context.Background())
	var we *WriteError
	if !errors.As(firstErr, &we) || we.Phase != PhaseRename {
		t.Fatalf("Commit(onto non-empty dir) = %v, want *WriteError{PhaseRename}", firstErr)
	}

	_, secondErr := pf.Commit(context.Background())
	if secondErr != firstErr {
		t.Errorf("second Commit = %v, want the cached rename error %v (identical value)", secondErr, firstErr)
	}
	assertNoTempLeak(t, dir)
}

// Cleanup must be best-effort on a close failure: a pre-closed fd makes the
// internal p.Close() in Cleanup return os.ErrClosed (logged at Debug), which
// Cleanup MUST swallow and still remove the temp and return nil. A return-clErr
// regression would leak the temp.
func TestPendingFile_Cleanup_CloseFailure_StillRemovesTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cleanup-closed-fd.txt")

	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if _, err := pf.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tmpName := pf.Name()

	// First Close succeeds and leaves state pendingOpen + the temp on disk;
	// the internal p.Close() call is then a second close -> os.ErrClosed.
	if err := pf.Close(); err != nil {
		t.Fatalf("File.Close: %v", err)
	}

	if err := pf.Cleanup(); err != nil {
		t.Errorf("Cleanup after closed fd = %v, want nil (close failure is logged, not returned)", err)
	}
	if _, statErr := os.Stat(tmpName); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("temp %q still present after Cleanup, want removed (no leak)", tmpName)
	}
	assertNoTempLeak(t, dir)
}

// TestPendingFile_Cleanup_RemoveFailureSurfacesError pins that
// (*PendingFile).Cleanup surfaces a temp-removal failure (other than
// ErrNotExist) as a non-nil error. Replacing the temp with a NON-EMPTY
// directory triggers ENOTEMPTY, which root cannot bypass, so the assertion holds
// even when the suite runs as root.
func TestPendingFile_Cleanup_RemoveFailureSurfacesError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")

	pf, err := NewPendingFile(context.Background(), target)
	if err != nil {
		t.Fatalf("NewPendingFile(%q) = %v, want nil", target, err)
	}
	tmpName := pf.Name()

	// Swap the temp file for a non-empty directory at the same path so
	// Cleanup's os.Remove(tmpName) fails with ENOTEMPTY.
	replaceWithNonEmptyDir(t, tmpName)

	gotErr := pf.Cleanup()
	if gotErr == nil {
		t.Fatalf("Cleanup() (os.Remove hits non-empty dir) = nil, want non-nil error")
	}
	if errors.Is(gotErr, fs.ErrNotExist) {
		t.Fatalf("Cleanup() = %v, want a non-ErrNotExist error (ENOTEMPTY expected)", gotErr)
	}
}

// TestPendingFile_Cleanup_SuccessReturnsNil pins the opposite branch: when
// os.Remove succeeds, Cleanup returns nil and the temp is gone.
func TestPendingFile_Cleanup_SuccessReturnsNil(t *testing.T) {
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

// TestPendingFile_Cleanup_RetriesAfterRemoveFailure pins the pendingCleanupFailed
// retry semantics: when the first Cleanup's os.Remove fails (ENOTEMPTY), Cleanup
// returns the error and does NOT falsely mark the write cleaned, so Commit still
// aborts and a later Cleanup retries the removal once the obstruction clears. A
// regression to the old "mark cleaned before remove" behavior would make the
// second Cleanup a silent no-op and leak the temp. Replacing the temp with a
// NON-EMPTY directory triggers ENOTEMPTY, which root cannot bypass, so this holds
// even when the suite runs as root.
func TestPendingFile_Cleanup_RetriesAfterRemoveFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "retry.txt")

	pf, err := NewPendingFile(context.Background(), target)
	if err != nil {
		t.Fatalf("NewPendingFile(%q) = %v, want nil", target, err)
	}
	tmpName := pf.Name()

	// Swap the temp for a non-empty directory so the first os.Remove fails.
	replaceWithNonEmptyDir(t, tmpName)

	firstErr := pf.Cleanup()
	if firstErr == nil {
		t.Fatalf("first Cleanup (os.Remove hits non-empty dir) = nil, want non-nil error")
	}
	if errors.Is(firstErr, fs.ErrNotExist) {
		t.Fatalf("first Cleanup = %v, want a non-ErrNotExist error (ENOTEMPTY expected)", firstErr)
	}

	// A failed Cleanup must NOT falsely mark the write cleaned: Commit still
	// aborts because nothing reached the final path.
	if _, commitErr := pf.Commit(context.Background()); !errors.Is(commitErr, ErrAborted) {
		t.Fatalf("Commit after a failed Cleanup = %v, want ErrAborted", commitErr)
	}

	// Clearing the obstruction lets a second Cleanup retry the removal and
	// succeed. The old (mark-cleaned-before-remove) behavior would make this a
	// silent no-op and leak the directory left at tmpName.
	if err := os.Remove(filepath.Join(tmpName, "child")); err != nil {
		t.Fatalf("clear obstruction: %v", err)
	}
	if secondErr := pf.Cleanup(); secondErr != nil {
		t.Fatalf("second Cleanup (retry) = %v, want nil", secondErr)
	}
	if _, statErr := os.Stat(tmpName); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("temp %q still present after retry Cleanup, want removed (no leak)", tmpName)
	}
}

// TestPendingFile_Cleanup_RetryKeepsRetryableStateAfterSecondRemoveFailure pins
// the retry-failure branch of the pendingCleanupFailed state: a first Cleanup
// fails (ENOTEMPTY), a SECOND Cleanup while the temp is still obstructed also
// fails and MUST keep the write in a retryable (not falsely-cleaned) state, so
// Commit still aborts; only after the obstruction clears does a third Cleanup
// succeed and remove the temp. A regression that marks the write cleaned on the
// second failed removal would make the third Cleanup a silent no-op and leak the
// temp directory. Replacing the temp with a NON-EMPTY directory triggers
// ENOTEMPTY, which root cannot bypass, so this holds even when run as root.
func TestPendingFile_Cleanup_RetryKeepsRetryableStateAfterSecondRemoveFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "retry-still-blocked.txt")

	pf, err := NewPendingFile(context.Background(), target)
	if err != nil {
		t.Fatalf("NewPendingFile(%q) = %v, want nil", target, err)
	}
	tmpName := pf.Name()

	replaceWithNonEmptyDir(t, tmpName)

	firstErr := pf.Cleanup()
	if firstErr == nil {
		t.Fatalf("first Cleanup (os.Remove hits non-empty dir) = nil, want non-nil error")
	}
	if errors.Is(firstErr, fs.ErrNotExist) {
		t.Fatalf("first Cleanup = %v, want a non-ErrNotExist error (ENOTEMPTY expected)", firstErr)
	}

	secondErr := pf.Cleanup()
	if secondErr == nil {
		t.Fatalf("second Cleanup while temp is still a non-empty dir = nil, want non-nil error")
	}
	if errors.Is(secondErr, fs.ErrNotExist) {
		t.Fatalf("second Cleanup = %v, want a non-ErrNotExist error (ENOTEMPTY expected)", secondErr)
	}
	if _, commitErr := pf.Commit(context.Background()); !errors.Is(commitErr, ErrAborted) {
		t.Fatalf("Commit after repeated failed Cleanup = %v, want ErrAborted", commitErr)
	}

	if err := os.Remove(filepath.Join(tmpName, "child")); err != nil {
		t.Fatalf("clear obstruction: %v", err)
	}
	if thirdErr := pf.Cleanup(); thirdErr != nil {
		t.Fatalf("third Cleanup (retry after obstruction clears) = %v, want nil", thirdErr)
	}
	if _, statErr := os.Stat(tmpName); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("temp %q still present after third Cleanup, want removed (no leak)", tmpName)
	}
}
