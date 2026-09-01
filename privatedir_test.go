package atomicfile

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// permOf returns the bits chmod(2) owns for path, read with Lstat so a
// planted symlink is reported as itself, not its target.
func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return chmodBits(fi.Mode())
}

// mkdirExact creates dir and then CHMODs it to mode, because mkdir(2)'s
// mode is only a request (measured on ZFS nfs4acl: 0o700 mkdir stores
// 0770), and a fixture built from the mkdir mode alone would assert
// against whatever the filesystem happened to store.
func mkdirExact(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatalf("chmod %s to %#o: %v", dir, mode, err)
	}
	if got := permOf(t, dir); got != mode {
		t.Fatalf("fixture %s is mode %#o, want %#o: the filesystem refuses the mode this case needs", dir, got, mode)
	}
}

// mkdirStoresRequestedMode measures (rather than assumes) whether this
// filesystem honours a 0700 mkdir — the oracle the create-path tests need,
// since asserting either outcome unconditionally passes on one host and
// fails on the other for the right reason.
func mkdirStoresRequestedMode(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "_mode-oracle")
	if err := os.Mkdir(probe, privateDirMode); err != nil {
		t.Fatalf("mkdir oracle %s: %v", probe, err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })
	return permOf(t, probe) == privateDirMode
}

// TestEnforceModeStoresAndReportsTheMode pins that it reports the mode
// read back from the handle AFTER the chmod, not the mode asked for, in
// both a tightening and a widening direction.
func TestEnforceModeStoresAndReportsTheMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	for _, want := range []os.FileMode{0o600, 0o640, 0o400} {
		got, err := EnforceMode(f, want)
		if err != nil {
			t.Fatalf("EnforceMode(f, %#o) = %v, want nil", want, err)
		}
		if got != want {
			t.Errorf("EnforceMode(f, %#o) = %#o, want %#o", want, got, want)
		}
		if onDisk := permOf(t, path); onDisk != want {
			t.Errorf("after EnforceMode(f, %#o) the file is %#o on disk, want %#o", want, onDisk, want)
		}
	}
}

// TestEnforceModeWorksOnADirectoryHandle pins that a directory's type
// bits never read as a mismatch: comparing the raw os.FileMode would see
// ModeDir|0700 against 0700 and refuse a directory it just set correctly.
func TestEnforceModeWorksOnADirectoryHandle(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "d")
	mkdirExact(t, dir, 0o777)
	f, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer f.Close()

	got, err := EnforceMode(f, privateDirMode)
	if err != nil {
		t.Fatalf("EnforceMode(dir, 0700) = %v, want nil", err)
	}
	if got != privateDirMode {
		t.Errorf("EnforceMode(dir, 0700) = %#o, want %#o (type bits must not be part of the comparison)", got, privateDirMode)
	}
	if onDisk := permOf(t, dir); onDisk != privateDirMode {
		t.Errorf("directory is %#o on disk, want %#o", onDisk, privateDirMode)
	}
}

// TestEnforceModeNilFile pins that a nil file is a refusal, not a nil
// dereference.
func TestEnforceModeNilFile(t *testing.T) {
	t.Parallel()
	if _, err := EnforceMode(nil, privateDirMode); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("EnforceMode(nil, 0700) = %v, want ErrUnsafePath", err)
	}
}

// Two error paths are deliberately untested: a failing fchmod/fstat needs
// the descriptor's ownership or mount taken away mid-call, and
// ErrModeNotStored is a property of the MOUNT itself. Neither is
// stageable in a temp directory without an injection seam whose only
// consumer would be this test.

// TestEnsurePrivateDirCreatesWhenAbsent pins the ordinary path: the level
// did not exist, this call made it, and what is on disk afterwards is
// owner-only regardless of the filesystem's mkdir behavior.
func TestEnsurePrivateDirCreatesWhenAbsent(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	honest := mkdirStoresRequestedMode(t, parent)
	dir := filepath.Join(parent, "state")

	pd, err := EnsurePrivateDir(dir)
	if err != nil {
		t.Fatalf("EnsurePrivateDir(%s) = %v, want nil", dir, err)
	}
	if !pd.Created {
		t.Error("Created = false, want true: the directory did not exist before the call")
	}
	if pd.Mode != privateDirMode {
		t.Errorf("Mode = %#o, want %#o", pd.Mode, privateDirMode)
	}
	if onDisk := permOf(t, dir); onDisk != privateDirMode {
		t.Errorf("directory is %#o on disk, want %#o", onDisk, privateDirMode)
	}
	// Repaired describes the FILESYSTEM, so assert against what it was just
	// measured doing, not a guess.
	if wantRepaired := !honest; pd.Repaired != wantRepaired {
		t.Errorf("Repaired = %v, want %v (a 0700 mkdir in %s stores %#o)",
			pd.Repaired, wantRepaired, parent, permOf(t, dir))
	}
}

