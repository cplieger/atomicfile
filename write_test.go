package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestWriteFile(t *testing.T) {
	t.Parallel()

	t.Run("basic_write_and_read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "test.txt")
		res, err := WriteFile(context.Background(), path, []byte("hello world"))
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if res.Path != path {
			t.Errorf("Result.Path = %q, want %q", res.Path, path)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true for a synced write")
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if string(got) != "hello world" {
			t.Errorf("got %q, want %q", got, "hello world")
		}
	})

	t.Run("overwrites_existing_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "overwrite.txt")
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatalf("seed WriteFile: %v", err)
		}
		if _, err := WriteFile(context.Background(), path, []byte("new")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("empty_path_returns_error", func(t *testing.T) {
		t.Parallel()
		if _, err := WriteFile(context.Background(), "", []byte("data")); !errors.Is(err, ErrEmptyPath) {
			t.Fatalf("WriteFile(empty) = %v, want ErrEmptyPath", err)
		}
	})

	t.Run("relative_path_returns_error", func(t *testing.T) {
		t.Parallel()
		if _, err := WriteFile(context.Background(), "relative/path.txt", []byte("data")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("WriteFile(relative) = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("empty_data_creates_empty_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.txt")
		if _, err := WriteFile(context.Background(), path, nil); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if len(got) != 0 {
			t.Errorf("got %d bytes, want 0", len(got))
		}
	})

	t.Run("respects_file_permissions", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file mode not meaningful on Windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "perms.txt")
		if _, err := WriteFile(context.Background(), path, []byte("x"), WithMode(0o600)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("permissions = %o, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("context_cancelled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "cancelled.txt")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := WriteFile(ctx, path, []byte("data"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteFile(cancelled) = %v, want context.Canceled", err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Error("file should not exist after cancelled write")
		}
	})

	// A missing parent without WithMkdirMode must fail with PhaseTempCreate:
	// the destination could not be opened for temp creation. vibekit's
	// transient-failure classification branches on exactly this phase, so the
	// adapter's os.OpenRoot failure mapping is a pinned contract, not an
	// implementation detail.
	t.Run("missing_parent_is_PhaseTempCreate_without_mkdir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "x", "y", "file.txt")
		_, err := WriteFile(context.Background(), path, []byte("data"))
		if err == nil {
			t.Fatal("expected error for missing parent without WithMkdirMode")
		}
		var we *WriteError
		if !errors.As(err, &we) {
			t.Fatalf("error = %T (%v), want *WriteError", err, err)
		}
		if we.Phase != PhaseTempCreate {
			t.Errorf("WriteError.Phase = %v, want PhaseTempCreate", we.Phase)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want it to wrap fs.ErrNotExist", err)
		}
	})
}

func TestWriteFile_Durable(t *testing.T) {
	t.Parallel()

	t.Run("synced_write_is_durable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "durable.txt")
		res, err := WriteFile(context.Background(), path, []byte("x"))
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true")
		}
	})

	t.Run("nosync_write_is_not_durable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nodurable.txt")
		res, err := WriteFile(context.Background(), path, []byte("x"), WithNoSync())
		if err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false under WithNoSync")
		}
		got, _ := os.ReadFile(path)
		if string(got) != "x" {
			t.Errorf("got %q, want %q", got, "x")
		}
	})
}

func TestWriteFile_ro_dir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses EACCES")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	path := filepath.Join(dir, "out.txt")
	_, err := WriteFile(context.Background(), path, []byte("data"))
	if err == nil {
		t.Fatal("expected error for read-only dir")
	}
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("error = %T, want *WriteError", err)
	}
	if we.Phase != PhaseTempCreate {
		t.Errorf("WriteError.Phase = %v, want PhaseTempCreate", we.Phase)
	}
}

func TestWriteFile_ZeroPerm(t *testing.T) {
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

func TestWriteFile_RenameFailure_ReportsRenamePhase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "iam-a-dir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	_, err := WriteFile(context.Background(), target, []byte("data"))
	if err == nil {
		t.Fatal("WriteFile(dir target) = nil, want error")
	}
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("error = %T, want *WriteError", err)
	}
	if we.Phase != PhaseRename {
		t.Errorf("WriteError.Phase = %v, want PhaseRename", we.Phase)
	}
}

