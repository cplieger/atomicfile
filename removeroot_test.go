package atomicfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cplieger/atomicfile/v2"
)

// pinRoot makes a temp dir, opens it as an *os.Root, and registers the close.
func pinRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("setup: os.OpenRoot(%q): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// TestOpenParentInRoot_pins_a_nested_parent pins the ordinary case: the returned root
// addresses the leaf's own directory, the returned base is the leaf, and operating on
// that pair reaches the same file the caller named.
func TestOpenParentInRoot_pins_a_nested_parent(t *testing.T) {
	t.Parallel()
	root, dir := pinRoot(t)
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatalf("setup: MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "leaf.pfx"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}

	parent, base, err := atomicfile.OpenParentInRoot(root, filepath.Join("a", "b", "leaf.pfx"))
	if err != nil {
		t.Fatalf("OpenParentInRoot(nested leaf) = %v, want nil", err)
	}
	defer parent.Close()

	if base != "leaf.pfx" {
		t.Errorf("base = %q, want %q", base, "leaf.pfx")
	}
	fi, statErr := parent.Lstat(base)
	if statErr != nil {
		t.Fatalf("pinned parent Lstat(%q) = %v, want the leaf", base, statErr)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("pinned parent Lstat(%q) mode = %s, want a regular file", base, fi.Mode())
	}
	// The pin addresses the leaf's directory, not the root: a sibling of the ROOT must
	// not be reachable by its own name through it.
	if _, wrong := parent.Lstat("a"); wrong == nil {
		t.Error("the pinned root resolved a name from the tree root, so it is not the leaf's own directory")
	}
}

// TestOpenParentInRoot_returns_a_closable_root_for_a_flat_name pins the API property that
// makes the close unconditional: even for a leaf sitting directly under root — where the
// pinned parent IS root's directory — the caller gets its OWN handle, so closing it can
// never close the root the caller still owns.
func TestOpenParentInRoot_returns_a_closable_root_for_a_flat_name(t *testing.T) {
	t.Parallel()
	root, dir := pinRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "flat.pfx"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}

	parent, base, err := atomicfile.OpenParentInRoot(root, "flat.pfx")
	if err != nil {
		t.Fatalf("OpenParentInRoot(flat name) = %v, want nil", err)
	}
	if base != "flat.pfx" {
		t.Errorf("base = %q, want %q", base, "flat.pfx")
	}
	if closeErr := parent.Close(); closeErr != nil {
		t.Errorf("closing the pinned parent = %v, want nil", closeErr)
	}
	// The caller's root must still work after that close.
	if _, statErr := root.Lstat("flat.pfx"); statErr != nil {
		t.Errorf("the caller's root stopped working after closing the pinned parent: %v", statErr)
	}
}

// TestOpenParentInRoot_refuses_a_redirected_component pins what the descent exists for.
// An os.Root FOLLOWS an in-root symlink component, so every one of these shapes would
// otherwise resolve — to a directory the caller never inspected — and the operation the
// caller then performs would land there.
func TestOpenParentInRoot_refuses_a_redirected_component(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		plant func(t *testing.T, dir string)
		name  string
	}{
		"a symlinked directory component": {
			name: filepath.Join("link", "leaf.pfx"),
			plant: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, "real"), 0o750); err != nil {
					t.Fatalf("setup: Mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "real", "leaf.pfx"), []byte("x"), 0o600); err != nil {
					t.Fatalf("setup: WriteFile: %v", err)
				}
				if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
					t.Fatalf("setup: Symlink: %v", err)
				}
			},
		},
		"a regular file in a directory position": {
			name: filepath.Join("notadir", "leaf.pfx"),
			plant: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "notadir"), []byte("x"), 0o600); err != nil {
					t.Fatalf("setup: WriteFile: %v", err)
				}
			},
		},
		"a fifo in a directory position": {
			name: filepath.Join("pipe", "leaf.pfx"),
			plant: func(t *testing.T, dir string) {
				t.Helper()
				if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
					t.Fatalf("setup: Mkfifo: %v", err)
				}
			},
		},
	}

	for title, tc := range cases {
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			root, dir := pinRoot(t)
			tc.plant(t, dir)

			parent, base, err := atomicfile.OpenParentInRoot(root, tc.name)
			if err == nil {
				_ = parent.Close()
				t.Fatalf("OpenParentInRoot(%q) = (%v, %q, nil), want a refusal: a component that is not the"+
					" real directory it looks like must never be pinned", tc.name, parent, base)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("OpenParentInRoot(%q) = %v, which matches fs.ErrNotExist: a refused component must"+
					" not read as the benign already-gone race", tc.name, err)
			}
		})
	}
}

// TestOpenParentInRoot_reports_a_missing_component_as_not_exist pins the classification
// callers branch on: a component that has vanished is the benign racing-deletion case and
// must be distinguishable from a refusal, or every transient race is reported to an
// operator as a misconfiguration.
func TestOpenParentInRoot_reports_a_missing_component_as_not_exist(t *testing.T) {
	t.Parallel()
	root, _ := pinRoot(t)

	if _, _, err := atomicfile.OpenParentInRoot(root, filepath.Join("gone", "leaf.pfx")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenParentInRoot(missing directory) = %v, want an error matching fs.ErrNotExist", err)
	}
}

