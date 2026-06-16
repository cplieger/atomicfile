package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// countLogByMessage returns how many captured records match both level and
// message exactly. Used to pin which best-effort log lines fire (and which do
// not) for a given outcome.
func countLogByMessage(records []slog.Record, level slog.Level, message string) int {
	n := 0
	for _, r := range records {
		if r.Level == level && r.Message == message {
			n++
		}
	}
	return n
}

const (
	msgRemoveTempFailed = "atomicfile: temp file cleanup failed"
	msgStaleRemoved     = "atomicfile.CleanupStaleTemps: removed stale temps"
	msgStaleRemoveFail  = "atomicfile.CleanupStaleTemps: some stale temps could not be removed"
)

// TestRemoveTemp_SuccessfulRemoval_DoesNotLogDebug pins removeTemp's contract
// that a successful cleanup is silent. It kills the CONDITIONALS_NEGATION
// mutant on the rmErr-guard (atomicfile.go:313): flipping `rmErr != nil` to
// `rmErr == nil` makes a successful removal (rmErr == nil) take the log branch.
func TestRemoveTemp_SuccessfulRemoval_DoesNotLogDebug(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "removable.tmp")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}
	h := &captureHandler{}

	removeTemp(path, slog.New(h))

	got := countLogByMessage(h.records, slog.LevelDebug, msgRemoveTempFailed)
	if got != 0 {
		t.Errorf("removeTemp(existing removable file) logged %q %d times, want 0", msgRemoveTempFailed, got)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("removeTemp(%q) left file behind: Stat err = %v, want ErrNotExist", path, err)
	}
}

// TestRemoveTemp_FailedRemoval_LogsDebug pins removeTemp's contract that a
// removal failing for a reason other than ErrNotExist is logged once at Debug.
// os.Remove of a non-empty directory fails with a non-ErrNotExist error on
// every platform, so no permission tricks are needed. It kills the
// CONDITIONALS_NEGATION mutant on atomicfile.go:313: flipping `rmErr != nil` to
// `rmErr == nil` suppresses the log on a genuine failure (rmErr != nil).
func TestRemoveTemp_FailedRemoval_LogsDebug(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	nonEmpty := filepath.Join(dir, "busy")
	if err := os.Mkdir(nonEmpty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	h := &captureHandler{}

	removeTemp(nonEmpty, slog.New(h))

	got := countLogByMessage(h.records, slog.LevelDebug, msgRemoveTempFailed)
	if got != 1 {
		t.Errorf("removeTemp(non-empty dir) logged %q %d times, want 1", msgRemoveTempFailed, got)
	}
}

// TestPendingFile_Cleanup_RemoveFailure_ReturnsError pins that Cleanup surfaces
// a temp-removal failure (other than ErrNotExist) as a non-nil error. Removing
// write permission on the parent dir blocks unlink with EACCES. It kills the
// CONDITIONALS_NEGATION mutant on atomicfile.go:590: flipping `err != nil` to
// `err == nil` swallows the real removal failure and returns nil.
func TestPendingFile_Cleanup_RemoveFailure_ReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive unlink permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses directory-write EACCES")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	cleanupErr := pf.Cleanup()

	if cleanupErr == nil {
		t.Error("Cleanup() = nil, want error (temp removal was blocked by EACCES)")
	}
}

// TestCleanupStaleTemps_RemovedTemps_LogsInfoNotWarn pins the success-summary
// logging: with exactly one stale temp reclaimed and none failing, the Info
// "removed" line fires once and the Warn "could not be removed" line never
// fires. It kills two mutants:
//   - CONDITIONALS_NEGATION on atomicfile.go:722 (`removed > 0` -> `removed <= 0`):
//     with removed == 1 the real code logs Info; the mutant does not.
//   - CONDITIONALS_BOUNDARY on atomicfile.go:725 (`failed > 0` -> `failed >= 0`):
//     with failed == 0 the real code stays silent; the mutant emits the Warn.
func TestCleanupStaleTemps_RemovedTemps_LogsInfoNotWarn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stale := filepath.Join(dir, ".atomicfile-123456.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed stale temp: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	h := &captureHandler{}

	removed, err := CleanupStaleTemps(dir, time.Hour, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	infoCount := countLogByMessage(h.records, slog.LevelInfo, msgStaleRemoved)
	if infoCount != 1 {
		t.Errorf("Info %q count = %d, want 1 (removed=1 must log the summary)", msgStaleRemoved, infoCount)
	}
	warnCount := countLogByMessage(h.records, slog.LevelWarn, msgStaleRemoveFail)
	if warnCount != 0 {
		t.Errorf("Warn %q count = %d, want 0 (failed=0 must not warn)", msgStaleRemoveFail, warnCount)
	}
}

// TestCleanupStaleTemps_NothingRemoved_DoesNotLogInfo pins that a run which
// reclaims nothing (no matching temps) emits no summary logs. It kills the
// CONDITIONALS_BOUNDARY mutant on atomicfile.go:722 (`removed > 0` ->
// `removed >= 0`): with removed == 0 the real code stays silent, while the
// mutant logs the "removed stale temps" Info line.
func TestCleanupStaleTemps_NothingRemoved_DoesNotLogInfo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.json"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}
	h := &captureHandler{}

	removed, err := CleanupStaleTemps(dir, time.Hour, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	infoCount := countLogByMessage(h.records, slog.LevelInfo, msgStaleRemoved)
	if infoCount != 0 {
		t.Errorf("Info %q count = %d, want 0 (removed=0 must not log the summary)", msgStaleRemoved, infoCount)
	}
	warnCount := countLogByMessage(h.records, slog.LevelWarn, msgStaleRemoveFail)
	if warnCount != 0 {
		t.Errorf("Warn %q count = %d, want 0 (failed=0 must not warn)", msgStaleRemoveFail, warnCount)
	}
}
