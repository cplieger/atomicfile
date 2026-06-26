package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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

	t.Run("missing_parent_is_error_without_mkdir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "x", "y", "file.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data")); err == nil {
			t.Fatal("expected error for missing parent without WithMkdirMode")
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

func TestCleanupStaleTemps(t *testing.T) {
	t.Parallel()

	t.Run("removes_old_keeps_recent_and_returns_count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		oldTime := time.Now().Add(-2 * time.Hour)

		stale := filepath.Join(dir, ".atomicfile-987654321.tmp")
		if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		recent := filepath.Join(dir, ".atomicfile-111222333.tmp")
		if err := os.WriteFile(recent, []byte("new"), 0o644); err != nil {
			t.Fatalf("seed recent: %v", err)
		}

		removed, err := CleanupStaleTemps(dir, time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}
		if removed != 1 {
			t.Errorf("removed = %d, want 1", removed)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale temp not removed: stat err = %v", err)
		}
		if _, err := os.Stat(recent); err != nil {
			t.Errorf("recent temp wrongly removed: %v", err)
		}
	})

	t.Run("missing_dir_is_not_an_error", func(t *testing.T) {
		t.Parallel()
		removed, err := CleanupStaleTemps(filepath.Join(t.TempDir(), "nope"), time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps(missing) = %v, want nil", err)
		}
		if removed != 0 {
			t.Errorf("removed = %d, want 0", removed)
		}
	})

	t.Run("spares_caller_files_that_share_prefix_or_suffix", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		oldTime := time.Now().Add(-2 * time.Hour)

		// Caller-owned files that must never be reclaimed even when old.
		spared := []string{
			".atomicfile-notes.tmp",  // non-digit middle
			"config.tmp-backup",      // not the package shape at all
			".atomicfilebackup.tmp",  // no separating digits
			".atomicfile-12ab34.tmp", // mixed alnum middle
		}
		for _, name := range spared {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
				t.Fatalf("seed %q: %v", name, err)
			}
			if err := os.Chtimes(p, oldTime, oldTime); err != nil {
				t.Fatalf("Chtimes %q: %v", name, err)
			}
		}

		// A genuine package temp, old enough to reclaim.
		realTemp := filepath.Join(dir, ".atomicfile-123456.tmp")
		if err := os.WriteFile(realTemp, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed real temp: %v", err)
		}
		if err := os.Chtimes(realTemp, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes real temp: %v", err)
		}

		removed, err := CleanupStaleTemps(dir, time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}
		if removed != 1 {
			t.Errorf("removed = %d, want 1 (only the digit-suffixed temp)", removed)
		}
		for _, name := range spared {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("caller file %q wrongly removed: %v", name, err)
			}
		}
		if _, err := os.Stat(realTemp); !os.IsNotExist(err) {
			t.Errorf("real temp not removed")
		}
	})

	t.Run("real_writes_leave_reclaimable_temps_only_when_orphaned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "live.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		// A successful write leaves no temp, so nothing to reclaim.
		removed, err := CleanupStaleTemps(dir, 0)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}
		if removed != 0 {
			t.Errorf("removed = %d, want 0 (no orphaned temps after a clean write)", removed)
		}
	})
}

func TestCleanupStaleTemps_readdir_error_does_not_panic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive readdir permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses EACCES")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "inaccessible")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir error = %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := CleanupStaleTemps(dir, time.Hour); err == nil {
		t.Error("CleanupStaleTemps(EACCES dir) = nil error, want readdir error")
	}
}

func TestCleanupStaleTemps_continues_after_remove_failure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive unlink permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses directory-write EACCES")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, ".atomicfile-111.tmp")
	other := filepath.Join(dir, ".atomicfile-222.tmp")
	for _, p := range []string{blocked, other} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", p, err)
		}
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes(%q) error = %v", p, err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	// A read-only dir blocks unlink; CleanupStaleTemps must not panic and must
	// report zero removed since every remove fails.
	removed, err := CleanupStaleTemps(dir, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil (per-file failures are skipped)", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (all removes blocked)", removed)
	}
	_ = os.Chmod(dir, 0o755)
	for _, p := range []string{blocked, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("CleanupStaleTemps removed %q despite EACCES: %v", p, err)
		}
	}
}

