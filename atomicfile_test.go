package atomicfile

import (
	"bytes"
	"context"
	"errors"
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
		if err := WriteFile(context.Background(), path, []byte("hello world")); err != nil {
			t.Fatalf("WriteFile: %v", err)
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
		os.WriteFile(path, []byte("old"), 0o644)
		if err := WriteFile(context.Background(), path, []byte("new")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("empty_path_returns_error", func(t *testing.T) {
		t.Parallel()
		if err := WriteFile(context.Background(), "", []byte("data")); err == nil {
			t.Fatal("expected error for empty path")
		}
	})

	t.Run("empty_data_creates_empty_file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "empty.txt")
		if err := WriteFile(context.Background(), path, nil); err != nil {
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
		if err := WriteFile(context.Background(), path, []byte("x"), WithMode(0o600)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("permissions = %o, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("custom_temp_pattern", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "custom.txt")
		if err := WriteFile(context.Background(), path, []byte("data"), WithTempPattern(".myapp-*.tmp")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "data" {
			t.Errorf("got %q, want %q", got, "data")
		}
	})

	t.Run("context_cancelled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "cancelled.txt")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := WriteFile(ctx, path, []byte("data"))
		if err == nil {
			t.Fatal("expected error for cancelled context")
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Error("file should not exist after cancelled write")
		}
	})
}

func TestPrepareAndCommit(t *testing.T) {
	t.Parallel()

	t.Run("creates_temp_file_ready_for_rename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "final.txt")
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("prepared data"))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cleanup()
		got, err := os.ReadFile(tmpPath)
		if err != nil {
			t.Fatalf("ReadFile(tmp): %v", err)
		}
		if string(got) != "prepared data" {
			t.Errorf("tmp content = %q, want %q", got, "prepared data")
		}
	})

	t.Run("commit_renames_to_final", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "committed.txt")
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("commit me"))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cleanup()
		if err := Commit(tmpPath, path); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "commit me" {
			t.Errorf("final content = %q, want %q", got, "commit me")
		}
	})

	t.Run("empty_path_returns_error", func(t *testing.T) {
		t.Parallel()
		_, _, err := Prepare(context.Background(), "", []byte("x"))
		if err == nil {
			t.Fatal("expected error for empty path")
		}
	})
}

func TestReadBounded(t *testing.T) {
	t.Parallel()

	t.Run("reads_file_within_limit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "bounded.txt")
		os.WriteFile(path, []byte("bounded content"), 0o644)
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
		os.WriteFile(path, make([]byte, 100), 0o644)
		_, err := ReadBounded(context.Background(), path, 50)
		if err == nil {
			t.Fatal("expected error for file exceeding limit")
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
		os.WriteFile(path, nil, 0o644)
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
		os.WriteFile(path, []byte("12345"), 0o644)
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
		os.WriteFile(path, []byte("hello"), 0o644)
		got, err := ReadBounded(context.Background(), path, math.MaxInt64)
		if err != nil {
			t.Fatalf("ReadBounded with MaxInt64: %v", err)
		}
		if string(got) != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})
}

func TestSaveBytes(t *testing.T) {
	t.Parallel()

	t.Run("round_trips_content_and_applies_mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("file mode semantics differ on Windows")
		}
		tests := []struct {
			name    string
			data    []byte
			perm    os.FileMode
			dirPerm os.FileMode
		}{
			{"empty", []byte{}, 0o644, 0o755},
			{"text", []byte("hello world\n"), 0o644, 0o755},
			{"binary", []byte{0x00, 0x01, 0xff, 0x7f, 0x80}, 0o644, 0o755},
			{"private perm triggers 0700 parent", []byte("secret"), 0o600, 0o700},
			{"world-readable stays 0755", []byte("pub"), 0o664, 0o755},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "nested")
				path := filepath.Join(dir, "out.bin")
				if err := SaveBytes(path, tt.data, tt.perm); err != nil {
					t.Fatalf("SaveBytes: %v", err)
				}
				got, _ := os.ReadFile(path)
				if !bytes.Equal(got, tt.data) {
					t.Errorf("round-trip mismatch")
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != tt.perm {
					t.Errorf("file mode = %o, want %o", info.Mode().Perm(), tt.perm)
				}
				dirInfo, _ := os.Stat(dir)
				if dirInfo.Mode().Perm() != tt.dirPerm {
					t.Errorf("dir mode = %o, want %o", dirInfo.Mode().Perm(), tt.dirPerm)
				}
			})
		}
	})

	t.Run("parent_unusable_returns_error", func(t *testing.T) {
		root := t.TempDir()
		blocker := filepath.Join(root, "blocker")
		os.WriteFile(blocker, []byte("x"), 0o644)
		path := filepath.Join(blocker, "child", "out.bin")
		if err := SaveBytes(path, []byte("data"), 0o644); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("rename_error_cleans_up_temp", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "target")
		os.Mkdir(path, 0o755)
		if err := SaveBytes(path, []byte("data"), 0o644); err == nil {
			t.Fatal("expected error")
		}
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("stale temp file: %q", e.Name())
			}
		}
	})
}

