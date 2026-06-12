package atomicfile

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// === SATURATE-ADD / READ-BOUNDED BOUNDARIES ===

func TestRound3_SaturateAdd_MaxInt64(t *testing.T) {
	t.Parallel()
	if got := saturateAdd(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("saturateAdd(MaxInt64, 1) = %d, want MaxInt64", got)
	}
	if got := saturateAdd(math.MaxInt64, math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("saturateAdd(MaxInt64, MaxInt64) = %d, want MaxInt64", got)
	}
}

func TestRound3_ReadBounded_MaxInt64_NoPanic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := ReadBounded(context.Background(), path, math.MaxInt64)
	if err != nil {
		t.Fatalf("ReadBounded(MaxInt64): %v", err)
	}
	if string(data) != "abc" {
		t.Fatalf("got %q", data)
	}
}

func TestRound3_ReadBounded_OneByteOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "over.txt")
	if err := os.WriteFile(path, []byte("abcde"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ReadBounded(context.Background(), path, 4); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("expected ErrFileTooLarge, got: %v", err)
	}
}

func TestRound3_ReadBounded_ExactOneByte(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "one.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := ReadBounded(context.Background(), path, 1)
	if err != nil {
		t.Fatalf("ReadBounded: %v", err)
	}
	if string(data) != "x" {
		t.Fatalf("got %q", data)
	}
}

// ReadBounded checks ctx before the open and again after the stat. cancelAt=2
// trips the post-stat guard (the open and stat succeed first).
func TestRound3_ReadBounded_CancelAfterStat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ctx-after-stat.txt")
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 2}
	if _, err := ReadBounded(ctx, path, 1024); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadBounded(cancel-after-stat) = %v, want context.Canceled", err)
	}
}

// === TEMP LEAK ATTACKS ===

func TestRound3_WriteFile_ContextCancel_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ctx.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = WriteFile(ctx, path, []byte("data"))
	assertNoTempLeak(t, dir)
}

func TestRound3_WriteFile_RenameFailure_CleansTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "isdir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// Renaming onto a non-empty directory fails at PhaseRename.
	if err := os.WriteFile(filepath.Join(target, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := WriteFile(context.Background(), target, []byte("data"))
	if err == nil {
		t.Fatal("expected error renaming onto a directory")
	}
	var we *WriteError
	if !errors.As(err, &we) || we.Phase != PhaseRename {
		t.Fatalf("error = %v, want *WriteError{PhaseRename}", err)
	}
	assertNoTempLeak(t, dir)
}

// === CONCURRENT WRITERS UNDER -race ===

func TestRound3_ConcurrentWriteFile_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.txt")
	var wg sync.WaitGroup
	const N = 20
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = WriteFile(context.Background(), path, []byte(strings.Repeat("x", idx+1)))
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected content length: %d", len(got))
	}
	assertNoTempLeak(t, dir)
}

func TestRound3_ConcurrentPendingFile_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-race.txt")
	var wg sync.WaitGroup
	const N = 15
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pf, err := NewPendingFile(context.Background(), path)
			if err != nil {
				return
			}
			defer func() { _ = pf.Cleanup() }()
			if _, err := pf.Write([]byte(strings.Repeat("y", idx+1))); err != nil {
				return
			}
			_, _ = pf.Commit(context.Background())
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected content length: %d", len(got))
	}
	assertNoTempLeak(t, dir)
}

// === PENDINGFILE STATE MACHINE MISUSE ===

