package atomicfile

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The durability property WithMkdirMode used to break: a directory ENTRY that
// has never been fsynced into its own parent can vanish in a crash, taking the
// whole subtree with it — including a file the write just fsynced and renamed.
// So every level the write CREATES must have its parent fsynced, not only the
// file's immediate parent.
//
// Serial (no t.Parallel): the fsyncRootDir seam is package state.
func TestMkdirMode_FsyncsEveryCreatedDirectoryParent(t *testing.T) {
	t.Run("WriteFile", func(t *testing.T) {
		recorded := recordFsyncRootDir(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "c", "f.txt")
		res, err := WriteFile(t.Context(), path, []byte("payload"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true")
		}
		assertContent(t, path, "payload")
		// The mkdir chain runs inside a root on the deepest EXISTING ancestor
		// (dir), so its fsync targets are relative to that root: "." for a's
		// parent, "a" for b's, "a/b" for c's. The commit-side fsync then runs
		// inside a root on the file's own parent, where the target is ".".
		assertFsynced(t, recorded(), []string{".", "a", "a/b", "."})
	})

	t.Run("WriteFileInRoot", func(t *testing.T) {
		recorded := recordFsyncRootDir(t)
		root, dir := openTestRoot(t)
		res, err := WriteFileInRoot(t.Context(), root, "a/b/c/f.txt", []byte("payload"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("WriteFileInRoot() = %v", err)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true")
		}
		assertContent(t, filepath.Join(dir, "a", "b", "c", "f.txt"), "payload")
		// One root for the whole write, so every target is root-relative and the
		// commit-side fsync of the file's parent is the last one.
		assertFsynced(t, recorded(), []string{".", "a", "a/b", "a/b/c"})
	})

	t.Run("NewPendingFile", func(t *testing.T) {
		recorded := recordFsyncRootDir(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "f.txt")
		pf, err := NewPendingFile(t.Context(), path, WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("NewPendingFile() = %v", err)
		}
		if _, err := pf.WriteString("payload"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		res, err := pf.Commit(t.Context())
		if err != nil {
			t.Fatalf("Commit() = %v", err)
		}
		if !res.Durable {
			t.Errorf("Result.Durable = false, want true")
		}
		assertFsynced(t, recorded(), []string{".", "a", "."})
	})

	// A write into a directory that already exists creates nothing, so the only
	// fsync is the commit-side one. This is the control: it proves the extra
	// syncs above come from the creation and are not unconditional.
	t.Run("existing_parent_syncs_once", func(t *testing.T) {
		recorded := recordFsyncRootDir(t)
		root, _ := openTestRoot(t)
		if err := root.Mkdir("a", 0o755); err != nil {
			t.Fatalf("Mkdir() = %v", err)
		}
		if _, err := WriteFileInRoot(t.Context(), root, "a/f.txt", []byte("x"), WithMkdirMode(0o755)); err != nil {
			t.Fatalf("WriteFileInRoot() = %v", err)
		}
		assertFsynced(t, recorded(), []string{"a"})
	})
}

// assertFsynced fails t unless the recorded fsync targets are exactly want, in
// order. The order is part of the property: a level's parent is synced as that
// level is created, so the sequence reads outermost-first.
func assertFsynced(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("fsynced dirs = %v, want %v", got, want)
	}
}

// A created directory whose parent cannot be fsynced degrades Result.Durable
// instead of failing the write, matching what the commit-side barrier does with
// the identical failure. Hard-failing here would make every WithMkdirMode write
// an error on a filesystem that refuses to fsync a directory while the same
// write into an existing directory kept succeeding.
func TestMkdirMode_CreatedDirFsyncFailure_ReportsNotDurable(t *testing.T) {
	sentinel := errors.New("injected dir fsync failure")

	t.Run("WriteFile", func(t *testing.T) {
		stubFsyncRootDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "f.txt")
		res, err := WriteFile(t.Context(), path, []byte("payload"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("WriteFile() = %v, want nil error", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false after a created-dir fsync failure")
		}
		assertContent(t, path, "payload")
	})

	t.Run("WriteFileInRoot", func(t *testing.T) {
		stubFsyncRootDir(t, sentinel)
		root, dir := openTestRoot(t)
		res, err := WriteFileInRoot(t.Context(), root, "a/b/f.txt", []byte("payload"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("WriteFileInRoot() = %v, want nil error", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false after a created-dir fsync failure")
		}
		assertContent(t, filepath.Join(dir, "a", "b", "f.txt"), "payload")
	})

	// The PendingFile carries the verdict from construction to Commit, which is
	// the only place it can be reported: the mkdir happens in NewPendingFile and
	// Result is assembled a caller's worth of writes later.
	t.Run("PendingFile", func(t *testing.T) {
		stubFsyncRootDir(t, sentinel)
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "f.txt")
		pf, err := NewPendingFile(t.Context(), path, WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("NewPendingFile() = %v", err)
		}
		if _, err := pf.WriteString("payload"); err != nil {
			t.Fatalf("WriteString() = %v", err)
		}
		res, err := pf.Commit(t.Context())
		if err != nil {
			t.Fatalf("Commit() = %v, want nil error", err)
		}
		if res.Durable {
			t.Errorf("Result.Durable = true, want false after a created-dir fsync failure")
		}
		assertContent(t, path, "payload")
	})

	// A created-dir fsync failure must be visible to an operator, like the
	// commit-side one.
	t.Run("logs_warn", func(t *testing.T) {
		stubFsyncRootDir(t, sentinel)
		root, _ := openTestRoot(t)
		logger, logged := captureWarn()
		if _, err := WriteFileInRoot(t.Context(), root, "a/f.txt", []byte("x"),
			WithMkdirMode(0o755), WithLogger(logger)); err != nil {
			t.Fatalf("WriteFileInRoot() = %v", err)
		}
		if !strings.Contains(logged(), "fsync of a created directory's parent failed") {
			t.Errorf("logs = %q, want the created-directory fsync warning", logged())
		}
	})
}

// WithMkdirMode's mode is ENFORCED on the directories the write creates, not
// merely requested. mkdir(2) puts the mode through umask, so under a tight umask
// a requested 0o755 lands narrower — which would make the option a suggestion in
// the one package that spent a primitive (EnforceMode) establishing that a mode
// argument must not be.
//
// Serial (no t.Parallel): umask is process state.
func TestMkdirMode_EnforcesTheRequestedMode(t *testing.T) {
	if isWindows() {
		t.Skip("POSIX mode bits")
	}
	withUmask(t, 0o077)
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "f.txt")
	if _, err := WriteFile(t.Context(), path, []byte("x"), WithMkdirMode(0o755)); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	// Both created levels, not only the leaf: an ancestor left at 0o700 under a
	// 0o755 request is the same defect one level up.
	assertDirMode(t, filepath.Join(dir, "a"), 0o755)
	assertDirMode(t, filepath.Join(dir, "a", "b"), 0o755)
}

// A PRE-EXISTING directory is never chmod'ed, matching EnsurePrivateDir's rule:
// the mode enforcement is licensed by mkdir(2) having just handed this process a
// name nobody else ever held, and a directory somebody else made is theirs.
func TestMkdirMode_LeavesAPreExistingDirectoryAlone(t *testing.T) {
	if isWindows() {
		t.Skip("POSIX mode bits")
	}
	root, dir := openTestRoot(t)
	if err := root.Mkdir("a", 0o700); err != nil {
		t.Fatalf("Mkdir() = %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "a"), 0o701); err != nil {
		t.Fatalf("Chmod() = %v", err)
	}
	if _, err := WriteFileInRoot(t.Context(), root, "a/b/f.txt", []byte("x"), WithMkdirMode(0o750)); err != nil {
		t.Fatalf("WriteFileInRoot() = %v", err)
	}
	assertDirMode(t, filepath.Join(dir, "a"), 0o701)      // untouched
	assertDirMode(t, filepath.Join(dir, "a", "b"), 0o750) // created, enforced
}

// A non-directory occupying a level of the chain reports ENOTDIR, the error
// os.MkdirAll gives for the same shape, so a caller's errors.Is does not change
// with the new implementation.
//
// Reached by calling mkdirAllInRoot directly, because the write entry points
// cannot get here: the guard sequence Lstats the TARGET before the mkdir runs,
// and that Lstat traverses the same blocked component and fails first (with the
// same ENOTDIR — TestMkdirMode_BlockedByFile pins that path). The arm exists for
// the race where the blocker appears between the two.
func TestMkdirAllInRoot_NonDirectoryInChain(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("i am a file"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	durable, err := mkdirAllInRoot(root, filepath.Join("a", "b"), 0o755, slog.Default())
	if err == nil {
		t.Fatal("mkdirAllInRoot(file in the chain) = nil, want an error")
	}
	if durable {
		t.Error("durable = true on a failed chain, want false")
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("error = %v, want errors.Is syscall.ENOTDIR", err)
	}
}

// A level the walk found MISSING that is then occupied by an escaping symlink is
// refused by the root rather than followed. This is the confinement mkdirAllAbs
// adds over os.MkdirAll, and it is exercised directly because arranging the race
// through a write entry point is not deterministic.
func TestMkdirAllInRoot_RefusesAnEscapingSymlinkInTheChain(t *testing.T) {
	t.Parallel()
	root, dir := openTestRoot(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "a")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := mkdirAllInRoot(root, filepath.Join("a", "b"), 0o755, slog.Default()); err == nil {
		t.Fatal("mkdirAllInRoot(through an escaping symlink) = nil, want a refusal")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "b")); statErr == nil {
		t.Error("the chain was created outside the root")
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Stat(outside/b) = %v, want fs.ErrNotExist", statErr)
	}
}

// The absolute-path family reaches the same chain through deepestExistingDir, so
// the same shape one level higher must report the same way.
func TestMkdirAllAbs_NonDirectoryInChain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("i am a file"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
	_, err := WriteFile(t.Context(), filepath.Join(dir, "a", "b", "f.txt"), []byte("x"), WithMkdirMode(0o755))
	if err == nil {
		t.Fatal("WriteFile(file in the chain) = nil, want an error")
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("error = %v, want errors.Is syscall.ENOTDIR", err)
	}
}

func TestDeepestExistingDir(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "x", "y"), 0o755); err != nil {
		t.Fatalf("MkdirAll() = %v", err)
	}

	tests := map[string]struct {
		dir      string
		wantBase string
		wantRel  string
	}{
		"itself_exists":  {dir: filepath.Join(base, "x", "y"), wantBase: filepath.Join(base, "x", "y"), wantRel: "."},
		"one_missing":    {dir: filepath.Join(base, "x", "y", "z"), wantBase: filepath.Join(base, "x", "y"), wantRel: "z"},
		"three_missing":  {dir: filepath.Join(base, "x", "p", "q", "r"), wantBase: filepath.Join(base, "x"), wantRel: filepath.Join("p", "q", "r")},
		"root_is_enough": {dir: base, wantBase: base, wantRel: "."},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gotBase, gotRel, err := deepestExistingDir(tc.dir)
			if err != nil {
				t.Fatalf("deepestExistingDir(%q) = error %v", tc.dir, err)
			}
			if gotBase != tc.wantBase || gotRel != tc.wantRel {
				t.Errorf("deepestExistingDir(%q) = (%q, %q), want (%q, %q)",
					tc.dir, gotBase, gotRel, tc.wantBase, tc.wantRel)
			}
		})
	}

	t.Run("non_directory_ancestor", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "f"), nil, 0o600); err != nil {
			t.Fatalf("WriteFile() = %v", err)
		}
		_, _, err := deepestExistingDir(filepath.Join(d, "f", "g"))
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("deepestExistingDir(under a file) = %v, want errors.Is syscall.ENOTDIR", err)
		}
	})
}

// The mkdir chain runs through an *os.Root opened on the deepest existing
// ancestor, and a symlinked ancestor that ALREADY existed is followed, exactly as
// os.MkdirAll followed it — /var/log being a link to /mnt/log is ordinary, and
// refusing it would break callers. Pinned so the confinement claim in
// mkdirAllAbs's doc is not read as stronger than it is.
func TestMkdirAllAbs_FollowsAnExistingSymlinkedAncestor(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(base, "a")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	path := filepath.Join(base, "a", "b", "f.txt")
	if _, err := WriteFile(t.Context(), path, []byte("payload"), WithMkdirMode(0o755)); err != nil {
		t.Fatalf("WriteFile(through an existing symlinked ancestor) = %v, want nil", err)
	}
	assertContent(t, filepath.Join(target, "b", "f.txt"), "payload")
}