func TestSaveJSON(t *testing.T) {
	t.Parallel()

	t.Run("basic", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "data.json")
		var mu sync.Mutex
		if err := SaveJSON(path, &mu, map[string]int{"x": 1}, "test", 0o644); err != nil {
			t.Fatalf("SaveJSON: %v", err)
		}
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), `"x": 1`) {
			t.Errorf("content = %q", string(data))
		}
	})

	t.Run("nil_mutex_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "out.json")
		err := SaveJSON(path, nil, map[string]int{"x": 1}, "test", 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "nil mutex") {
			t.Errorf("error = %q", err.Error())
		}
	})

	t.Run("marshal_error_does_not_create_file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "out.json")
		var mu sync.Mutex
		if err := SaveJSON(path, &mu, make(chan int), "test", 0o644); err == nil {
			t.Error("expected marshal error")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("file created on marshal error")
		}
	})
}

func TestCleanupStaleTemps(t *testing.T) {
	t.Parallel()

	t.Run("removes_old_keeps_recent", func(t *testing.T) {
		dir := t.TempDir()
		recent := filepath.Join(dir, "live.json.tmp-1111")
		os.WriteFile(recent, []byte("new"), 0o644)
		old := filepath.Join(dir, "data.json.tmp-2222")
		os.WriteFile(old, []byte("old"), 0o644)
		oldTime := time.Now().Add(-2 * time.Hour)
		os.Chtimes(old, oldTime, oldTime)
		canonical := filepath.Join(dir, "data.json")
		os.WriteFile(canonical, []byte(`{}`), 0o644)
		os.Chtimes(canonical, oldTime, oldTime)

		CleanupStaleTemps(dir, time.Hour)

		if _, err := os.Stat(recent); err != nil {
			t.Errorf("recent temp removed")
		}
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Errorf("old temp not removed")
		}
		if _, err := os.Stat(canonical); err != nil {
			t.Errorf("canonical file removed")
		}
	})

	t.Run("missing_dir_does_not_panic", func(t *testing.T) {
		CleanupStaleTemps("/nonexistent-dir-xyz", time.Hour)
	})

	t.Run("preserves_user_file_with_tmp_in_name", func(t *testing.T) {
		dir := t.TempDir()
		userFile := filepath.Join(dir, "alice.tmp-2024-notes.json")
		os.WriteFile(userFile, []byte("{}"), 0o644)
		oldTime := time.Now().Add(-2 * time.Hour)
		os.Chtimes(userFile, oldTime, oldTime)
		tmp := filepath.Join(dir, "data.json.tmp-abc123")
		os.WriteFile(tmp, []byte("x"), 0o644)
		os.Chtimes(tmp, oldTime, oldTime)

		CleanupStaleTemps(dir, time.Hour)

		if _, err := os.Stat(userFile); err != nil {
			t.Errorf("user file removed")
		}
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Errorf("real temp not removed")
		}
	})

	t.Run("removes_default_prefix_convention", func(t *testing.T) {
		dir := t.TempDir()
		// The convention WriteFile/WriteReader/Prepare/PendingFile emit.
		stale := filepath.Join(dir, ".atomicfile-987654321.tmp")
		if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		// A recent default-prefix temp must be preserved.
		recent := filepath.Join(dir, ".atomicfile-111222333.tmp")
		if err := os.WriteFile(recent, []byte("new"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		CleanupStaleTemps(dir, time.Hour)

		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale default-prefix temp not removed: stat err = %v", err)
		}
		if _, err := os.Stat(recent); err != nil {
			t.Errorf("recent default-prefix temp wrongly removed: %v", err)
		}
	})
}