func TestWriteReader(t *testing.T) {
	t.Parallel()

	t.Run("basic_stream", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "stream.txt")
		// A custom reader that is NOT an io.WriterTo exercises the readerCtx path.
		r := plainReader{r: strings.NewReader("streamed content")}
		res, err := WriteReader(context.Background(), path, r)
		if err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true")
		}
		got, _ := os.ReadFile(path)
		if string(got) != "streamed content" {
			t.Errorf("got %q, want %q", got, "streamed content")
		}
	})

	t.Run("uses_WriterTo_optimization", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "writerto.txt")
		// bytes.Reader implements io.WriterTo, exercising the writerCtx path.
		r := bytes.NewReader([]byte("via WriterTo"))
		if _, err := WriteReader(context.Background(), path, r); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "via WriterTo" {
			t.Errorf("got %q, want %q", got, "via WriterTo")
		}
	})

	t.Run("respects_mode_via_WithMode", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file mode not meaningful on Windows")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "mode.txt")
		r := strings.NewReader("x")
		if _, err := WriteReader(context.Background(), path, r, WithMode(0o600)); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("context_cancelled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "cancelled.txt")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := WriteReader(ctx, path, strings.NewReader("x"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteReader(cancelled) = %v, want context.Canceled", err)
		}
	})

	t.Run("empty_path_error", func(t *testing.T) {
		t.Parallel()
		if _, err := WriteReader(context.Background(), "", strings.NewReader("x")); !errors.Is(err, ErrEmptyPath) {
			t.Fatalf("WriteReader(empty) = %v, want ErrEmptyPath", err)
		}
	})

	t.Run("mkdir_mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "deep", "file.txt")
		if _, err := WriteReader(context.Background(), path, strings.NewReader("nested"), WithMkdirMode(0o755)); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "nested" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("nosync_is_not_durable", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync.txt")
		res, err := WriteReader(context.Background(), path, strings.NewReader("fast"), WithNoSync())
		if err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false under WithNoSync")
		}
	})
}

// writerToCancelReader is an io.WriterTo whose WriteTo issues several small
// writes, so a per-chunk writerCtx cancellation can interrupt it.
type writerToCancelReader struct {
	chunks int
}

func (w *writerToCancelReader) Read([]byte) (int, error) { return 0, io.EOF }

func (w *writerToCancelReader) WriteTo(dst io.Writer) (int64, error) {
	var total int64
	for range w.chunks {
		n, err := dst.Write([]byte("chunk"))
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func TestWriteReader_WriterToCancellation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wt-cancel.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Even though the source is an io.WriterTo, writerCtx makes the first
	// chunk write observe the cancelled context.
	_, err := WriteReader(ctx, path, &writerToCancelReader{chunks: 4})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteReader(WriterTo, cancelled) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after cancellation")
	}
	assertNoTempLeak(t, dir)
}

func TestWriteReader_ErroringReader_CleansUpTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "errreader.txt")
	// plainReader hides the WriterTo implementation, forcing the io.Copy path.
	r := plainReader{r: &errReader{n: 100, err: errors.New("simulated IO error")}}
	_, err := WriteReader(context.Background(), path, r)
	if err == nil {
		t.Fatal("expected error from erroring reader")
	}
	var we *WriteError
	if !errors.As(err, &we) {
		t.Fatalf("error = %T, want *WriteError", err)
	}
	if we.Phase != PhaseTempWrite {
		t.Errorf("WriteError.Phase = %v, want PhaseTempWrite", we.Phase)
	}
	assertNoTempLeak(t, dir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("target file should not exist after reader error")
	}
}

func TestWriteReader_WriterTo_Error_CleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "writerto-err.txt")
	r := &errWriterTo{err: errors.New("WriterTo failure")}
	_, err := WriteReader(context.Background(), path, r)
	if err == nil {
		t.Fatal("expected error")
	}
	assertNoTempLeak(t, dir)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("target file should not exist after WriterTo error")
	}
}

func TestWriteReader_EmptyReader(t *testing.T) {
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

func TestWriteReader_NilReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nilreader.txt")
	if _, err := WriteReader(context.Background(), path, nil); err == nil {
		t.Fatal("WriteReader(nil reader) = nil, want non-nil error")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("no target file should exist after nil-reader error")
	}
	assertNoTempLeak(t, dir)
}

