package atomicfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v3"
)

// openTestRoot opens dir as an *os.Root and closes it when the test ends.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%q) = %v, want nil", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

// writeTestFile writes n bytes at dir/name and returns the full path.
func writeTestFile(t *testing.T, dir, name string, n int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatalf("write %q = %v, want nil", path, err)
	}
	return path
}

// capCases are the three read-bound outcomes both open primitives must produce
// once the descriptor they hand back is read through ReadBoundedFile: a file
// under the cap and one landing exactly on it read whole, one byte past it is
// refused with ErrFileTooLarge rather than silently truncated.
var capCases = []struct {
	name    string
	size    int
	maxByte int64
	wantErr error
}{
	{"under_the_cap", 512, 1024, nil},
	{"exactly_at_the_cap", 1024, 1024, nil},
	{"one_byte_over_the_cap", 1025, 1024, atomicfile.ErrFileTooLarge},
}

func TestOpenRegularInRoot(t *testing.T) {
	t.Parallel()

	t.Run("hands_back_a_readable_descriptor_and_its_own_stat", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "in.pem"), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		root := openTestRoot(t, dir)

		f, fi, err := atomicfile.OpenRegularInRoot(root, "in.pem")
		if err != nil {
			t.Fatalf("OpenRegularInRoot = %v, want nil", err)
		}
		defer f.Close()

		if !fi.Mode().IsRegular() {
			t.Errorf("FileInfo mode = %s, want a regular file", fi.Mode())
		}
		if fi.Size() != int64(len("payload")) {
			t.Errorf("FileInfo size = %d, want %d", fi.Size(), len("payload"))
		}
		got, err := atomicfile.ReadBoundedFile(t.Context(), f, 1024)
		if err != nil {
			t.Fatalf("ReadBoundedFile = %v, want nil", err)
		}
		if string(got) != "payload" {
			t.Errorf("read %q, want %q", got, "payload")
		}
	})

	t.Run("the_stat_describes_the_descriptors_own_inode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeTestFile(t, dir, "snapshot.json", 16)
		root := openTestRoot(t, dir)

		f, fi, err := atomicfile.OpenRegularInRoot(root, "snapshot.json")
		if err != nil {
			t.Fatalf("OpenRegularInRoot = %v, want nil", err)
		}
		defer f.Close()

		// The point of returning the FileInfo: it is the reload-staleness
		// observation, taken from the same descriptor the bytes come from, so a
		// caller never has to stat the pathname a second time to record it.
		id := atomicfile.Identify(fi)
		if !id.Recorded() {
			t.Fatal("Identify(fi).Recorded() = false, want true")
		}
		onDisk, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !id.Matches(onDisk) {
			t.Error("Identify(fi) does not match a fresh stat of the same untouched file")
		}
	})

	for _, tc := range capCases {
		t.Run("bounds_the_read_"+tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeTestFile(t, dir, "feed.json", tc.size)
			root := openTestRoot(t, dir)

			f, _, err := atomicfile.OpenRegularInRoot(root, "feed.json")
			if err != nil {
				t.Fatalf("OpenRegularInRoot = %v, want nil", err)
			}
			defer f.Close()

			data, err := atomicfile.ReadBoundedFile(t.Context(), f, tc.maxByte)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("ReadBoundedFile(%d bytes, max %d) = %v, want %v", tc.size, tc.maxByte, err, tc.wantErr)
				}
			case err != nil:
				t.Errorf("ReadBoundedFile(%d bytes, max %d) = %v, want nil", tc.size, tc.maxByte, err)
			case len(data) != tc.size:
				t.Errorf("read %d bytes, want %d", len(data), tc.size)
			}
		})
	}

	t.Run("read_bounded_in_root_is_this_composition", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "in.pem"), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		root := openTestRoot(t, dir)

		f, _, err := atomicfile.OpenRegularInRoot(root, "in.pem")
		if err != nil {
			t.Fatalf("OpenRegularInRoot = %v, want nil", err)
		}
		defer f.Close()
		composed, err := atomicfile.ReadBoundedFile(t.Context(), f, 1024)
		if err != nil {
			t.Fatalf("ReadBoundedFile = %v, want nil", err)
		}
		whole, err := atomicfile.ReadBoundedInRoot(t.Context(), root, "in.pem", 1024)
		if err != nil {
			t.Fatalf("ReadBoundedInRoot = %v, want nil", err)
		}
		if string(whole) != string(composed) {
			t.Errorf("ReadBoundedInRoot read %q, the open+read composition read %q", whole, composed)
		}
	})

	t.Run("missing_file_matches_fs_ErrNotExist", func(t *testing.T) {
		t.Parallel()
		root := openTestRoot(t, t.TempDir())

		f, fi, err := atomicfile.OpenRegularInRoot(root, "absent.json")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenRegularInRoot(missing) = %v, want fs.ErrNotExist", err)
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("escaping_symlink_is_refused", func(t *testing.T) {
		t.Parallel()
		dir, outside := t.TempDir(), t.TempDir()
		secret := filepath.Join(outside, "secret.pem")
		if err := os.WriteFile(secret, []byte("do not leak"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(dir, "leak.pem")); err != nil {
			t.Fatal(err)
		}
		root := openTestRoot(t, dir)

		f, fi, err := atomicfile.OpenRegularInRoot(root, "leak.pem")
		if err == nil {
			_ = f.Close()
			t.Fatal("OpenRegularInRoot(escaping symlink) = nil error, want a confinement refusal")
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("parent_traversal_is_refused", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "secret.pem"), []byte("nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(parent, "inner")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		root := openTestRoot(t, dir)

		f, fi, err := atomicfile.OpenRegularInRoot(root, "../secret.pem")
		if err == nil {
			_ = f.Close()
			t.Fatal("OpenRegularInRoot(traversal) = nil error, want a refusal")
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("fifo_is_refused_without_blocking", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := syscall.Mkfifo(filepath.Join(dir, "pipe.pem"), 0o600); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}
		root := openTestRoot(t, dir)

		assertRefusesFIFO(t, func() (*os.File, os.FileInfo, error) {
			return atomicfile.OpenRegularInRoot(root, "pipe.pem")
		})
	})

	t.Run("directory_is_refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
			t.Fatal(err)
		}
		root := openTestRoot(t, dir)

		f, fi, err := atomicfile.OpenRegularInRoot(root, "sub")
		if !errors.Is(err, atomicfile.ErrNotRegular) {
			t.Errorf("OpenRegularInRoot(directory) = %v, want ErrNotRegular", err)
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("empty_name_and_nil_root_are_refused", func(t *testing.T) {
		t.Parallel()
		root := openTestRoot(t, t.TempDir())

		if _, _, err := atomicfile.OpenRegularInRoot(root, ""); !errors.Is(err, atomicfile.ErrEmptyPath) {
			t.Errorf("empty name = %v, want ErrEmptyPath", err)
		}
		if _, _, err := atomicfile.OpenRegularInRoot(nil, "x"); err == nil {
			t.Error("nil root = nil error, want a rejection")
		}
	})
}

func TestOpenRegular(t *testing.T) {
	t.Parallel()

	t.Run("hands_back_a_readable_descriptor_and_its_own_stat", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}

		f, fi, err := atomicfile.OpenRegular(path)
		if err != nil {
			t.Fatalf("OpenRegular = %v, want nil", err)
		}
		defer f.Close()

		if !fi.Mode().IsRegular() {
			t.Errorf("FileInfo mode = %s, want a regular file", fi.Mode())
		}
		got, err := atomicfile.ReadBoundedFile(t.Context(), f, 1024)
		if err != nil {
			t.Fatalf("ReadBoundedFile = %v, want nil", err)
		}
		if string(got) != "payload" {
			t.Errorf("read %q, want %q", got, "payload")
		}
	})

	for _, tc := range capCases {
		t.Run("bounds_the_read_"+tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTestFile(t, t.TempDir(), "feed.json", tc.size)

			f, _, err := atomicfile.OpenRegular(path)
			if err != nil {
				t.Fatalf("OpenRegular = %v, want nil", err)
			}
			defer f.Close()

			data, err := atomicfile.ReadBoundedFile(t.Context(), f, tc.maxByte)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("ReadBoundedFile(%d bytes, max %d) = %v, want %v", tc.size, tc.maxByte, err, tc.wantErr)
				}
			case err != nil:
				t.Errorf("ReadBoundedFile(%d bytes, max %d) = %v, want nil", tc.size, tc.maxByte, err)
			case len(data) != tc.size:
				t.Errorf("read %d bytes, want %d", len(data), tc.size)
			}
		})
	}

	t.Run("missing_file_matches_fs_ErrNotExist", func(t *testing.T) {
		t.Parallel()

		f, fi, err := atomicfile.OpenRegular(filepath.Join(t.TempDir(), "absent.json"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenRegular(missing) = %v, want fs.ErrNotExist", err)
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("final_component_symlink_is_refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		f, fi, err := atomicfile.OpenRegular(link)
		if !errors.Is(err, atomicfile.ErrSymlinkTarget) {
			t.Errorf("OpenRegular(symlink) = %v, want ErrSymlinkTarget", err)
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("symlink_out_of_the_directory_is_refused_unread", func(t *testing.T) {
		t.Parallel()
		dir, outside := t.TempDir(), t.TempDir()
		secret := filepath.Join(outside, "secret.pem")
		if err := os.WriteFile(secret, []byte("do not leak"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "leak.pem")
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}

		f, fi, err := atomicfile.OpenRegular(link)
		if !errors.Is(err, atomicfile.ErrSymlinkTarget) {
			t.Errorf("OpenRegular(escaping symlink) = %v, want ErrSymlinkTarget", err)
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("read_bounded_still_follows_symlinks", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "real.json")
		if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		// OpenRegular refusing a symlink must not have changed ReadBounded,
		// whose documented contract is that os.Open resolves the link.
		got, err := atomicfile.ReadBounded(t.Context(), link, 1024)
		if err != nil {
			t.Fatalf("ReadBounded(symlink) = %v, want nil", err)
		}
		if string(got) != "real" {
			t.Errorf("ReadBounded(symlink) read %q, want %q", got, "real")
		}
	})

	t.Run("fifo_is_refused_without_blocking", func(t *testing.T) {
		t.Parallel()
		fifo := filepath.Join(t.TempDir(), "pipe.pem")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}

		assertRefusesFIFO(t, func() (*os.File, os.FileInfo, error) {
			return atomicfile.OpenRegular(fifo)
		})
	})

	t.Run("directory_is_refused", func(t *testing.T) {
		t.Parallel()

		f, fi, err := atomicfile.OpenRegular(t.TempDir())
		if !errors.Is(err, atomicfile.ErrNotRegular) {
			t.Errorf("OpenRegular(directory) = %v, want ErrNotRegular", err)
		}
		assertNoHandle(t, f, fi)
	})

	t.Run("applies_the_validate_path_rule", func(t *testing.T) {
		t.Parallel()
		tests := map[string]struct {
			path string
			want error
		}{
			"empty":        {"", atomicfile.ErrEmptyPath},
			"relative":     {"state.json", atomicfile.ErrUnsafePath},
			"null_byte":    {"/tmp/a\x00b", atomicfile.ErrUnsafePath},
			"dot_relative": {"./state.json", atomicfile.ErrUnsafePath},
		}
		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f, fi, err := atomicfile.OpenRegular(tc.path)
				if !errors.Is(err, tc.want) {
					t.Errorf("OpenRegular(%q) = %v, want %v", tc.path, err, tc.want)
				}
				assertNoHandle(t, f, fi)
			})
		}
	})
}

// assertNoHandle fails t unless a refused open handed back neither a descriptor
// (which the caller could never close, since it has no reason to believe one
// exists) nor a FileInfo.
func assertNoHandle(t *testing.T, f *os.File, fi os.FileInfo) {
	t.Helper()
	if f != nil {
		_ = f.Close()
		t.Errorf("a refused open returned a descriptor (%q), want nil", f.Name())
	}
	if fi != nil {
		t.Errorf("a refused open returned a FileInfo (%v), want nil", fi.Mode())
	}
}

// assertRefusesFIFO fails t unless open refuses a named pipe with ErrNotRegular
// promptly. The prompt part is the point of O_NONBLOCK: open(2) on a FIFO with
// no writer blocks indefinitely, so a caller with one goroutine would hang
// instead of getting an error it can act on.
func assertRefusesFIFO(t *testing.T, open func() (*os.File, os.FileInfo, error)) {
	t.Helper()
	type result struct {
		f   *os.File
		fi  os.FileInfo
		err error
	}
	done := make(chan result, 1)
	go func() {
		f, fi, err := open()
		done <- result{f, fi, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, atomicfile.ErrNotRegular) {
			t.Errorf("open(fifo) = %v, want ErrNotRegular", got.err)
		}
		assertNoHandle(t, got.f, got.fi)
	case <-time.After(3 * time.Second):
		t.Fatal("open blocked on a FIFO; the open must be non-blocking")
	}
}
