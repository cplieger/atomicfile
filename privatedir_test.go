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

// permOf returns the bits chmod(2) owns for path — permissions plus
// setuid/setgid/sticky — read with Lstat so a planted symlink is reported as
// itself rather than as whatever it points at.
func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return chmodBits(fi.Mode())
}

// mkdirExact creates dir and then CHMODs it to mode, because mkdir(2)'s mode is
// a request: measured on the ZFS nfs4acl dataset this library is developed on,
// os.Mkdir(dir, 0o700) stores 0770. A fixture built from the mkdir mode alone
// would be asserting against whatever the test host's filesystem felt like
// storing, which is the very drift EnsurePrivateDir exists to catch.
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

// mkdirStoresRequestedMode reports whether THIS filesystem honours a 0700 mkdir,
// measured in dir rather than assumed. It is the oracle the create-path tests
// need: on ext4/tmpfs a fresh 0700 directory comes back 0700 and no repair is
// due, while on a dataset with an inheritable group ACE the same call stores
// 0770 and the repair MUST fire. Asserting either outcome unconditionally would
// pass on one host and fail on the other for the right reason.
func mkdirStoresRequestedMode(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "_mode-oracle")
	if err := os.Mkdir(probe, privateDirMode); err != nil {
		t.Fatalf("mkdir oracle %s: %v", probe, err)
	}
	t.Cleanup(func() { _ = os.Remove(probe) })
	return permOf(t, probe) == privateDirMode
}

// TestEnforceModeStoresAndReportsTheMode pins the whole point of the primitive:
// it reports the mode read back from the handle AFTER the chmod, not the mode
// that was asked for, and it does so in both directions (a tightening and a
// widening) so the return value cannot be a hard-coded echo of want.
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

// TestEnforceModeWorksOnADirectoryHandle pins that the primitive serves the
// directory case — an *os.File is an *os.File — and that a directory's type bits
// never read as a mismatch: comparing the raw os.FileMode would see ModeDir|0700
// against 0700 and refuse a directory it had just set correctly.
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

// TestEnforceModeNilFile pins that the guard is a refusal rather than a nil
// dereference: a caller whose open failed and whose error check slipped gets an
// error it can match, not a panic.
func TestEnforceModeNilFile(t *testing.T) {
	t.Parallel()
	if _, err := EnforceMode(nil, privateDirMode); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("EnforceMode(nil, 0700) = %v, want ErrUnsafePath", err)
	}
}

// Two error paths in this file are deliberately untested, and both for the same
// reason rather than for want of effort. A failing fchmod/fstat needs the
// descriptor's ownership or its mount taken away mid-call, which cannot be staged
// in a temp directory without dropping thread credentials underneath a parallel
// suite; and ErrModeNotStored is a property of the MOUNT — a filesystem that
// stores something other than what chmod set — which is exactly the failure the
// declined Result.Mode proposal noted could not be staged locally either. Both would need an injection seam whose only consumer
// is the test that exercises it, which this package does not add for a branch that
// returns the filesystem's own error unchanged.

// TestEnsurePrivateDirCreatesWhenAbsent pins the ordinary path: the level did not
// exist, this call made it, and what is on disk afterwards is owner-only whatever
// the filesystem's opinion of the mkdir mode was.
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
	// Repaired is a statement about the FILESYSTEM, so it is asserted against
	// what this filesystem was just measured doing rather than against a guess.
	if wantRepaired := !honest; pd.Repaired != wantRepaired {
		t.Errorf("Repaired = %v, want %v (a 0700 mkdir in %s stores %#o)",
			pd.Repaired, wantRepaired, parent, permOf(t, dir))
	}
}

// TestEnsurePrivateDirAdoptsCompliantExisting pins the second-run case every
// consumer actually hits: a directory this process left behind on a previous boot
// is adopted, reported as not-created, and left exactly as it was found.
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

