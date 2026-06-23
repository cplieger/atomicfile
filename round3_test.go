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
// committed file, and a later Commit still replays the original result. This
// pins the committed -> Cleanup -> Commit corner of the state machine.
func TestRound3_PendingFile_CommitThenCleanup_KeepsFile(t *testing.T) {
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

// A failed Commit lands in pendingCommitted with a cached *WriteError; a second
// Commit must replay that identical error value (not nil, not ErrAborted, not a
// fresh error) and the same zero Result, without re-running the barrier or
// leaking a temp. Every other double-Commit test replays a SUCCESS (p.err already
// nil on the first call), so this is the only test pinning the cached-error
// replay edge of the 3-state machine.
func TestRound3_PendingFile_FailedCommit_IsIdempotent_ReplaysSameError(t *testing.T) {
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
	// Closing the embedded fd makes the barrier's first step (Chmod) fail, so the
	// first Commit returns a *WriteError{PhaseTempChmod} and its deferred cleanup
	// removes the temp.
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

	// The cached error must be replayed verbatim: same error value (not a fresh
	// one, not nil, not ErrAborted), and the same zero Result.
	if secondErr != firstErr {
		t.Errorf("second Commit err = %v, want the cached first err %v (identical value)", secondErr, firstErr)
	}
	if errors.Is(secondErr, ErrAborted) {
		t.Errorf("second Commit err = %v, want the cached WriteError, not ErrAborted", secondErr)
	}
	if secondRes != firstRes {
		t.Errorf("second Commit result = %+v, want cached %+v", secondRes, firstRes)
	}
	// A replayed (cached) failure must not re-run the barrier or re-create/leak a temp.
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("final file exists after replayed failed Commit, want absent")
	}
	assertNoTempLeak(t, dir)
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

// WriteReader takes the io.WriterTo fast path when the source implements it
// (strings.Reader does). writerCtx wraps the destination so the WriterTo write
// still observes ctx cancellation. seqCancelCtx{cancelAt:2} passes the
// openTempForPath ctx check (call 1) then trips inside writerCtx.Write (call 2),
// the first Write the WriterTo issues. The write must abort with a
// PhaseTempWrite WriteError wrapping context.Canceled, leave no final file, and
// leak no temp.
func TestRound3_WriteReader_WriterToFastPath_CancelDuringWrite_NoLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wt-cancel.txt")

	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 2}
	_, err := WriteReader(ctx, path, strings.NewReader("payload"))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteReader(WriterTo, cancel-during-write) = %v, want context.Canceled", err)
	}
	var we *WriteError
	if !errors.As(err, &we) || we.Phase != PhaseTempWrite {
		t.Fatalf("error = %v, want *WriteError{PhaseTempWrite}", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("final file exists after cancelled WriterTo write, want absent")
	}
	assertNoTempLeak(t, dir)
}

// finalizeTempFile tags a chmod failure as PhaseTempChmod and the caller cleans
// up the temp. A real chmod failure is impractical to force on a healthy fs,
// but PendingFile embeds *os.File, so closing the temp fd before Commit makes
// the barrier's first step (Chmod) fail with "file already closed". Commit must
// return a *WriteError{PhaseTempChmod} and remove the temp. Pins both the phase
// tag at the chmod call site (only the String() method was previously tested)
// and the on-barrier-failure temp cleanup.
func TestRound3_PendingFile_Commit_ChmodBarrierFailure_TaggedAndCleansTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "chmod-fail.txt")

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

	_, commitErr := pf.Commit(context.Background())

	var we *WriteError
	if !errors.As(commitErr, &we) || we.Phase != PhaseTempChmod {
		t.Fatalf("Commit(closed fd) = %v, want *WriteError{PhaseTempChmod}", commitErr)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("final file exists after barrier failure, want absent")
	}
	assertNoTempLeak(t, dir)
}

// TestMkdirMode_BlockedByFile (adversarial_test.go) intends to cover the
// MkdirAll-failure branch but never reaches it: its path traverses a regular
// file, so checkSymlink's os.Lstat fails ENOTDIR and openTempForPath returns
// before MkdirAll. A traversable-but-non-writable parent makes the Lstat
// pre-check pass (ENOENT) while MkdirAll fails EACCES, reaching the
// "create parent directory" wrap. Non-root only (root bypasses EACCES); not
// t.Parallel, matching the sibling TestWriteFile_ro_dir permission test.
func TestRound3_WriteFile_MkdirMode_ParentNotWritable_WrapsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses EACCES")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o555); err != nil { // traversable (x), not writable (no w)
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	// ro is a real dir, so the symlink pre-check Lstat sees a clean ENOENT on the
	// missing leaf and passes; MkdirAll(ro/sub) then fails EACCES.
	path := filepath.Join(ro, "sub", "file.txt")
	_, err := WriteFile(context.Background(), path, []byte("data"), WithMkdirMode(0o755))

	if err == nil {
		t.Fatal("expected error creating a parent directory under a non-writable dir")
	}
	if !strings.Contains(err.Error(), "create parent directory") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "create parent directory")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want it to wrap os.ErrPermission", err)
	}
	// The mkdir failure precedes temp creation, so it is a plain wrapped error,
	// never a *WriteError (reserved for post-temp-create phases).
	var we *WriteError
	if errors.As(err, &we) {
		t.Errorf("error is *WriteError{Phase=%v}, want a plain wrapped error", we.Phase)
	}
	assertNoTempLeak(t, dir)
}

// The PendingFile rename-failure branch (commit() -> commitTemp error ->
// write.go:349 `return Result{}, cErr`) is otherwise uncovered: the existing
// TestRound3_WriteFile_RenameFailure_CleansTemp exercises the identically-shaped
// branch in writeAtomic, a different function. Committing onto a NON-EMPTY
// directory forces os.Rename to fail with ENOTEMPTY on every platform without
// test seams. The first Commit must tag PhaseRename and clean the temp; a second
// Commit must replay the same cached error.
func TestRound3_PendingFile_Commit_RenameFailure_TaggedCleansTempAndReplays(t *testing.T) {
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
// internal p.Close() in Cleanup return os.ErrClosed (logged at Debug,
// write.go:370-373, previously hits=0), which Cleanup MUST swallow and still
// remove the temp + return nil. A return-clErr regression would leak the temp.
func TestRound3_PendingFile_Cleanup_CloseFailure_StillRemovesTemp(t *testing.T) {
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
