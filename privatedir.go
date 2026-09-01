package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// privateDirMode is the mode EnsurePrivateDir requests and enforces:
// owner-only access.
const privateDirMode os.FileMode = 0o700

// PrivateDir reports the outcome of one EnsurePrivateDir call.
//
// Only meaningful alongside a nil error: a non-nil error means no verdict
// was reached, which is NOT "nothing happened" — the mkdir may have
// succeeded and a later step refused the result, so the directory can exist
// while its custody is unestablished. Treat a failure as fatal; do not
// retry into it.
type PrivateDir struct {
	// Mode is the mode the filesystem actually stored, read back from the
	// open handle (permission bits plus setuid/setgid/sticky). Always
	// privateDirMode when the error is nil, except on the
	// pre-existing-and-already-private path, where it is what was found.
	Mode os.FileMode
	// Created reports whether THIS call created the directory — the field
	// the repair decision turns on: a directory this process just made is
	// one no other writer has ever held a name to.
	Created bool
	// Repaired reports whether the mode had to be corrected because the
	// filesystem did not store what was asked (e.g. an inheritable ACL
	// widening a fresh 0700 directory). Unconditional on a Created
	// directory; on a pre-existing one, only under WithRepairOwnedDir.
	Repaired bool
}

// EnforceMode sets want on the OPEN HANDLE f and returns the mode the
// filesystem actually stored, having already refused a mismatch.
//
// A mode argument is a REQUEST, not a result: mkdir(2)/open(2) apply umask,
// and an inheritable ACL can widen the outcome regardless of what was
// asked (measured on ZFS nfs4acl: 0o700 mkdir stores 0770). chmod(2) only
// SETS a mode, so the sequence here is chmod then stat — the stat is what
// turns "asked for 0600" into "is 0600".
//
// Both calls run on the open descriptor (fchmod/fstat), not the pathname,
// so a rename between two path-based observations cannot certify the wrong
// file. Works for a file or a directory, both being an *os.File.
//
// Only the bits chmod(2) can set are compared (permission bits plus
// setuid/setgid/sticky), so a directory's type bits never read as a
// mismatch, and setgid is included on purpose: Linux inherits it from a
// setgid parent.
//
// A mismatch returns ErrModeNotStored naming both modes, with the stored
// mode returned alongside so a caller that warns and continues need not
// re-stat. Every other error is the filesystem's own, unwrapped.
func EnforceMode(f *os.File, want os.FileMode) (os.FileMode, error) {
	if f == nil {
		return 0, fmt.Errorf("%w: nil file", ErrUnsafePath)
	}
	// (*os.File).Chmod is fchmod(2) on the descriptor, not chmod(2) on the name.
	if err := f.Chmod(want); err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	stored, asked := chmodBits(fi.Mode()), chmodBits(want)
	if stored != asked {
		return stored, fmt.Errorf("%w: %s: asked for %#o, filesystem stored %#o",
			ErrModeNotStored, f.Name(), asked, stored)
	}
	return stored, nil
}