// TestEnsurePrivateDirAdoptsCompliantExisting pins the second-run case: a
// directory left behind by a previous boot is adopted, reported as
// not-created, and left exactly as found.
func TestEnsurePrivateDirAdoptsCompliantExisting(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	mkdirExact(t, dir, privateDirMode)

	pd, err := EnsurePrivateDir(dir)
	if err != nil {
		t.Fatalf("EnsurePrivateDir(pre-existing 0700) = %v, want nil", err)
	}
	if pd.Created {
		t.Error("Created = true, want false: the directory already existed")
	}
	if pd.Repaired {
		t.Error("Repaired = true, want false: a pre-existing directory is never chmod'ed")
	}
	if pd.Mode != privateDirMode {
		t.Errorf("Mode = %#o, want %#o", pd.Mode, privateDirMode)
	}
}

// TestEnsurePrivateDirRefusesPlantedOccupant pins the refusals that make
// this a custody check rather than a mkdir wrapper: each case is something
// a local user winning the name race could leave there.
//
// The FIFO case also has to RETURN: open(2) on a pipe with no writer
// blocks indefinitely, so a hang here is itself the finding.
func TestEnsurePrivateDirRefusesPlantedOccupant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		plant func(t *testing.T, path string)
		name  string
		want  error
	}{
		{
			name: "symlink_to_a_compliant_directory",
			plant: func(t *testing.T, path string) {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "elsewhere")
				mkdirExact(t, target, privateDirMode)
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			want: ErrSymlinkTarget,
		},
		{
			name: "dangling_symlink",
			plant: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(filepath.Dir(path), "missing"), path); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			want: ErrSymlinkTarget,
		},
		{
			name: "plain_file",
			plant: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatalf("write file: %v", err)
				}
			},
			want: ErrNotDirectory,
		},
		{
			name: "fifo",
			plant: func(t *testing.T, path string) {
				t.Helper()
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatalf("mkfifo: %v", err)
				}
			},
			want: ErrNotDirectory,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "state")
			tt.plant(t, dir)

			pd, err := EnsurePrivateDir(dir)
			if !errors.Is(err, tt.want) {
				t.Errorf("EnsurePrivateDir(%s planted) = %v, want %v", tt.name, err, tt.want)
			}
			if pd != (PrivateDir{}) {
				t.Errorf("PrivateDir = %+v, want the zero value: a refusal reaches no verdict", pd)
			}
		})
	}
}

// TestEnsurePrivateDirRefusesWideExistingMode pins that any group or
// other bit on a pre-existing directory is refused AND the directory is
// left exactly as found — chmod'ing another principal's directory into
// compliance would take over their name.
func TestEnsurePrivateDirRefusesWideExistingMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []os.FileMode{0o770, 0o750, 0o710, 0o707, 0o701, 0o704} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "state")
			mkdirExact(t, dir, mode)

			if _, err := EnsurePrivateDir(dir); !errors.Is(err, ErrModeTooOpen) {
				t.Errorf("EnsurePrivateDir(pre-existing %#o) = %v, want ErrModeTooOpen", mode, err)
			}
			if got := permOf(t, dir); got != mode {
				t.Errorf("directory is now %#o, want it left at %#o: a refused directory must not be repaired", got, mode)
			}
		})
	}
}

// TestEnsurePrivateDirRefusesForeignOwner pins that a perfectly-moded
// 0700 directory owned by another uid is still refused: its owner could
// rename or replace it after the verdict returns.
//
// Needs privilege to chown a directory away, so it skips rather than
// faking the ownership under test.
func TestEnsurePrivateDirRefusesForeignOwner(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("needs privilege to create a directory owned by another uid")
	}
	dir := filepath.Join(t.TempDir(), "state")
	mkdirExact(t, dir, privateDirMode)
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatalf("chown: %v", err)
	}

	_, err := EnsurePrivateDir(dir)
	if !errors.Is(err, ErrNotOwned) {
		t.Errorf("EnsurePrivateDir(foreign-owned 0700) = %v, want ErrNotOwned", err)
	}
}