func TestMkdirMode(t *testing.T) {
	t.Parallel()

	t.Run("WriteFile_creates_dirs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "file.txt")
		if _, err := WriteFile(context.Background(), path, []byte("nested"), WithMkdirMode(0o755)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "nested" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("WriteFile_no_mkdir_by_default", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "x", "y", "file.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data")); err == nil {
			t.Fatal("expected error without MkdirMode")
		}
	})
}

func TestMkdirMode_BlockedByFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("I am a file"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A regular file in the parent chain makes MkdirAll fail ENOTDIR; the
	// write must error and leave no temp.
	path := filepath.Join(blocker, "sub", "file.txt")
	_, err := WriteFile(context.Background(), path, []byte("data"), WithMkdirMode(0o755))
	if err == nil {
		t.Fatal("expected error when the parent chain is blocked by a file")
	}
	assertNoTempLeak(t, dir)
}

// TestWriteFile_MkdirMode_ParentNotWritable_WrapsError pins the MkdirAll
// failure contract: a traversable but non-writable parent makes MkdirAll fail
// EACCES, so the error is the "create parent directory" wrap of
// os.ErrPermission, never a *WriteError (which is reserved for the
// destination-open and post-temp-create phases). Non-root only; not parallel,
// matching the sibling permission tests.
func TestWriteFile_MkdirMode_ParentNotWritable_WrapsError(t *testing.T) {
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
	var we *WriteError
	if errors.As(err, &we) {
		t.Errorf("error is *WriteError{Phase=%v}, want a plain wrapped error", we.Phase)
	}
	assertNoTempLeak(t, dir)
}

func TestWriteFile_ConcurrentSamePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "race.txt")
	var wg sync.WaitGroup
	const N = 30
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = WriteFile(context.Background(), path, []byte(strings.Repeat("A", idx+1)))
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

func TestWriteReader_ConcurrentSamePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "wr-race.txt")
	var wg sync.WaitGroup
	const N = 15
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = WriteReader(context.Background(), p, bytes.NewReader(bytes.Repeat([]byte{byte(idx)}, idx+1)))
		}(i)
	}
	wg.Wait()
	assertNoTempLeak(t, dir)
}

// ── Cancellation: whole-call and mid-barrier, never leaving a partial file ─

func TestWrite_CancelledContext_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "cancel.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _ = WriteFile(ctx, p, []byte("data"))
	_, _ = WriteReader(ctx, p, strings.NewReader("data"))

	assertNoTempLeak(t, dir)
}

// The absolute write path checks ctx at openParentRoot (1) and openTempForRoot
// (2); cancelAt=3 therefore trips the ctx guard at the top of finalizeTempFile
// (after the temp write, before chmod).
func TestWriteFile_CancelAfterWrite_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "afterwrite.txt")
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 3}
	_, err := WriteFile(ctx, path, []byte("data"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFile(cancel-after-write) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after mid-write cancellation")
	}
	assertNoTempLeak(t, dir)
}

// cancelAt=4 trips the post-Sync ctx guard inside finalizeTempFile (checks 1-2
// are the openParentRoot and openTempForRoot preamble guards, 3 the pre-chmod
// guard).
func TestWriteFile_CancelAfterSync_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "aftersync.txt")
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 4}
	_, err := WriteFile(ctx, path, []byte("data"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteFile(cancel-after-sync) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after post-sync cancellation")
	}
	assertNoTempLeak(t, dir)
}

func TestWriteReader_CancelMidBarrier_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wr-midbarrier.txt")
	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 3}
	_, err := WriteReader(ctx, path, plainReader{r: strings.NewReader("data")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteReader(cancel-mid-barrier) = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("final file should not exist after mid-barrier cancellation")
	}
	assertNoTempLeak(t, dir)
}

// WriteReader takes the io.WriterTo fast path when the source implements it
// (strings.Reader does). writerCtx wraps the destination so the WriterTo write
// still observes ctx cancellation. seqCancelCtx{cancelAt:3} passes the
// openParentRoot and openTempForRoot ctx checks then trips inside the first
// writerCtx.Write, so the write aborts with a PhaseTempWrite WriteError
// wrapping context.Canceled, leaving no final file and no temp.
func TestWriteReader_WriterToFastPath_CancelDuringWrite_NoLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wt-cancel.txt")

	ctx := &seqCancelCtx{Context: context.Background(), cancelAt: 3}
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