// chmodBits reduces a mode to the bits chmod(2) can set: permission bits
// plus setuid, setgid and sticky. Strips the type bits so a file and a
// directory compare meaningfully.
func chmodBits(m os.FileMode) os.FileMode {
	return m.Perm() | m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

// EnsurePrivateDir establishes custody of ONE directory level that only
// this process's user may enter: it creates dir 0700 if absent, then proves
// from an open handle that what is now at that path is a real directory,
// owned by the effective uid, with no group or other access.
//
// dir's own parent is not created, inspected, or vouched for; a multi-level
// path is the caller's own loop, outermost first. os.MkdirAll cannot
// substitute: it follows a symlink and cannot tell "I created this" from
// "something was already here", which the decisions below turn on.
//
// The sequence and why each step matters: mkdir(2) at 0700, recording
// whether THIS call created the level (fs.ErrExist means it did not). Open
// the result O_DIRECTORY|O_NOFOLLOW|O_RDONLY — the kernel refuses a planted
// symlink or non-directory rather than following it, and a planted FIFO
// cannot stall the caller in open(2). fstat(2) the handle so every later
// check reads the descriptor, never the pathname. Require the owner uid to
// equal os.Geteuid() — a level owned by somebody else can be renamed or
// replaced after the verdict returns, so mode alone is not enough. If THIS
// call created the level, EnforceMode it to 0700 (safe because mkdir just
// proved nobody else ever held that name). If the level PRE-EXISTED, it is
// never chmod'ed — repairing another principal's directory would take over
// their name — and is instead refused if any group or other bit is set
// (ErrModeTooOpen).
//
// The verdict is POINT-IN-TIME: it proves what was true of one inode while
// a handle was open, and does NOT pin the directory for later use. A
// caller that then does ReadDir/Remove/Open/Mkdir through the PATHNAME
// re-resolves every component and re-opens the window this closed. Closing
// that gap needs a retained handle (an *os.Root) instead of a name — a
// different API shape neither current consumer needs.
//
// Errors: ErrEmptyPath/ErrUnsafePath for an empty, NUL-bearing, or
// non-absolute dir; ErrSymlinkTarget for a symlink at the final component;
// ErrNotDirectory for a plain file, FIFO, device node or socket;
// ErrNotOwned for a foreign owner; ErrModeTooOpen for a pre-existing
// directory with group or other access; ErrModeNotStored when the
// filesystem refuses to store 0700 on a directory this call created.
// Everything else is the filesystem's own error. Match with errors.Is.
//
// WithRepairOwnedDir decides the pre-existing too-open case above;
// WithLogger supplies the logger for the one Warn a mode repair emits.
// Every other Option is ignored — the mode is not a parameter.
func EnsurePrivateDir(dir string, opts ...Option) (PrivateDir, error) {
	c := buildCfg(opts)
	clean, err := validateAbsClean(dir)
	if err != nil {
		return PrivateDir{}, err
	}
	created, err := mkdirPrivate(clean)
	if err != nil {
		return PrivateDir{}, err
	}
	f, err := openPrivateDir(clean)
	if err != nil {
		return PrivateDir{}, err
	}
	defer func() { _ = f.Close() }()

	fi, err := statOwnedDir(f, clean)
	if err != nil {
		return PrivateDir{}, err
	}
	found := chmodBits(fi.Mode())
	if !created {
		if found&0o077 == 0 {
			return PrivateDir{Mode: found}, nil
		}
		// The euid-ownership check above already proved this directory is
		// ours, which is what makes narrowing it our call rather than an
		// intrusion.
		if !c.repairOwnedDir {
			return PrivateDir{}, fmt.Errorf(
				"%w: %s: mode %#o grants group or other access, and a pre-existing directory is never repaired without WithRepairOwnedDir(true)",
				ErrModeTooOpen, clean, found,
			)
		}
		stored, repairErr := EnforceMode(f, privateDirMode)
		if repairErr != nil {
			return PrivateDir{}, repairErr
		}
		c.logger.Warn("atomicfile: pre-existing directory was not private; repaired",
			"dir", clean, "requested", privateDirMode.String(), "found", found.String())
		return PrivateDir{Mode: stored, Repaired: true}, nil
	}

	stored, err := EnforceMode(f, privateDirMode)
	if err != nil {
		return PrivateDir{}, err
	}
	repaired := found != privateDirMode
	if repaired {
		c.logger.Warn("atomicfile: created directory did not keep the requested mode; repaired",
			"dir", clean, "requested", privateDirMode.String(), "stored", found.String())
	}
	return PrivateDir{Mode: stored, Created: true, Repaired: repaired}, nil
}

// mkdirPrivate creates dir at 0700 and reports whether THIS call created
// it. fs.ErrExist means it did not, not a failure; every other error is the
// filesystem's.
func mkdirPrivate(dir string) (bool, error) {
	err := os.Mkdir(dir, privateDirMode)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	return false, err
}

// openPrivateDir opens dir as a directory without following a symlink at
// the final component: O_DIRECTORY and O_NOFOLLOW refuse a non-directory or
// a link in the kernel before this process holds a wrong descriptor;
// O_NONBLOCK turns a planted FIFO into an immediate rejection.
//
// The two flags TOGETHER collapse the error: with O_DIRECTORY set, Linux
// reports a final-component symlink as ENOTDIR rather than ELOOP, so the
// errno alone cannot tell a planted link from a planted file — hence
// refusedOccupant.
func openPrivateDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		return f, nil
	}
	if errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) {
		return nil, refusedOccupant(dir, err)
	}
	return nil, err
}

// refusedOccupant names which occupant the kernel just refused:
// ErrSymlinkTarget for a link, ErrNotDirectory for a plain file, FIFO,
// device node or socket.
//
// It observes the pathname a second time, which elsewhere in this package
// is the bug it exists to avoid — but it is sound here because the open
// already failed and no descriptor exists, so a name swapped in between
// can only mislabel an error message, never grant an admission.
func refusedOccupant(dir string, err error) error {
	if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s: %w", ErrSymlinkTarget, dir, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrNotDirectory, dir, err)
}

// statOwnedDir stats the OPEN descriptor and refuses anything that is not
// a directory owned by the effective uid — the handle, not the path, so no
// later check depends on the name. The type check is not redundant with
// O_DIRECTORY: it stays true even if the open flags ever change.
func statOwnedDir(f *os.File, dir string) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsDir() {
		return nil, fmt.Errorf("%w: %s (type %s)", ErrNotDirectory, dir, fi.Mode().Type())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("%w: %s: ownership could not be determined", ErrNotOwned, dir)
	}
	if euid := os.Geteuid(); int(st.Uid) != euid {
		return nil, fmt.Errorf("%w: %s: owned by uid %d, want the effective uid %d; its owner could replace the checked path",
			ErrNotOwned, dir, st.Uid, euid)
	}
	return fi, nil
}
