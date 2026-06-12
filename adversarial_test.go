package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

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

func TestWriteFile_SymlinkInParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	realDir := filepath.Join(dir, "realdir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	linkDir := filepath.Join(dir, "linkdir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// The target file itself is not a symlink, only an ancestor directory is,
	// so the write is permitted and lands in the real directory.
	path := filepath.Join(linkDir, "file.txt")
	if _, err := WriteFile(context.Background(), path, []byte("through symlink parent")); err != nil {
		t.Fatalf("WriteFile through symlink parent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(realDir, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile from realdir: %v", err)
	}
	if string(got) != "through symlink parent" {
		t.Errorf("got %q", got)
	}
}

func TestPreserveOwnership_WithoutPrivilege(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("test requires non-root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "owned.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Preserving ownership to the same (current) user is a no-op chown that
	// succeeds without privilege.
	if _, err := WriteFile(context.Background(), path, []byte("new"), WithPreserveOwnership()); err != nil {
		t.Fatalf("PreserveOwnership to same user should succeed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("got %q", got)
	}
}

func TestMkdirMode_BlockedByFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("I am a file"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path := filepath.Join(blocker, "sub", "file.txt")
	_, err := WriteFile(context.Background(), path, []byte("data"), WithMkdirMode(0o755))
	if err == nil {
		t.Fatal("expected error when MkdirAll is blocked by a file")
	}
	assertNoTempLeak(t, dir)
}

func TestReadBounded_ZeroMax(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nonempty.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ReadBounded(context.Background(), path, 0); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadBounded(0) = %v, want ErrFileTooLarge", err)
	}
}

func TestReadBounded_ZeroMax_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := ReadBounded(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ReadBounded(empty, 0): %v", err)
	}
	if len(data) != 0 {
		t.Errorf("got %d bytes", len(data))
	}
}

func TestReadBounded_NegativeMax(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ReadBounded(context.Background(), path, -1); err == nil {
		t.Fatal("expected error for negative maxBytes")
	}
}

func TestNullByte_AllEntryPoints(t *testing.T) {
	t.Parallel()
	nullPath := "/tmp/test\x00evil"

	t.Run("WriteFile", func(t *testing.T) {
		if _, err := WriteFile(context.Background(), nullPath, []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
	t.Run("WriteReader", func(t *testing.T) {
		if _, err := WriteReader(context.Background(), nullPath, strings.NewReader("x")); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
	t.Run("NewPendingFile", func(t *testing.T) {
		if _, err := NewPendingFile(context.Background(), nullPath); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
	t.Run("ReadBounded", func(t *testing.T) {
		if _, err := ReadBounded(context.Background(), nullPath, 1024); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("got %v, want ErrUnsafePath", err)
		}
	})
}

func TestSaturateAdd_EdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"zero+zero", 0, 0, 0},
		{"zero+one", 0, 1, 1},
		{"max+0", 9223372036854775807, 0, 9223372036854775807},
		{"max+1_saturates", 9223372036854775807, 1, 9223372036854775807},
		{"max-1+1", 9223372036854775806, 1, 9223372036854775807},
		{"max-1+2_saturates", 9223372036854775806, 2, 9223372036854775807},
		{"negative_a", -1, 1, 0},
		{"both_positive", 100, 200, 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := saturateAdd(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("saturateAdd(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestReadBounded_GrowsPastLimitDuringRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("FIFO not supported on Windows")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("Mkfifo unsupported: %v", err)
	}
	// A FIFO reports Size()==0 from Stat, so the pre-read size gate passes;
	// the post-read length recheck is the only guard that can catch the
	// overflow. A regular file of this size would be rejected earlier.
	const limit = 8
	payload := bytes.Repeat([]byte("x"), limit+4)
	go func() {
		w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer w.Close()
		_, _ = w.Write(payload)
	}()
	if _, err := ReadBounded(context.Background(), fifo, limit); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("ReadBounded(fifo overflow) = %v, want ErrFileTooLarge", err)
	}
}