type captureHandler struct {
	records []slog.Record
	mu      sync.Mutex
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// TestWriteFile_PreserveOwnership_ChownFailure_NonFatal pins the best-effort
// contract: when the temp-file chown fails, the write still lands (data at the
// final path, Result.Durable true) and the failure surfaces as a single Warn.
// Uses the osChown seam because a real chown failure is unforceable from a
// same-owner test. Not parallel: it mutates the package osChown var.
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
	stubOsChown(t, sentinel)

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

func TestCleanupStaleTemps_RemoveFailure_WarnsWithCount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive unlink permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses directory-write EACCES")
	}
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	for _, name := range []string{".atomicfile-111.tmp", ".atomicfile-222.tmp"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes %q: %v", name, err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	h := &captureHandler{}
	removed, err := CleanupStaleTemps(dir, time.Hour, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	var warnCount int
	var failedAttr int64 = -1
	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			warnCount++
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "failed" {
					failedAttr = a.Value.Int64()
				}
				return true
			})
		}
	}
	if warnCount != 1 {
		t.Errorf("warn count = %d, want 1", warnCount)
	}
	if failedAttr != 2 {
		t.Errorf("failed attr = %d, want 2", failedAttr)
	}
}

func TestCleanupStaleTemps_NonPositiveMaxAge_SparesOldTemp(t *testing.T) {
	t.Parallel()
	for _, maxAge := range []time.Duration{0, -time.Hour} {
		dir := t.TempDir()
		stale := filepath.Join(dir, ".atomicfile-987654321.tmp")
		if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(stale, old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		removed, err := CleanupStaleTemps(dir, maxAge)
		if err != nil {
			t.Fatalf("CleanupStaleTemps(%v) = %v", maxAge, err)
		}
		if removed != 0 {
			t.Errorf("CleanupStaleTemps(maxAge=%v) removed = %d, want 0", maxAge, removed)
		}
		if _, err := os.Stat(stale); err != nil {
			t.Errorf("CleanupStaleTemps(maxAge=%v) wrongly removed old temp: %v", maxAge, err)
		}
	}
}

func TestIsStaleTempName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"digit suffix temp", ".atomicfile-123456.tmp", true},
		{"single digit", ".atomicfile-7.tmp", true},
		{"word tail spared", ".atomicfile-notes.tmp", false},
		{"backup tail spared", ".atomicfile-backup.tmp", false},
		{"mixed alnum spared", ".atomicfile-12ab34.tmp", false},
		{"empty middle spared", ".atomicfile-.tmp", false},
		{"no prefix", "atomicfile-123.tmp", false},
		{"wrong prefix", ".atomicfilebackup.tmp", false},
		{"wrong suffix", ".atomicfile-123.bak", false},
		{"unrelated", "config.tmp-backup", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaleTempName(tt.in); got != tt.want {
				t.Errorf("isStaleTempName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsAllDigits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{"0", true},
		{"123456", true},
		{"", false},
		{"12a", false},
		{"a12", false},
		{" 12", false},
		{"-12", false},
	}
	for _, tt := range tests {
		if got := isAllDigits(tt.in); got != tt.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestValidateAbsClean(t *testing.T) {
	t.Parallel()

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Parallel()
		if _, err := validateAbsClean("relative/path"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("validateAbsClean(relative) = %v, want ErrUnsafePath", err)
		}
	})

	t.Run("collapses_traversal_to_clean_absolute_path", func(t *testing.T) {
		t.Parallel()
		got, err := validateAbsClean("/foo/../etc/passwd")
		if err != nil {
			t.Fatalf("validateAbsClean(%q) = error %v, want nil (Clean removes \"..\")",
				"/foo/../etc/passwd", err)
		}
		if got != "/etc/passwd" {
			t.Errorf("validateAbsClean(%q) = %q, want %q", "/foo/../etc/passwd", got, "/etc/passwd")
		}
	})

	t.Run("accepts_absolute_clean_path", func(t *testing.T) {
		t.Parallel()
		got, err := validateAbsClean("/tmp/test.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/tmp/test.txt" {
			t.Errorf("got %q, want %q", got, "/tmp/test.txt")
		}
	})

	t.Run("rejects_empty", func(t *testing.T) {
		t.Parallel()
		if _, err := validateAbsClean(""); !errors.Is(err, ErrEmptyPath) {
			t.Fatalf("validateAbsClean(empty) = %v, want ErrEmptyPath", err)
		}
	})

	t.Run("rejects_null_byte", func(t *testing.T) {
		t.Parallel()
		_, err := validateAbsClean("/tmp/foo\x00bar")
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("validateAbsClean(null) = %v, want ErrUnsafePath", err)
		}
		if !strings.Contains(err.Error(), "null byte") {
			t.Errorf("error = %q, want mention of null byte", err.Error())
		}
	})

	t.Run("rejects_null_byte_suffix", func(t *testing.T) {
		t.Parallel()
		if _, err := validateAbsClean("/tmp/test.txt\x00ignored"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("validateAbsClean(null suffix) = %v, want ErrUnsafePath", err)
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

func TestSymlinkTarget(t *testing.T) {
	t.Parallel()

	t.Run("refuses_symlink_target_by_default", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		_, err := WriteFile(context.Background(), link, []byte("new"))
		if !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("WriteFile(symlink) = %v, want ErrSymlinkTarget", err)
		}
		got, _ := os.ReadFile(real)
		if string(got) != "original" {
			t.Errorf("original file modified: %q", got)
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("allows_symlink_target_with_option", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := WriteFile(context.Background(), link, []byte("new"), WithAllowSymlinkTarget()); err != nil {
			t.Fatalf("WriteFile with AllowSymlinkTarget: %v", err)
		}
		got, _ := os.ReadFile(link)
		if string(got) != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("no_error_for_nonexistent_target", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "new.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("WriteReader_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := WriteReader(context.Background(), link, strings.NewReader("new")); !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("WriteReader(symlink) = %v, want ErrSymlinkTarget", err)
		}
	})

	t.Run("PendingFile_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if _, err := NewPendingFile(context.Background(), link); !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("NewPendingFile(symlink) = %v, want ErrSymlinkTarget", err)
		}
	})
}

func TestWritePhase_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		want  string
		phase WritePhase
	}{
		{"create", "create temp file", PhaseTempCreate},
		{"write", "write temp file", PhaseTempWrite},
		{"chmod", "chmod temp file", PhaseTempChmod},
		{"sync", "sync temp file", PhaseTempSync},
		{"close", "close temp file", PhaseTempClose},
		{"rename", "rename to final path", PhaseRename},
		{"zero_is_unknown", "unknown phase", WritePhase(0)},
		{"out_of_range_is_unknown", "unknown phase", WritePhase(99)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.phase.String(); got != tt.want {
				t.Errorf("WritePhase(%d).String() = %q, want %q", int(tt.phase), got, tt.want)
			}
		})
	}
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

func TestWriteError_Unwrap_PreservesChain(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("underlying io failure")
	we := &WriteError{Phase: PhaseTempWrite, Err: sentinel}
	if !errors.Is(we, sentinel) {
		t.Errorf("errors.Is(WriteError, sentinel) = false, want true")
	}
	if got := we.Unwrap(); got != sentinel {
		t.Errorf("WriteError.Unwrap() = %v, want %v", got, sentinel)
	}
}

func TestWriteError_Error_FormatsPhasePrefix(t *testing.T) {
	t.Parallel()
	we := &WriteError{Phase: PhaseRename, Err: errors.New("disk gone")}
	got := we.Error()
	want := "atomicfile: rename to final path: disk gone"
	if got != want {
		t.Errorf("WriteError.Error() = %q, want %q", got, want)
	}
}

// CleanupStaleTemps must never reclaim a non-regular entry that merely shares
// the .atomicfile-<digits>.tmp shape (os.CreateTemp only makes regular files).
// Pins the info.Mode().IsRegular() guard at cleanup.go:74 (added in the
// per-concern split, previously hits=0). The dir is aged past the cutoff so the
// ONLY thing sparing it is the non-regular skip, not the mtime guard; without
// the guard os.Remove would rmdir the empty aged dir and removed would be 1.
func TestCleanupStaleTemps_SkipsNonRegularTempNamedDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	staleDir := filepath.Join(dir, ".atomicfile-555000111.tmp")
	if err := os.Mkdir(staleDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := CleanupStaleTemps(dir, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTemps: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (a temp-named directory must never be reclaimed)", removed)
	}
	if fi, statErr := os.Stat(staleDir); statErr != nil || !fi.IsDir() {
		t.Errorf("temp-named directory removed/altered (stat err=%v); IsRegular guard must spare it", statErr)
	}
}

// ── WriteFile / WriteReader error and edge-case paths ──────────────────────

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

func TestMkdirMode_BlockedByFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("I am a file"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A regular file in the parent chain makes the symlink pre-check's Lstat
	// fail ENOTDIR before MkdirAll; the write must error and leave no temp.
	path := filepath.Join(blocker, "sub", "file.txt")
	_, err := WriteFile(context.Background(), path, []byte("data"), WithMkdirMode(0o755))
	if err == nil {
		t.Fatal("expected error when the parent chain is blocked by a file")
	}
	assertNoTempLeak(t, dir)
}

// TestWriteFile_MkdirMode_ParentNotWritable_WrapsError reaches the MkdirAll
// failure branch that TestMkdirMode_BlockedByFile cannot: a traversable but
// non-writable parent passes the symlink pre-check (ENOENT on the missing leaf)
// while MkdirAll then fails EACCES, so the error is the "create parent
// directory" wrap of os.ErrPermission, never a *WriteError (which is reserved
// for post-temp-create phases). Non-root only; not parallel, matching the
// sibling permission tests.
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

// ── Path / size validation across entry points ─────────────────────────────

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

func TestEmptyPath_AllEntryPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := WriteFile(ctx, "", []byte("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteFile empty: %v", err)
	}
	if _, err := WriteReader(ctx, "", strings.NewReader("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteReader empty: %v", err)
	}
	if _, err := NewPendingFile(ctx, ""); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("NewPendingFile empty: %v", err)
	}
	if _, err := ReadBounded(ctx, "", 1024); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("ReadBounded empty: %v", err)
	}
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