func TestRound3_PendingFile_DoubleCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "double.txt")
	pf, err := NewPendingFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.Write([]byte("once")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := pf.Commit(context.Background()); err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if _, err := pf.Commit(context.Background()); err != nil {
		t.Fatalf("second Commit should be no-op: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "once" {
		t.Fatalf("got %q", got)
	}
}

func TestRound3_PendingFile_DoubleCleanup(t *testing.T) {
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

func TestRound3_PendingFile_CleanupThenCommit(t *testing.T) {
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
	if _, err := pf.Commit(context.Background()); err != nil {
		t.Fatalf("Commit after Cleanup should be no-op: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("file should not exist after Cleanup+Commit")
	}
}

// === READER ERROR ATTACKS ===

func TestRound3_WriteReader_UnexpectedEOF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ueof.txt")
	r := plainReader{r: &errReader{n: 50, err: io.ErrUnexpectedEOF}}
	if _, err := WriteReader(context.Background(), path, r); err == nil {
		t.Fatal("expected error")
	}
	assertNoTempLeak(t, dir)
}

func TestRound3_WriteReader_EmptyReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-reader.txt")
	if _, err := WriteReader(context.Background(), path, strings.NewReader("")); err != nil {
		t.Fatalf("WriteReader(empty): %v", err)
	}
	got, _ := os.ReadFile(path)
	if len(got) != 0 {
		t.Fatalf("expected empty file, got %d bytes", len(got))
	}
}

// === PERMISSION EDGE CASES ===

func TestRound3_WriteFile_ZeroPerm(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "noperm.txt")
	if _, err := WriteFile(context.Background(), path, []byte("secret"), WithMode(0o000)); err != nil {
		t.Fatalf("WriteFile(0o000): %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o000 {
		t.Errorf("mode = %o, want 0000", fi.Mode().Perm())
	}
	_ = os.Chmod(path, 0o644)
}

func TestRound3_PreserveMode_UnusualPerm(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "unusual.txt")
	if err := os.WriteFile(path, []byte("old"), 0o751); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := WriteFile(context.Background(), path, []byte("new"), WithPreserveMode()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o751 {
		t.Errorf("mode = %o, want 0751", fi.Mode().Perm())
	}
}

// === LARGE DATA ===

func TestRound3_WriteFile_LargeData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if _, err := WriteFile(context.Background(), path, data); err != nil {
		t.Fatalf("WriteFile(1MB): %v", err)
	}
	got, _ := os.ReadFile(path)
	if len(got) != len(data) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(data))
	}
}

// === CONTEXT CANCELLATION (whole-call) ===

func TestRound3_NewPendingFile_ContextCancel_NoTempLeak(t *testing.T) {
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

func TestRound3_Commit_ContextCancel_NoTempLeak(t *testing.T) {
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

// === MID-BARRIER CANCELLATION (seqCancelCtx) ===

// WriteFile: cancelAt=2 trips the ctx guard at the top of finalizeTempFile
// (after the temp write, before chmod).
func TestRound3_WriteFile_CancelAfterWrite_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "afterwrite.txt")
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 2}
	_, err := WriteFile(ctx, path, []byte("data"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFile(cancel-after-write) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after mid-write cancellation")
	}
	assertNoTempLeak(t, dir)
}

// WriteFile: cancelAt=3 trips the post-Sync ctx guard inside finalizeTempFile.
func TestRound3_WriteFile_CancelAfterSync_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "aftersync.txt")
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 3}
	_, err := WriteFile(ctx, path, []byte("data"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFile(cancel-after-sync) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after post-sync cancellation")
	}
	assertNoTempLeak(t, dir)
}

func TestRound3_WriteReader_CancelMidBarrier_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wr-midbarrier.txt")
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 2}
	_, err := WriteReader(ctx, path, plainReader{r: strings.NewReader("data")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteReader(cancel-mid-barrier) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after mid-barrier cancellation")
	}
	assertNoTempLeak(t, dir)
}

// PendingFile.Commit: NewPendingFile runs on a live context, so cancelAt=1
// trips the ctx guard at the top of finalizeTempFile during Commit.
func TestRound3_PendingFile_CancelMidBarrier_NoTempLeak(t *testing.T) {
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

// === DURABILITY REPORTING (dir-fsync failure) ===

// When the parent-dir fsync fails after a successful rename, the write is
// written-but-not-durable: nil error, Result.Durable=false, content present.
// This replaces the old IsDurabilityError-based contract. Serial (no
// t.Parallel) because stubFsyncDir mutates the package fsyncDir seam.
func TestRound3_WriteFile_DirSyncFailure_ReportsNotDurable(t *testing.T) {
	stubFsyncDir(t, errors.New("injected dir fsync failure"))
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.txt")
	res, err := WriteFile(context.Background(), path, []byte("payload"))
	if err != nil {
		t.Fatalf("WriteFile(dir-fsync fail) = %v, want nil", err)
	}
	if res.Durable {
		t.Fatalf("Result.Durable = true, want false")
	}
	assertContent(t, path, "payload")
}