// TestEnsurePrivateDirRepairsWidenedCreate pins that a directory this
// call created, whose mode came back WIDER than 0700, is repaired and
// re-verified rather than returned as compliant.
//
// The widening is real: a setgid parent makes Linux propagate S_ISGID to
// a new subdirectory. The fixture is verified before the assertion, so a
// kernel that stops widening fails the test as INVALID rather than
// passing vacuously.
func TestEnsurePrivateDirRepairsWidenedCreate(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "parent")
	mkdirExact(t, parent, os.ModeSetgid|privateDirMode)

	// Confirm the fixture still produces a mode mkdir did not ask for.
	witness := filepath.Join(parent, "witness")
	if err := os.Mkdir(witness, privateDirMode); err != nil {
		t.Fatalf("mkdir witness: %v", err)
	}
	widened := permOf(t, witness)
	if widened == privateDirMode {
		t.Fatalf("fixture no longer widens: a 0700 mkdir under a %v parent stored %#o, so this test cannot exercise the repair",
			os.ModeSetgid|privateDirMode, widened)
	}

	h := &captureHandler{}
	dir := filepath.Join(parent, "state")
	pd, err := EnsurePrivateDir(dir, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("EnsurePrivateDir(%s) = %v, want nil", dir, err)
	}
	if !pd.Created {
		t.Fatal("Created = false, want true")
	}
	if !pd.Repaired {
		t.Errorf("Repaired = false, want true: mkdir stored %#o instead of %#o", widened, privateDirMode)
	}
	if pd.Mode != privateDirMode {
		t.Errorf("Mode = %#o, want %#o: the repair must re-stat, not report what was asked for", pd.Mode, privateDirMode)
	}
	if onDisk := permOf(t, dir); onDisk != privateDirMode {
		t.Errorf("directory is %#o on disk, want %#o: the widening survived the repair", onDisk, privateDirMode)
	}
	if got := h.CountLevelExact(slog.LevelWarn, msgPrivateDirRepaired); got != 1 {
		t.Errorf("Warn %q count = %d, want 1: a filesystem that ignores mode requests is operator-visible news",
			msgPrivateDirRepaired, got)
	}
}

// TestEnsurePrivateDirDoesNotWarnWithoutARepair pins that the Warn must
// not fire on a filesystem that honoured the mode request.
func TestEnsurePrivateDirDoesNotWarnWithoutARepair(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	if !mkdirStoresRequestedMode(t, parent) {
		t.Skip("this filesystem widens a 0700 mkdir, so every create here is a repair")
	}
	h := &captureHandler{}
	if _, err := EnsurePrivateDir(filepath.Join(parent, "state"), WithLogger(slog.New(h))); err != nil {
		t.Fatalf("EnsurePrivateDir = %v, want nil", err)
	}
	if got := h.CountLevelExact(slog.LevelWarn, msgPrivateDirRepaired); got != 0 {
		t.Errorf("Warn %q count = %d, want 0", msgPrivateDirRepaired, got)
	}
}

// TestEnsurePrivateDirRejectsBadPaths pins that the path gate fires
// before anything is created: a relative path would make the verdict a
// statement about the process's current directory.
func TestEnsurePrivateDirRejectsBadPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dir  string
		want error
	}{
		{"empty", "", ErrEmptyPath},
		{"relative", "state/dir", ErrUnsafePath},
		{"null_byte", "/tmp/a\x00b", ErrUnsafePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := EnsurePrivateDir(tt.dir); !errors.Is(err, tt.want) {
				t.Errorf("EnsurePrivateDir(%q) = %v, want %v", tt.dir, err, tt.want)
			}
		})
	}
}