func TestCleanupStaleTemps_CustomPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oldTime := time.Now().Add(-2 * time.Hour)
	custom := filepath.Join(dir, ".myapp-abc123.tmp")
	if err := os.WriteFile(custom, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_ = os.Chtimes(custom, oldTime, oldTime)

	// Without the matching option, a custom-pattern temp is NOT reclaimed.
	CleanupStaleTemps(dir, time.Hour)
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("custom temp reclaimed without WithTempPattern: %v", err)
	}
	// With the same WithTempPattern used for writes, it IS reclaimed.
	CleanupStaleTemps(dir, time.Hour, WithTempPattern(".myapp-*.tmp"))
	if _, err := os.Stat(custom); !os.IsNotExist(err) {
		t.Errorf("custom temp not reclaimed with WithTempPattern")
	}
}

func TestIsStaleTempName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		pattern string
		want    bool
	}{
		{"basic temp", "data.json.tmp-abc123", "", true},
		{"no signature", "regular.json", "", false},
		{"suffix contains dot", "alice.tmp-2024-notes.json", "", false},
		{"suffix contains slash", "foo.tmp-a/b", "", false},
		{"nothing after suffix", "just.tmp-", "", false},
		{"empty", "", "", false},
		{"default prefix temp", ".atomicfile-987654321.tmp", "", true},
		{"user dotfile not matched", ".atomicfilebackup.tmp", "", false},
		{"custom pattern matched with opt", ".myapp-xyz.tmp", ".myapp-*.tmp", true},
		{"custom pattern ignored without opt", ".myapp-xyz.tmp", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaleTempName(tt.in, tt.pattern); got != tt.want {
				t.Errorf("isStaleTempName(%q,%q) = %v, want %v", tt.in, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestValidateAbsClean(t *testing.T) {
	t.Parallel()

	t.Run("rejects_relative_path", func(t *testing.T) {
		_, err := validateAbsClean("relative/path")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("collapses_traversal_to_clean_absolute_path", func(t *testing.T) {
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
		got, err := validateAbsClean("/tmp/test.txt")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "/tmp/test.txt" {
			t.Errorf("got %q, want %q", got, "/tmp/test.txt")
		}
	})

	t.Run("rejects_null_byte", func(t *testing.T) {
		_, err := validateAbsClean("/tmp/foo\x00bar")
		if err == nil {
			t.Fatal("expected error for null byte in path")
		}
		if !strings.Contains(err.Error(), "null byte") {
			t.Errorf("error = %q, want mention of null byte", err.Error())
		}
	})

	t.Run("rejects_null_byte_suffix", func(t *testing.T) {
		_, err := validateAbsClean("/tmp/test.txt\x00ignored")
		if err == nil {
			t.Fatal("expected error for null byte in path")
		}
	})
}

func TestSaveBytes_writeTempFile_error_is_propagated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive this path")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses EACCES on read-only directories")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	path := filepath.Join(dir, "out.bin")
	err := SaveBytes(path, []byte("data"), 0o644)
	if err == nil {
		t.Fatal("SaveBytes(ro parent) = nil, want error")
	}
	_ = os.Chmod(dir, 0o755)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("SaveBytes(ro parent) left stale temp file: %q", e.Name())
		}
	}
}

func TestSaveJSON_applies_perm_mode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	var mu sync.Mutex
	if err := SaveJSON(path, &mu, map[string]int{"x": 1}, "test", 0o600); err != nil {
		t.Fatalf("SaveJSON error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 0600", got)
	}
}

func TestSaveJSON_leaves_no_temp_file_on_success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	var mu sync.Mutex
	if err := SaveJSON(path, &mu, map[string]int{"x": 1}, "test", 0o644); err != nil {
		t.Fatalf("SaveJSON error = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("stale temp file: %q", e.Name())
		}
	}
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
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("CleanupStaleTemps panicked on EACCES: %v", r)
		}
	}()
	CleanupStaleTemps(dir, time.Hour)
}

func TestCleanupStaleTemps_continues_after_remove_failure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive unlink permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses directory-write EACCES")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "a.json.tmp-aaa")
	sweepable := filepath.Join(dir, "b.json.tmp-bbb")
	for _, p := range []string{blocked, sweepable} {
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
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("CleanupStaleTemps panicked on remove failure: %v", r)
		}
	}()
	CleanupStaleTemps(dir, time.Hour)
	_ = os.Chmod(dir, 0o755)
	for _, p := range []string{blocked, sweepable} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("CleanupStaleTemps removed %q despite EACCES: %v", p, err)
		}
	}
}