// TestEnsurePrivateDirRefusesPlantedOccupant pins the refusals that make this a
// custody check rather than a mkdir wrapper. Each case is something a local user
// who wins the race to the name can leave there, and the sentinel is what lets a
// caller tell "somebody planted an object at my directory's name" apart from a
// mode or owner problem.
//
// The FIFO case carries a second property the assertion cannot show: it must
// RETURN. open(2) on a named pipe with no writer blocks indefinitely, so a test
// that hangs here is the finding.
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

// TestEnsurePrivateDirRefusesWideExistingMode pins the pre-existing half of the
// mode rule, in both its parts: any group or other bit is a refusal, and the
// directory is left exactly as found. The second part is the load-bearing one —
// chmod'ing a directory another principal made into compliance would take over
// their name and hand them whatever gets written under it, so the refusal must
// not "help".
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

// TestEnsurePrivateDirRefusesForeignOwner pins the check that is easiest to leave
// out and least visible when it is missing: a perfectly-moded 0700 directory
// owned by another uid passes every other check here, and its owner can still
// rename or replace it after the verdict returns.
//
// The fixture needs privilege — an unprivileged process cannot give a directory
// away — so the test skips rather than faking the ownership it is testing.
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

// TestEnsurePrivateDirRepairsWidenedCreate is the test the primitive exists for:
// a directory this call created whose mode came back WIDER than the 0700 it
// asked for must be repaired and re-verified, not returned as compliant.
//
// The widening is real, not mocked. A setgid parent makes the kernel itself store
// something other than what mkdir requested — Linux propagates S_ISGID to a new
// subdirectory — which is the same class of divergence as the inheritable
// group@:rwx ACE measured on ZFS (there the permission bits widen to 0770; here
// the setgid bit appears), and it reproduces on any Linux filesystem. The fixture
// is verified before the assertion: if this kernel stops widening, the test fails
// as INVALID rather than passing vacuously.
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

// TestEnsurePrivateDirDoesNotWarnWithoutARepair pins the other half of that log
// contract: the Warn says "this filesystem ignored a mode request", so it must
// not fire on a filesystem that honoured one.
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

// TestEnsurePrivateDirRejectsBadPaths pins that the path gate is the same one the
// rest of the ambient-path surface applies, and that it fires before anything is
// created: a relative path would make the verdict a statement about the process's
// current directory.
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

// TestEnsurePrivateDirDoesNotCreateParents pins the one-level contract: a missing
// ancestor is the caller's to establish, level by level, because only the caller
// knows which levels are its own. An os.MkdirAll here would create ancestors this
// function never inspected and could not vouch for.
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

// TestEnsurePrivateDirLoopsForAMultiLevelPath pins the composition the doc
// prescribes, and the reason it is the caller's loop: each level gets its own
// verdict, and the outer level is established before the inner one is even named.
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

// TestEnsurePrivateDir_RepairOwnedDirAdoptsTheAppsOwnPastOutput pins the option
// that keeps a mode fix from becoming an outage on upgrade. An app whose earlier
// release created <root>/<key>/ at 0700 on a filesystem that stored 0770 meets
// its OWN directory on the next run; the default rule refuses it, which fails
// every item at a directory the app itself made.
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

// TestEnsurePrivateDir_RefusesAnOwnedWideDirWithoutTheOption pins that the
// default is unchanged, so adding the option cannot silently narrow a directory
// somebody widened on purpose (seadex-scout's report dir is the live case).
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

// TestEnsurePrivateDir_RepairOwnedDirStillRefusesAForeignOwner pins that the
// option relaxes exactly one refusal. Ownership is what makes repairing sound —
// mkdir(2) gives a directory to its creator and a directory cannot be hard-link targeted
// — so the euid check must keep firing first, or the option would let the library
// chmod a neighbour's directory.
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

// TestEnsurePrivateDir_RepairOwnedDirLeavesAnAlreadyPrivateDirAlone pins that the
// option is not a blanket chmod: a pre-existing directory that is already private
// is adopted untouched and reports no repair, so an app can trust Repaired as a
// signal that the filesystem drifted rather than as noise on every boot.
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