// TestEnsurePrivateDirDoesNotCreateParents pins the one-level contract:
// a missing ancestor is the caller's to establish. An os.MkdirAll here
// would create ancestors this function never inspected.
func TestEnsurePrivateDirDoesNotCreateParents(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir := filepath.Join(base, "missing", "state")

	if _, err := EnsurePrivateDir(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("EnsurePrivateDir(%s) = %v, want an fs.ErrNotExist match", dir, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the missing parent exists after the call (lstat = %v); one level means one level", err)
	}
}

// TestEnsurePrivateDirLoopsForAMultiLevelPath pins that each level gets
// its own verdict, with the outer level established before the inner one
// is even named.
func TestEnsurePrivateDirLoopsForAMultiLevelPath(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "inner")

	for _, dir := range []string{outer, inner} {
		pd, err := EnsurePrivateDir(dir)
		if err != nil {
			t.Fatalf("EnsurePrivateDir(%s) = %v, want nil", dir, err)
		}
		if !pd.Created {
			t.Errorf("%s: Created = false, want true", dir)
		}
		if got := permOf(t, dir); got != privateDirMode {
			t.Errorf("%s is %#o on disk, want %#o", dir, got, privateDirMode)
		}
	}
}

// TestEnsurePrivateDir_RepairOwnedDirAdoptsTheAppsOwnPastOutput pins the
// upgrade case: an app whose earlier release created a 0700 dir that
// stored 0770 meets its OWN directory on the next run, and the default
// rule would refuse it.
func TestEnsurePrivateDir_RepairOwnedDirAdoptsTheAppsOwnPastOutput(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "legacy")
	mkdirExact(t, dir, 0o770)

	pd, err := EnsurePrivateDir(dir, WithRepairOwnedDir(true))
	if err != nil {
		t.Fatalf("EnsurePrivateDir with WithRepairOwnedDir refused an owned 0770 dir: %v", err)
	}
	if pd.Mode != privateDirMode {
		t.Errorf("Mode = %#o, want %#o", pd.Mode, privateDirMode)
	}
	if !pd.Repaired {
		t.Error("Repaired = false; the option changed the mode, so the caller should be able to log the one-time repair")
	}
	if pd.Created {
		t.Error("Created = true for a pre-existing directory")
	}
	if got := permOf(t, dir); got != privateDirMode {
		t.Errorf("on-disk mode = %#o, want %#o", got, privateDirMode)
	}
}

// TestEnsurePrivateDir_RefusesAnOwnedWideDirWithoutTheOption pins that
// the default is unchanged: the option must not silently narrow a
// directory somebody widened on purpose.
func TestEnsurePrivateDir_RefusesAnOwnedWideDirWithoutTheOption(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shared")
	mkdirExact(t, dir, 0o750)

	if _, err := EnsurePrivateDir(dir); !errors.Is(err, ErrModeTooOpen) {
		t.Fatalf("err = %v, want ErrModeTooOpen: the default must still refuse", err)
	}
	if got := permOf(t, dir); got != 0o750 {
		t.Errorf("mode changed to %#o; a refusal must not mutate the directory", got)
	}
}

// TestEnsurePrivateDir_RepairOwnedDirStillRefusesAForeignOwner pins that
// the option relaxes exactly one refusal: the euid check must still fire
// first, or the option would let the library chmod a neighbour's
// directory.
func TestEnsurePrivateDir_RepairOwnedDirStillRefusesAForeignOwner(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("cannot chown to another uid unprivileged")
	}
	dir := filepath.Join(t.TempDir(), "theirs")
	mkdirExact(t, dir, 0o770)
	if err := os.Chown(dir, 12345, 12345); err != nil {
		t.Skipf("chown unavailable: %v", err)
	}

	if _, err := EnsurePrivateDir(dir, WithRepairOwnedDir(true)); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned even with WithRepairOwnedDir", err)
	}
}

// TestEnsurePrivateDir_RepairOwnedDirLeavesAnAlreadyPrivateDirAlone pins
// that the option is not a blanket chmod: an already-private directory is
// adopted untouched and reports no repair.
func TestEnsurePrivateDir_RepairOwnedDirLeavesAnAlreadyPrivateDirAlone(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "fine")
	mkdirExact(t, dir, 0o700)

	pd, err := EnsurePrivateDir(dir, WithRepairOwnedDir(true))
	if err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	if pd.Repaired {
		t.Error("Repaired = true for a directory that was already 0700")
	}
	if pd.Mode != privateDirMode {
		t.Errorf("Mode = %#o, want %#o", pd.Mode, privateDirMode)
	}
}