func TestOptions_Logger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "logged.txt")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := WriteFile(context.Background(), path, []byte("data"), WithLogger(logger)); err != nil {
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
	os.Chmod(dir, 0o555)
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	path := filepath.Join(dir, "out.txt")
	err := WriteFile(context.Background(), path, []byte("data"))
	if err == nil {
		t.Fatal("expected error for read-only dir")
	}
}

func TestWriteReader(t *testing.T) {
	t.Parallel()

	t.Run("basic_stream", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "stream.txt")
		r := strings.NewReader("streamed content")
		if err := WriteReader(context.Background(), path, r, 0o644); err != nil {
			t.Fatalf("WriteReader: %v", err)
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
		r := bytes.NewReader([]byte("via WriterTo"))
		if err := WriteReader(context.Background(), path, r, 0o644); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "via WriterTo" {
			t.Errorf("got %q, want %q", got, "via WriterTo")
		}
	})

	t.Run("respects_mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "mode.txt")
		r := strings.NewReader("x")
		if err := WriteReader(context.Background(), path, r, 0o600); err != nil {
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
		err := WriteReader(ctx, path, strings.NewReader("x"), 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("empty_path_error", func(t *testing.T) {
		t.Parallel()
		err := WriteReader(context.Background(), "", strings.NewReader("x"), 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("mkdir_mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "deep", "file.txt")
		if err := WriteReader(context.Background(), path, strings.NewReader("nested"), 0o644, WithMkdirMode(0o755)); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "nested" {
			t.Errorf("got %q", got)
		}
	})
}

func TestPendingFile(t *testing.T) {
	t.Parallel()

	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "pending.txt")
		pf, err := NewPendingFile(path, 0o644)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer pf.Cleanup()
		if _, err := pf.Write([]byte("pending data")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := pf.CommitFile(); err != nil {
			t.Fatalf("CommitFile: %v", err)
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
		pf, err := NewPendingFile(path, 0o644)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		pf.Write([]byte("will be cleaned"))
		tmpName := pf.Name()
		pf.Cleanup()
		if _, err := os.Stat(tmpName); !os.IsNotExist(err) {
			t.Error("temp file not removed after Cleanup")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("final file should not exist after Cleanup")
		}
	})

	t.Run("cleanup_noop_after_commit", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "noop.txt")
		pf, err := NewPendingFile(path, 0o644)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		pf.Write([]byte("data"))
		pf.CommitFile()
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
		dir := t.TempDir()
		path := filepath.Join(dir, "mode.txt")
		pf, err := NewPendingFile(path, 0o600)
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer pf.Cleanup()
		pf.Write([]byte("secret"))
		if err := pf.CommitFile(); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
		}
	})

	t.Run("invalid_path", func(t *testing.T) {
		t.Parallel()
		_, err := NewPendingFile("relative/path", 0o644)
		if err == nil {
			t.Fatal("expected error for relative path")
		}
	})

	t.Run("mkdir_mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "pending.txt")
		pf, err := NewPendingFile(path, 0o644, WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer pf.Cleanup()
		pf.Write([]byte("nested"))
		if err := pf.CommitFile(); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "nested" {
			t.Errorf("got %q", got)
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
		os.WriteFile(path, []byte("old"), 0o755)
		if err := WriteFile(context.Background(), path, []byte("new"), WithPreserveMode()); err != nil {
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
		if err := WriteFile(context.Background(), path, []byte("data"), WithMode(0o600), WithPreserveMode()); err != nil {
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
		os.WriteFile(path, []byte("old"), 0o750)
		if err := WriteReader(context.Background(), path, strings.NewReader("new"), 0o644, WithPreserveMode()); err != nil {
			t.Fatalf("WriteReader: %v", err)
		}
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o750 {
			t.Errorf("mode = %o, want 0750 (preserved)", fi.Mode().Perm())
		}
	})

	t.Run("preserve_mode_with_SaveBytes", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "save.txt")
		os.WriteFile(path, []byte("old"), 0o750)
		if err := SaveBytes(path, []byte("new"), 0o644, WithPreserveMode()); err != nil {
			t.Fatalf("SaveBytes: %v", err)
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
		os.WriteFile(path, []byte("old"), 0o750)
		pf, err := NewPendingFile(path, 0o644, WithPreserveMode())
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer pf.Cleanup()
		pf.Write([]byte("new"))
		if err := pf.CommitFile(); err != nil {
			t.Fatalf("CommitFile: %v", err)
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
		if err := WriteFile(context.Background(), path, []byte("data"), WithPreserveOwnership()); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "data" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("preserves_ownership_as_root", func(t *testing.T) {
		if u, err := user.Current(); err != nil || u.Uid != "0" {
			t.Skip("requires root")
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "owned.txt")
		os.WriteFile(path, []byte("old"), 0o644)
		if err := WriteFile(context.Background(), path, []byte("new"), WithPreserveOwnership()); err != nil {
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
		if err := WriteFile(context.Background(), path, []byte("nested"), WithMkdirMode(0o755)); err != nil {
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
		err := WriteFile(context.Background(), path, []byte("data"))
		if err == nil {
			t.Fatal("expected error without MkdirMode")
		}
	})

	t.Run("Prepare_creates_dirs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "p", "q", "file.txt")
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("data"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		defer cleanup()
		got, _ := os.ReadFile(tmpPath)
		if string(got) != "data" {
			t.Errorf("got %q", got)
		}
	})
}

func TestLoadJSON(t *testing.T) {
	t.Parallel()

	t.Run("round_trip_with_SaveJSON", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "data.json")
		var mu sync.Mutex
		type cfg struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		}
		orig := cfg{Name: "test", Count: 42}
		if err := SaveJSON(path, &mu, orig, "test", 0o644); err != nil {
			t.Fatalf("SaveJSON: %v", err)
		}
		var loaded cfg
		if err := LoadJSON(context.Background(), path, 1<<20, &loaded); err != nil {
			t.Fatalf("LoadJSON: %v", err)
		}
		if loaded != orig {
			t.Errorf("got %+v, want %+v", loaded, orig)
		}
	})

	t.Run("rejects_oversized", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "big.json")
		os.WriteFile(path, []byte(`{"x": "long string padding here"}`), 0o644)
		var v map[string]string
		err := LoadJSON(context.Background(), path, 5, &v)
		if err == nil {
			t.Fatal("expected error for oversized file")
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "bad.json")
		os.WriteFile(path, []byte(`{not json`), 0o644)
		var v map[string]string
		err := LoadJSON(context.Background(), path, 1<<20, &v)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("missing_file", func(t *testing.T) {
		t.Parallel()
		var v map[string]string
		err := LoadJSON(context.Background(), "/nonexistent/path.json", 1<<20, &v)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

func TestSaveBytes_path_validation(t *testing.T) {
	t.Parallel()

	t.Run("rejects_relative_path", func(t *testing.T) {
		t.Parallel()
		err := SaveBytes("relative/path.txt", []byte("data"), 0o644)
		if err == nil {
			t.Fatal("expected error for relative path")
		}
	})

	t.Run("rejects_empty_path", func(t *testing.T) {
		t.Parallel()
		err := SaveBytes("", []byte("data"), 0o644)
		if err == nil {
			t.Fatal("expected error for empty path")
		}
	})

	t.Run("rejects_null_byte_no_temp_leak", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "foo\x00bar")
		err := SaveBytes(path, []byte("data"), 0o644)
		if err == nil {
			t.Fatal("expected error for null byte in path")
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("temp file leaked: %s", e.Name())
			}
		}
	})
}

func TestCommit_uses_opts_logger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "committed.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("data"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := Commit(tmpPath, path, WithLogger(logger)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "data" {
		t.Errorf("got %q, want %q", got, "data")
	}
}

func TestNoSync(t *testing.T) {
	t.Parallel()

	t.Run("WriteFile_nosync", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync.txt")
		if err := WriteFile(context.Background(), path, []byte("fast"), WithNoSync()); err != nil {
			t.Fatalf("WriteFile with NoSync: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "fast" {
			t.Errorf("got %q, want %q", got, "fast")
		}
	})

	t.Run("WriteReader_nosync", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync-reader.txt")
		if err := WriteReader(context.Background(), path, strings.NewReader("fast-stream"), 0o644, WithNoSync()); err != nil {
			t.Fatalf("WriteReader with NoSync: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "fast-stream" {
			t.Errorf("got %q, want %q", got, "fast-stream")
		}
	})

	t.Run("PendingFile_nosync", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync-pf.txt")
		pf, err := NewPendingFile(path, 0o644, WithNoSync())
		if err != nil {
			t.Fatalf("NewPendingFile: %v", err)
		}
		defer pf.Cleanup()
		pf.Write([]byte("fast-pending"))
		if err := pf.CommitFile(); err != nil {
			t.Fatalf("CommitFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "fast-pending" {
			t.Errorf("got %q, want %q", got, "fast-pending")
		}
	})

	t.Run("SaveBytes_nosync", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync-save.txt")
		if err := SaveBytes(path, []byte("fast-save"), 0o644, WithNoSync()); err != nil {
			t.Fatalf("SaveBytes with NoSync: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "fast-save" {
			t.Errorf("got %q, want %q", got, "fast-save")
		}
	})

	t.Run("Prepare_nosync", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "nosync-prepare.txt")
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("fast-prep"), WithNoSync())
		if err != nil {
			t.Fatalf("Prepare with NoSync: %v", err)
		}
		defer cleanup()
		got, _ := os.ReadFile(tmpPath)
		if string(got) != "fast-prep" {
			t.Errorf("got %q, want %q", got, "fast-prep")
		}
	})
}