// ── Concurrency: many writers racing the same path leave it consistent ─────

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

// cancelAt=2 trips the ctx guard at the top of finalizeTempFile (after the temp
// write, before chmod).
func TestWriteFile_CancelAfterWrite_NoTempLeak(t *testing.T) {
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

// cancelAt=3 trips the post-Sync ctx guard inside finalizeTempFile.
func TestWriteFile_CancelAfterSync_NoTempLeak(t *testing.T) {
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

func TestWriteReader_CancelMidBarrier_NoTempLeak(t *testing.T) {
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

// WriteReader takes the io.WriterTo fast path when the source implements it
// (strings.Reader does). writerCtx wraps the destination so the WriterTo write
// still observes ctx cancellation. seqCancelCtx{cancelAt:2} passes the
// openTempForPath ctx check then trips inside the first writerCtx.Write, so the
// write aborts with a PhaseTempWrite WriteError wrapping context.Canceled,
// leaving no final file and no temp.
func TestWriteReader_WriterToFastPath_CancelDuringWrite_NoLeak(t *testing.T) {
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

// ── PendingFile lifecycle / state machine ──────────────────────────────────

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

// ── Best-effort logging (removeTemp, CleanupStaleTemps summaries) ───────────

// A successful temp removal is silent (no Debug log).
func TestRemoveTemp_SuccessfulRemoval_DoesNotLogDebug(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "removable.tmp")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}
	h := &captureHandler{}

	removeTemp(path, slog.New(h))

	if got := countLogByMessage(h.records, slog.LevelDebug, msgRemoveTempFailed); got != 0 {
		t.Errorf("removeTemp(existing removable file) logged %q %d times, want 0", msgRemoveTempFailed, got)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("removeTemp(%q) left file behind: Stat err = %v, want ErrNotExist", path, err)
	}
}

// A removal failing for a reason other than ErrNotExist is logged once at Debug.
// os.Remove of a non-empty directory fails with a non-ErrNotExist error on every
// platform, so no permission tricks are needed.
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

	if got := countLogByMessage(h.records, slog.LevelDebug, msgRemoveTempFailed); got != 1 {
		t.Errorf("removeTemp(non-empty dir) logged %q %d times, want 1", msgRemoveTempFailed, got)
	}
}

// With exactly one stale temp reclaimed and none failing, the Info "removed"
// line fires once and the Warn "could not be removed" line never fires.
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
	if got := countLogByMessage(h.records, slog.LevelInfo, msgStaleRemoved); got != 1 {
		t.Errorf("Info %q count = %d, want 1 (removed=1 must log the summary)", msgStaleRemoved, got)
	}
	if got := countLogByMessage(h.records, slog.LevelWarn, msgStaleRemoveFail); got != 0 {
		t.Errorf("Warn %q count = %d, want 0 (failed=0 must not warn)", msgStaleRemoveFail, got)
	}
}

// A run that reclaims nothing emits no summary logs.
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
	if got := countLogByMessage(h.records, slog.LevelInfo, msgStaleRemoved); got != 0 {
		t.Errorf("Info %q count = %d, want 0 (removed=0 must not log the summary)", msgStaleRemoved, got)
	}
	if got := countLogByMessage(h.records, slog.LevelWarn, msgStaleRemoveFail); got != 0 {
		t.Errorf("Warn %q count = %d, want 0 (failed=0 must not warn)", msgStaleRemoveFail, got)
	}
}
