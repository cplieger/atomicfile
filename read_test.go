package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestReadBounded(t *testing.T) {
	t.Parallel()

	t.Run("reads_file_within_limit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "bounded.txt")
		if err := os.WriteFile(path, []byte("bounded content"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := ReadBounded(context.Background(), path, 1024)
		if err != nil {
			t.Fatalf("ReadBounded: %v", err)
		}
		if string(got) != "bounded content" {
			t.Errorf("got %q, want %q", got, "bounded content")
		}
	})

	t.Run("rejects_file_exceeding_limit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "large.txt")
		if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		_, err := ReadBounded(context.Background(), path, 50)
		if !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("ReadBounded(over) = %v, want ErrFileTooLarge", err)
		}
	})

	t.Run("returns_error_for_missing_file", func(t *testing.T) {
		t.Parallel()
		_, err := ReadBounded(context.Background(), "/nonexistent/path.txt", 1024)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("reads_empty_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.txt")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := ReadBounded(context.Background(), path, 1024)
		if err != nil {
			t.Fatalf("ReadBounded: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d bytes, want 0", len(got))
		}
	})

	t.Run("exact_limit_succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "exact.txt")
		if err := os.WriteFile(path, []byte("12345"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := ReadBounded(context.Background(), path, 5)
		if err != nil {
			t.Fatalf("ReadBounded: %v", err)
		}
		if string(got) != "12345" {
			t.Errorf("got %q, want %q", got, "12345")
		}
	})

	t.Run("maxint64_no_overflow", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "maxint.txt")
		if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := ReadBounded(context.Background(), path, math.MaxInt64)
		if err != nil {
			t.Fatalf("ReadBounded with MaxInt64: %v", err)
		}
		if string(got) != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("context_cancelled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "ctx.txt")
		if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ReadBounded(ctx, path, 1024); !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadBounded(cancelled) = %v, want context.Canceled", err)
		}
	})

	t.Run("empty_path_returns_error", func(t *testing.T) {
		t.Parallel()
		if _, err := ReadBounded(context.Background(), "", 1024); !errors.Is(err, ErrEmptyPath) {
			t.Fatalf("ReadBounded(empty) = %v, want ErrEmptyPath", err)
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
		{"max+0", math.MaxInt64, 0, math.MaxInt64},
		{"max+1_saturates", math.MaxInt64, 1, math.MaxInt64},
		{"max-1+1", math.MaxInt64 - 1, 1, math.MaxInt64},
		{"max-1+2_saturates", math.MaxInt64 - 1, 2, math.MaxInt64},
		{"max+max_saturates", math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{"negative_a", -1, 1, 0},
		{"both_positive", 100, 200, 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := saturateAdd(tt.a, tt.b); got != tt.want {
				t.Errorf("saturateAdd(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
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

// ReadBounded checks ctx before the open and again after the stat. cancelAt=2
// trips the post-stat guard (the open and stat succeed first).
func TestReadBounded_CancelAfterStat(t *testing.T) {
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