// TestOpenParentInRoot_rejects_names_that_name_no_entry pins the argument contract. A
// name that cleans to "." or ".." has no final element to operate on, and answering with
// a root pinned to something plausible would invite a caller to remove a directory it
// never named.
func TestOpenParentInRoot_rejects_names_that_name_no_entry(t *testing.T) {
	t.Parallel()
	root, _ := pinRoot(t)

	for _, name := range []string{"", ".", "..", "sub/..", "/abs/leaf.pfx"} {
		if _, _, err := atomicfile.OpenParentInRoot(root, name); err == nil {
			t.Errorf("OpenParentInRoot(%q) = nil error, want a rejection", name)
		}
	}
	if _, _, err := atomicfile.OpenParentInRoot(nil, "leaf.pfx"); err == nil {
		t.Error("OpenParentInRoot(nil root) = nil error, want a rejection")
	}
}

// TestRemoveFileInRoot_removes_through_a_pinned_parent pins the whole point of the
// exported removal: the file the caller named goes away, and a same-named file reachable
// only through a swapped ancestor does not.
//
// The swap is the realistic one: the caller decided to remove old/x.pfx while old was a
// real directory, and by the time the unlink runs the name is a symlink to a live
// directory holding a file of the same name. A plain root.Remove would unlink the live
// file, because os.Root follows an in-root symlink component.
func TestRemoveFileInRoot_removes_through_a_pinned_parent(t *testing.T) {
	t.Parallel()

	t.Run("removes the named file", func(t *testing.T) {
		t.Parallel()
		root, dir := pinRoot(t)
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
			t.Fatalf("setup: MkdirAll: %v", err)
		}
		target := filepath.Join(dir, "sub", "leaf.pfx")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: WriteFile: %v", err)
		}

		if err := atomicfile.RemoveFileInRoot(root, filepath.Join("sub", "leaf.pfx")); err != nil {
			t.Fatalf("RemoveFileInRoot(nested regular file) = %v, want nil", err)
		}
		assertGone(t, target)
	})

	t.Run("refuses a swapped ancestor", func(t *testing.T) {
		t.Parallel()
		root, dir := pinRoot(t)
		for _, sub := range []string{"live", "old"} {
			if err := os.Mkdir(filepath.Join(dir, sub), 0o750); err != nil {
				t.Fatalf("setup: Mkdir(%s): %v", sub, err)
			}
			if err := os.WriteFile(filepath.Join(dir, sub, "x.pfx"), []byte("x"), 0o600); err != nil {
				t.Fatalf("setup: WriteFile(%s/x.pfx): %v", sub, err)
			}
		}
		if err := os.RemoveAll(filepath.Join(dir, "old")); err != nil {
			t.Fatalf("setup: RemoveAll(old): %v", err)
		}
		if err := os.Symlink("live", filepath.Join(dir, "old")); err != nil {
			t.Fatalf("setup: Symlink: %v", err)
		}

		if err := atomicfile.RemoveFileInRoot(root, filepath.Join("old", "x.pfx")); err == nil {
			t.Error("RemoveFileInRoot(swapped ancestor) = nil, want a refusal")
		}
		assertPresent(t, filepath.Join(dir, "live", "x.pfx"),
			"a file reachable only through a symlinked ancestor is not the file the caller named")
	})
}

// TestRemoveFileInRoot_refuses_a_non_regular_occupant pins that the name shape alone is
// not enough to unlink something: a caller sweeping names it believes it wrote must not be
// tricked into removing a directory, a symlink or a device node that took one of them.
func TestRemoveFileInRoot_refuses_a_non_regular_occupant(t *testing.T) {
	t.Parallel()
	root, dir := pinRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "dir.pfx"), 0o750); err != nil {
		t.Fatalf("setup: Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: WriteFile: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link.pfx")); err != nil {
		t.Fatalf("setup: Symlink: %v", err)
	}

	for _, name := range []string{"dir.pfx", "link.pfx"} {
		err := atomicfile.RemoveFileInRoot(root, name)
		if !errors.Is(err, atomicfile.ErrNotRegular) {
			t.Errorf("RemoveFileInRoot(%q) = %v, want ErrNotRegular", name, err)
		}
		assertPresent(t, filepath.Join(dir, name), "a non-regular occupant is left exactly as found")
	}
	// The symlink's target must survive too: refusing the link is not an excuse to
	// resolve it.
	assertPresent(t, filepath.Join(dir, "real"), "the symlink target was never named")
}

// TestRemoveFileInRoot_reports_a_missing_file pins that an already-gone name is reported
// rather than swallowed: whether that is success or a surprise is the caller's judgement,
// and the sweeps in this package rely on telling it apart from a real failure.
func TestRemoveFileInRoot_reports_a_missing_file(t *testing.T) {
	t.Parallel()
	root, _ := pinRoot(t)

	if err := atomicfile.RemoveFileInRoot(root, "absent.pfx"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("RemoveFileInRoot(absent name) = %v, want an error matching fs.ErrNotExist", err)
	}
}
