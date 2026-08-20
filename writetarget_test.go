package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A target already occupied by something that is not a regular file is refused
// before anything is staged. Two of the three shapes here were previously
// ACCEPTED or mis-reported:
//
//   - a FIFO, socket or device node was silently REPLACED by a regular file and
//     the write reported Durable, because rename(2) overwrites any non-directory.
//     A co-mounting writer could therefore get this package to destroy an object
//     it never created — the same shape RemoveFileInRoot and ReadBoundedInRoot
//     already refuse with ErrNotRegular.
//   - a directory failed, but only at PhaseRename, after a complete staged write
//     and both fsyncs, under a phase naming the wrong step.
func TestWriteTarget_RefusesANonRegularOccupant(t *testing.T) {
	t.Parallel()

	t.Run("fifo_is_refused_and_survives", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		fifo := filepath.Join(dir, "pipe")
		if err := syscall.Mkfifo(fifo, 0o644); err != nil {
			t.Skipf("mkfifo unsupported: %v", err)
		}
		_, err := WriteFile(t.Context(), fifo, []byte("payload"))
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("WriteFile(over a FIFO) = %v, want errors.Is ErrNotRegular", err)
		}
		fi, statErr := os.Lstat(fifo)
		if statErr != nil {
			t.Fatalf("Lstat(fifo) = %v; the FIFO must survive the refusal", statErr)
		}
		if fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("mode of the target = %s, want it to still be a FIFO", fi.Mode())
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("directory_is_refused_before_staging", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "adir")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("Mkdir() = %v", err)
		}
		_, err := WriteFile(t.Context(), target, []byte("payload"))
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("WriteFile(over a directory) = %v, want errors.Is ErrNotRegular", err)
		}
		var we *WriteError
		if errors.As(err, &we) {
			t.Errorf("error is *WriteError{Phase=%v}, want a pre-barrier refusal", we.Phase)
		}
		assertNoTempLeak(t, dir)
	})

	t.Run("symlink_keeps_its_own_sentinel", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "link")
		if err := os.Symlink(filepath.Join(dir, "elsewhere"), target); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		_, err := WriteFile(t.Context(), target, []byte("payload"))
		if !errors.Is(err, ErrSymlinkTarget) {
			t.Fatalf("WriteFile(over a symlink) = %v, want errors.Is ErrSymlinkTarget", err)
		}
		if errors.Is(err, ErrNotRegular) {
			t.Error("a symlink must keep ErrSymlinkTarget, not fold into ErrNotRegular")
		}
	})

	t.Run("existing_regular_file_is_still_overwritten", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		if _, err := WriteFile(t.Context(), target, []byte("new")); err != nil {
			t.Fatalf("WriteFile(over a regular file) = %v, want nil", err)
		}
		assertContent(t, target, "new")
	})

	t.Run("in_root", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
			t.Fatalf("Mkdir() = %v", err)
		}
		if _, err := WriteFileInRoot(t.Context(), root, "adir", []byte("x")); !errors.Is(err, ErrNotRegular) {
			t.Errorf("WriteFileInRoot(over a directory) = %v, want errors.Is ErrNotRegular", err)
		}
		if _, err := NewPendingFileInRoot(t.Context(), root, "adir"); !errors.Is(err, ErrNotRegular) {
			t.Errorf("NewPendingFileInRoot(over a directory) = %v, want errors.Is ErrNotRegular", err)
		}
		assertNoTempLeak(t, dir)
	})
}

// A name that cleans to something naming no ENTRY is refused up front rather than
// at the rename. Before the shared check, WriteFileInRoot(root, "sub/..") staged a
// temp, wrote it, chmod'ed it, fsynced it and closed it before failing at
// PhaseRename with "file exists", while OpenParentInRoot refused the identical
// name with ErrUnsafePath before touching the filesystem.
func TestWriteTarget_RefusesANameThatNamesNoEntry(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".", "..", "sub/..", "a/b/../.."} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, dir := openTestRoot(t)
			_, err := WriteFileInRoot(t.Context(), root, name, []byte("x"))
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("WriteFileInRoot(%q) = %v, want errors.Is ErrUnsafePath", name, err)
			}
			if !strings.Contains(err.Error(), "names no entry") {
				t.Errorf("error = %q, want it to say the name holds no entry", err)
			}
			assertNoTempLeak(t, dir)
		})
	}

	// The absolute-path family reaches the same check through the base name.
	t.Run("root_of_the_filesystem", func(t *testing.T) {
		t.Parallel()
		if _, err := WriteFile(t.Context(), string(filepath.Separator), []byte("x")); !errors.Is(err, ErrUnsafePath) {
			t.Errorf(`WriteFile("/") = %v, want errors.Is ErrUnsafePath`, err)
		}
	})
}

func TestValidateRootEntry(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		name      string
		wantClean string
		wantErr   error
	}{
		"plain":            {name: "f.txt", wantClean: "f.txt"},
		"nested":           {name: "a/b/f.txt", wantClean: filepath.Join("a", "b", "f.txt")},
		"inner_dotdot":     {name: "a/../b.txt", wantClean: "b.txt"},
		"escaping_dotdot":  {name: "../escape", wantClean: filepath.Join("..", "escape")},
		"empty":            {name: "", wantErr: ErrEmptyPath},
		"null_byte":        {name: "a\x00b", wantErr: ErrUnsafePath},
		"absolute":         {name: "/etc/passwd", wantErr: ErrUnsafePath},
		"dot":              {name: ".", wantErr: ErrUnsafePath},
		"dotdot":           {name: "..", wantErr: ErrUnsafePath},
		"cleans_to_dot":    {name: "a/..", wantErr: ErrUnsafePath},
		"cleans_to_dotdot": {name: "a/../..", wantErr: ErrUnsafePath},
	}
	for label, tc := range tests {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			clean, err := validateRootEntry(tc.name)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("validateRootEntry(%q) = (%q, %v), want errors.Is %v", tc.name, clean, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateRootEntry(%q) = error %v, want nil", tc.name, err)
			}
			if clean != tc.wantClean {
				t.Errorf("validateRootEntry(%q) = %q, want %q", tc.name, clean, tc.wantClean)
			}
		})
	}

	// "." stays acceptable to validateRootName, because it is a legitimate
	// DIRECTORY name inside a root and ProbeWritableInRoot documents taking it.
	// Only the entry-shaped operations refuse it.
	t.Run("validateRootName_still_accepts_dot", func(t *testing.T) {
		t.Parallel()
		if clean, err := validateRootName("."); err != nil || clean != "." {
			t.Errorf(`validateRootName(".") = (%q, %v), want (".", nil)`, clean, err)
		}
	})
}
