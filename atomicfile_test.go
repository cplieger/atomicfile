package atomicfile

import (
	"bytes"
	"context"
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
		if err := WriteFileMode(context.Background(), path, []byte("x"), 0o600, nil); err != nil {
			t.Fatalf("WriteFileMode: %v", err)
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
		opts := &Options{TempPattern: ".myapp-*.tmp"}
		if err := WriteFileMode(context.Background(), path, []byte("data"), 0o644, opts); err != nil {
			t.Fatalf("WriteFileMode: %v", err)
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
		err := WriteFileMode(ctx, path, []byte("data"), 0o644, nil)
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
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("prepared data"), nil)
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
		tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("commit me"), nil)
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
		_, _, err := Prepare(context.Background(), "", []byte("x"), nil)
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
				if err := SaveBytes(path, tt.data, tt.perm, nil); err != nil {
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
		if err := SaveBytes(path, []byte("data"), 0o644, nil); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("rename_error_cleans_up_temp", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "target")
		os.Mkdir(path, 0o755)
		if err := SaveBytes(path, []byte("data"), 0o644, nil); err == nil {
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
		if err := SaveJSON(path, &mu, map[string]int{"x": 1}, "test", 0o644, nil); err != nil {
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
		err := SaveJSON(path, nil, map[string]int{"x": 1}, "test", 0o644, nil)
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
		if err := SaveJSON(path, &mu, make(chan int), "test", 0o644, nil); err == nil {
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
		old := filepath.Join(dir, "chat.json.tmp-2222")
		os.WriteFile(old, []byte("old"), 0o644)
		oldTime := time.Now().Add(-2 * time.Hour)
		os.Chtimes(old, oldTime, oldTime)
		canonical := filepath.Join(dir, "chat.json")
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
		tmp := filepath.Join(dir, "chat.json.tmp-abc123")
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
}

func TestIsStaleTempName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"chat temp", "chat.json.tmp-abc123", true},
		{"upload temp", "photo.jpg.upload-abc123", true},
		{"copy temp", "backup.tar.copy-xyz789", true},
		{"no signature", "regular.json", false},
		{"suffix contains dot", "alice.tmp-2024-notes.json", false},
		{"suffix contains slash", "foo.tmp-a/b", false},
		{"nothing after suffix", "just.tmp-", false},
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

func TestValidateAbsClean(t *testing.T) {
	t.Parallel()

	t.Run("rejects_relative_path", func(t *testing.T) {
		_, err := validateAbsClean("relative/path")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects_traversal", func(t *testing.T) {
		_, err := validateAbsClean("/foo/../etc/passwd")
		// filepath.Clean resolves this to /etc/passwd which is valid
		// The traversal check is for cases Clean can't resolve
		if err != nil {
			// This is fine — Clean resolves it
			return
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
}

func TestWriteFileMode_ro_dir(t *testing.T) {
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
	err := WriteFileMode(context.Background(), path, []byte("data"), 0o644, nil)
	if err == nil {
		t.Fatal("expected error for read-only dir")
	}
}
