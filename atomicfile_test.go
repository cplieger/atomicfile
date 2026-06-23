package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

func TestOptions_Logger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "logged.txt")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := WriteFile(context.Background(), path, []byte("data"), WithLogger(logger)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "data" {
		t.Errorf("got %q, want %q", got, "data")
	}
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

	t.Run("cleanup_noop_after_commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "noop.txt")
		pf, err := NewPendingFile(context.Background(), path)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		if _, err := pf.Write([]byte("data")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := pf.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := pf.Cleanup(); err != nil {
			t.Fatalf("Cleanup after commit: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "data" {
			t.Errorf("file corrupted after Cleanup post-commit")
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