func TestSymlinkTarget(t *testing.T) {
	t.Parallel()

	t.Run("refuses_symlink_target_by_default", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		os.WriteFile(real, []byte("original"), 0o644)
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)

		err := WriteFile(context.Background(), link, []byte("new"))
		if err == nil {
			t.Fatal("expected error for symlink target")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Errorf("error = %q, want symlink-related", err.Error())
		}
		got, _ := os.ReadFile(real)
		if string(got) != "original" {
			t.Errorf("original file modified: %q", got)
		}
	})

	t.Run("allows_symlink_target_with_option", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		os.WriteFile(real, []byte("original"), 0o644)
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)

		err := WriteFile(context.Background(), link, []byte("new"), WithAllowSymlinkTarget())
		if err != nil {
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
		err := WriteFile(context.Background(), path, []byte("data"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("WriteReader_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		os.WriteFile(real, []byte("x"), 0o644)
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)

		err := WriteReader(context.Background(), link, strings.NewReader("new"), 0o644)
		if err == nil {
			t.Fatal("expected error for symlink target")
		}
	})

	t.Run("SaveBytes_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		os.WriteFile(real, []byte("x"), 0o644)
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)

		err := SaveBytes(link, []byte("new"), 0o644)
		if err == nil {
			t.Fatal("expected error for symlink target")
		}
	})

	t.Run("PendingFile_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		os.WriteFile(real, []byte("x"), 0o644)
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)

		_, err := NewPendingFile(link, 0o644)
		if err == nil {
			t.Fatal("expected error for symlink target")
		}
	})

	t.Run("Prepare_refuses_symlink", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		real := filepath.Join(dir, "real.txt")
		os.WriteFile(real, []byte("x"), 0o644)
		link := filepath.Join(dir, "link.txt")
		os.Symlink(real, link)

		_, _, err := Prepare(context.Background(), link, []byte("new"))
		if err == nil {
			t.Fatal("expected error for symlink target")
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
		{"chown", "chown temp file", PhaseChown},
		{"dirsync", "fsync parent directory", PhaseDirSync},
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
	err := WriteFile(context.Background(), target, []byte("data"))
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
	want := "rename to final path: disk gone"
	if got != want {
		t.Errorf("WriteError.Error() = %q, want %q", got, want)
	}
}

func TestCommit_SymlinkFinalPath_RefusesAndCleansTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("orig"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	plain := filepath.Join(dir, "plain.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), plain, []byte("payload"), WithNoSync())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup()

	err = Commit(tmpPath, link, WithNoSync())
	if !errors.Is(err, ErrSymlinkTarget) {
		t.Fatalf("Commit(symlink finalPath) = %v, want ErrSymlinkTarget", err)
	}
	if _, statErr := os.Stat(tmpPath); !os.IsNotExist(statErr) {
		t.Errorf("temp file not cleaned after symlink refusal: stat err = %v", statErr)
	}
	got, _ := os.ReadFile(real)
	if string(got) != "orig" {
		t.Errorf("symlink target overwritten: got %q, want %q", got, "orig")
	}
}
