package atomicfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

	t.Run("accepts_literal_dots_in_path_segment", func(t *testing.T) {
		t.Parallel()
		for _, in := range []string{"/tmp/foo../bar", "/tmp/.../file"} {
			got, err := validateAbsClean(in)
			if err != nil {
				t.Fatalf("validateAbsClean(%q) = %v, want nil (literal dots are not traversal)", in, err)
			}
			if got != filepath.Clean(in) {
				t.Fatalf("validateAbsClean(%q) = %q, want %q", in, got, filepath.Clean(in))
			}
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

// TestValidatePath covers the exported gate: the same verdicts as the private
// validator behind it (absolute accepted, relative / empty / NUL rejected with
// the sentinel the write path returns), plus the delegation itself — every case
// asserts that ValidatePath and validateAbsClean agree, so the two can never
// become two rules.
func TestValidatePath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		wantErr error
		name    string
		path    string
	}{
		{name: "accepts_absolute_path", path: "/tmp/test.txt"},
		{name: "accepts_absolute_path_needing_clean", path: "/tmp//sub/./test.txt"},
		// Clean normalizes ".." in an absolute path: accepted, and documented
		// as not being a containment boundary.
		{name: "accepts_absolute_path_with_traversal", path: "/foo/../etc/passwd"},
		{name: "rejects_relative_path", path: "relative/path", wantErr: ErrUnsafePath},
		{name: "rejects_bare_relative_name", path: "file.txt", wantErr: ErrUnsafePath},
		{name: "rejects_empty_path", path: "", wantErr: ErrEmptyPath},
		{name: "rejects_null_byte", path: "/tmp/foo\x00bar", wantErr: ErrUnsafePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ValidatePath(tc.path)

			// One rule, one implementation: the exported gate must return
			// exactly what the write path's validator returns.
			_, want := validateAbsClean(tc.path)
			switch {
			case (got == nil) != (want == nil):
				t.Fatalf("ValidatePath(%q) = %v but validateAbsClean(%q) = %v; the exported gate must be the same rule the write path applies",
					tc.path, got, tc.path, want)
			case got != nil && got.Error() != want.Error():
				t.Errorf("ValidatePath(%q) = %q, want the validator's own %q", tc.path, got, want)
			}

			if tc.wantErr == nil {
				if got != nil {
					t.Errorf("ValidatePath(%q) = %v, want nil", tc.path, got)
				}
				return
			}
			if !errors.Is(got, tc.wantErr) {
				t.Errorf("ValidatePath(%q) = %v, want %v", tc.path, got, tc.wantErr)
			}
		})
	}
}

// TestValidatePathPerformsNoFilesystemIO pins the property that separates
// ValidatePath from ProbeWritable: the gate inspects the string and consults
// nothing on disk. Every path below is one an implementation that stat-ed,
// opened, or staged a file could not accept — a name that does not exist, a
// name whose parent is a regular file (any open of it is ENOTDIR), a name under
// a directory that does not exist, a directory, a FIFO (a blocking open never
// returns), and an existing file a create-and-remove probe would have
// truncated. The directory is snapshotted around the batch, and the seeded
// file's content and identity re-checked, so a staged temp or a touched file
// would surface even if the verdicts stayed nil.
func TestValidatePathPerformsNoFilesystemIO(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	seeded := filepath.Join(dir, "seeded.txt")
	if err := os.WriteFile(seeded, []byte("untouched"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info, err := os.Stat(seeded)
	if err != nil {
		t.Fatalf("stat seed: %v", err)
	}
	id := Identify(info)

	paths := []string{
		filepath.Join(dir, "does-not-exist.txt"),
		filepath.Join(seeded, "parent-is-a-regular-file.txt"),
		filepath.Join(dir, "no-such-dir", "deep", "name.txt"),
		dir,
		seeded,
	}
	fifo := filepath.Join(dir, "pipe")
	if mkErr := syscall.Mkfifo(fifo, 0o600); mkErr != nil {
		t.Logf("mkfifo unsupported here (%v); the blocking-open case is skipped", mkErr)
	} else {
		paths = append(paths, fifo)
	}

	before := dirEntryNames(t, dir)

	// An implementation that opened the FIFO would block until a writer
	// arrived, so the batch runs off the test goroutine: a hang becomes a
	// failure here instead of a wedged package.
	errs := make([]error, len(paths))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i, path := range paths {
			errs[i] = ValidatePath(path)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ValidatePath did not return within 10s; it must not open the path (a FIFO open blocks until a writer arrives)")
	}
	for i, path := range paths {
		if errs[i] != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil (the gate reads the string, not the filesystem)", path, errs[i])
		}
	}

	if after := dirEntryNames(t, dir); !slices.Equal(before, after) {
		t.Errorf("directory entries changed: before %v, after %v", before, after)
	}
	assertNoTempLeak(t, dir)
	assertContent(t, seeded, "untouched")
	nowInfo, err := os.Stat(seeded)
	if err != nil {
		t.Fatalf("re-stat seed: %v", err)
	}
	if !id.Matches(nowInfo) {
		t.Error("seeded file identity changed (mtime or inode); ValidatePath must not touch it")
	}
}

// dirEntryNames returns dir's entry names in ReadDir order, for a before/after
// comparison that fails on anything created or removed.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v, want nil", dir, err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
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
