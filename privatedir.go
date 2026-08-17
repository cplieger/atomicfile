package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// privateDirMode is the mode EnsurePrivateDir requests and enforces: owner-only
// access, the one mode that makes a directory's contents unreachable to every
// other principal on the host.
const privateDirMode os.FileMode = 0o700

// PrivateDir reports the outcome of one EnsurePrivateDir call: what happened to
// the directory, and what the filesystem actually stored for it.
//
// It is only meaningful alongside a nil error. A non-nil error means no verdict
// was reached, which is NOT the same as "nothing happened" — the mkdir may have
// succeeded and a later step refused the result, so the directory can exist
// while its custody is unestablished. Treat the failure as fatal for the
// purpose the directory was for; do not retry into it.
type PrivateDir struct {
	// Mode is the mode the filesystem actually stored, as read back from the
	// open handle: the permission bits plus setuid/setgid/sticky, without the
	// type bits. When the error is nil this is always privateDirMode, except on
	// the pre-existing-and-already-private path, where it is what was found.
	Mode os.FileMode
	// Created reports whether THIS call created the directory. It is the field
	// the repair decision turns on (see EnsurePrivateDir): a directory this
	// process just made is one no other writer has ever held a name to, and a
	// pre-existing one belongs to whoever made it.
	Created bool
	// Repaired reports whether the mode had to be corrected because the
	// filesystem did not store what it was asked for — an inheritable ACL
	// widening a fresh 0700 directory. On a Created directory that repair is
	// unconditional; on a pre-existing one it happens only under
	// WithRepairOwnedDir, so Repaired without Created means the option was set
	// AND it changed something. Either way it is worth logging: it says the
	// filesystem under this path does not honour mode requests, which every
	// other mkdir in the program is also subject to.
	Repaired bool
}

// EnforceMode sets want on the OPEN HANDLE f and returns the mode the
// filesystem actually stored, having already refused a mismatch.
//
// It exists because a mode argument is a REQUEST, not a result, and nothing in
// the os package says so. mkdir(2) and open(2) take the mode through umask, and
// a filesystem carrying an inheritable ACL can widen the outcome regardless of
// what was asked: measured on a ZFS nfs4acl dataset, an inheritable group@:rwx
// ACE yields 0770 from a 0o700 mkdir, and a child of an already-0700 parent
// comes back 0770 too, so tightening the parent does not cover it. chmod(2) is
// the only call that SETS a mode, and a caller that stops there has still only
// issued a second request. So the sequence is chmod then stat, and the stat is
// the whole point: it is the only thing that turns "I asked for 0600" into "the
// file is 0600".
//
// The handle is what makes the verdict sound, for the reason OpenRegular
// records: observing the pathname a second time to stat it re-opens the window
// the check exists to close. A path-based chmod-then-stat can chmod one file and
// certify another if the name is swapped in between, and against a hostile
// neighbour that is precisely the sequence to attack. Both calls here are
// fchmod(2) and fstat(2) on the same descriptor, which no rename can redirect.
// It works for a file or a directory because both are an *os.File.
//
// Only the bits chmod(2) can set are compared — the permission bits plus
// setuid/setgid/sticky — so the type bits an os.FileMode also carries never make
// a directory look like a mismatch. That comparison includes setgid on purpose:
// a directory created under a setgid parent inherits the bit on Linux, which is
// a real difference between what was asked for and what is on disk.
//
// It is a primitive, not a policy: what to do about a filesystem that refuses
// the mode stays with the caller. A mismatch returns ErrModeNotStored naming
// both modes, and the stored mode is returned alongside it so a caller that
// wants to warn and continue does not have to re-stat to find out what it got.
// Every other error is the filesystem's own (fchmod or fstat failing), returned
// unwrapped so errors.Is against fs.ErrPermission works.
func EnforceMode(f *os.File, want os.FileMode) (os.FileMode, error) {
	if f == nil {
		return 0, fmt.Errorf("%w: nil file", ErrUnsafePath)
	}
	// (*os.File).Chmod is fchmod(2) on the descriptor, not chmod(2) on f.Name().
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

// chmodBits reduces a mode to the bits chmod(2) can set: the permission bits
// plus setuid, setgid and sticky. It is what makes an os.FileMode comparison
// meaningful across a file and a directory, whose type bits differ and are not
// settable by anyone.
func chmodBits(m os.FileMode) os.FileMode {
	return m.Perm() | m&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
}

// EnsurePrivateDir establishes custody of ONE directory level that only this
// process's user may enter: it creates dir 0700 if absent, then proves from an
// open handle that what is now at that path is a real directory, owned by the
// effective uid, with no group or other access.
//
// It is the composition callers keep hand-rolling. Two apps in this fleet
// establish custody of a private directory inside a parent other users can
// write — a state directory under /tmp, an admin-socket directory — and their
// copies have already DRIFTED on the mode question: one learned that mkdir's
// mode is a request and added a chmod plus a re-stat of the levels it created,
// the other never did, so all four of its checks run only on the already-exists
// path and its own fresh mkdir returns success having verified nothing. That is
// the bug this exists to delete: a custody directory born group-writable, by a
// call that reported success, and never looked at again.
//
// # One level, deliberately
//
// dir's own parent is not created, inspected, or vouched for. A multi-level path
// is a loop over the levels, outermost first, because each level needs the same
// verdict and the caller is the only one who knows which levels are its own —
// the one existing consumer that does this already loops. os.MkdirAll cannot
// substitute for either half: it stats the path, FOLLOWS a symlink, finds a
// directory and returns nil, so it cannot tell "I created this" from "something
// was already here", which is the distinction every decision below turns on.
//
// # The sequence, and why each step is there
//
//  1. mkdir(2) at 0700, recording whether THIS call created the level. An
//     fs.ErrExist means it did not; any other error is returned as the
//     filesystem gave it.
//  2. Open the result O_DIRECTORY|O_NOFOLLOW|O_RDONLY. O_NOFOLLOW makes the
//     KERNEL refuse a symlink planted at the final component rather than
//     following it, which no check-then-open sequence can do without a race;
//     O_DIRECTORY refuses anything that is not a directory and is also what stops
//     a planted FIFO stalling the caller indefinitely in open(2) with no writer on
//     the other end. Which of the two refusals fired is reported as
//     ErrSymlinkTarget or ErrNotDirectory; because the flags collapse a
//     final-component symlink onto ENOTDIR, telling them apart costs one Lstat of
//     the already-refused name, used to label the error and nothing else (see
//     refusedOccupant).
//  3. fstat(2) the handle. Everything after this reads the descriptor, never the
//     pathname, so no rename between two observations can make the verdict
//     describe a different object than the one inspected.
//  4. Require the owner uid to equal os.Geteuid(). This is ownership, not just
//     mode, and it is the check most easily left out: a level somebody else owns
//     is one they can rename or replace AFTER the verdict returns — including
//     with a symlink the caller then follows — so a perfectly-moded 0700
//     directory owned by another uid passes every other check here and still
//     leaves the planted-path attack reachable.
//  5. If this call CREATED the level, EnforceMode it to 0700. This is the
//     ACL-widening repair, and it is safe precisely BECAUSE we made it: mkdir
//     reported the path as ours, so no other writer has ever held a name there
//     to swap in, and the chmod cannot be tightening someone else's directory.
//  6. If the level PRE-EXISTED, it is never chmod'ed — repairing a directory
//     another principal made is not this primitive's call, and taking over the
//     name would hand them whatever gets written under it. It is instead REFUSED
//     when any group or other bit is set (ErrModeTooOpen), which is what keeps
//     the refusal firing on exactly the planted shape the check is for.
//
// # The verdict is POINT-IN-TIME, and this is the limit to read carefully
//
// It proves what was true of one inode while a handle was open on it. It does
// NOT pin the directory for the caller's later use, and nothing here can: the
// handle is closed before the call returns, and a caller that then does a
// ReadDir, a Remove, an Open or a Mkdir through the PATHNAME re-resolves every
// component and re-opens the window this closed. Where the parent is writable by
// others, the honest reading is that the verdict covers the moment of the check
// and the caller's own subsequent operations are unprotected by it.
//
// What would close that gap is acting through a retained handle instead of a
// name — the openat(2)-relative operations of an *os.Root (see OpenParentInRoot
// for the same argument about ancestor components) — which is a different API
// shape: the caller would hold the directory open for its whole lifetime and
// name only leaves inside it. Neither current consumer needs that: one seeds a
// directory whose entries are unguessable handles, the other creates a socket
// directory once at startup. If a consumer ever does, the fix is a root-returning
// sibling of this function, not more checks in it.
//
// # Errors
//
// ErrEmptyPath or ErrUnsafePath when dir is empty, holds a NUL, or is not
// absolute once cleaned (the ValidatePath rule; a relative dir would make the
// verdict a statement about the process's current directory, which another
// goroutine can change while this runs). ErrSymlinkTarget for a symlink at the
// final component, ErrNotDirectory for a plain file, FIFO, device node or socket,
// ErrNotOwned for a foreign owner, ErrModeTooOpen for a pre-existing directory
// with group or other access, ErrModeNotStored when the filesystem refuses to
// store 0700 on a directory this call created. Everything else is the
// filesystem's own error, so a permission failure matches fs.ErrPermission.
// Match with errors.Is.
//
// Of the functional options, WithRepairOwnedDir decides the pre-existing
// too-open directory case above, and WithLogger supplies the logger for the
// one Warn line a mode repair emits; every other Option is ignored (the mode
// is not a parameter — a "private directory" with a caller-chosen mode is a
// different primitive, and this one's whole contract is 0700).
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
		// WithRepairOwnedDir: the euid-ownership check above has already proved
		// this directory is ours, which is what makes narrowing it our call
		// rather than an intrusion. With repair declined this is where a caller
		// meeting its own past output gets refused.
		if !c.repairOwnedDir {
			return PrivateDir{}, fmt.Errorf(
				"%w: %s: mode %#o grants group or other access, and a pre-existing directory is never repaired without WithRepairOwnedDir(true)",
				ErrModeTooOpen, clean, found)
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

// mkdirPrivate creates dir at 0700 and reports whether THIS call created it. An
// fs.ErrExist is the "somebody else made it" answer, not a failure, because the
// caller's next question is which of the two happened; every other error is the
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

// openPrivateDir opens dir as a directory without following a symlink at the
// final component. O_DIRECTORY refuses anything that is not a directory and
// O_NOFOLLOW refuses a link, both in the kernel, before this process holds a
// descriptor on the wrong object; O_NONBLOCK is what makes a planted FIFO an
// immediate rejection rather than an indefinite hang.
//
// The two flags TOGETHER collapse the error, which is why the classification
// below exists: with O_DIRECTORY in the mix Linux reports a final-component
// symlink as ENOTDIR (the link is not a directory) rather than the ELOOP
// O_NOFOLLOW alone would give, so the errno cannot tell a planted link from a
// planted file. Both arms are kept because the kernel is free to report either.
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

// refusedOccupant names which occupant the kernel just refused, mapping it onto
// this package's vocabulary: ErrSymlinkTarget for a link (the same sentinel the
// write side and OpenRegular use for a refused link), ErrNotDirectory for a plain
// file, FIFO, device node or socket.
//
// It observes the pathname a second time, which everywhere else in this package
// is the bug. It is sound HERE and only here because the refusal has already
// happened: the open failed, no descriptor exists, and nothing downstream depends
// on this answer. The Lstat only labels an error message, so the worst a name
// swapped in between can achieve is a misleading sentinel on a path that was
// refused either way — never an admission. An Lstat that fails, or that finds
// something else because the planted object has already been withdrawn, takes the
// ErrNotDirectory arm, which is what the open itself said.
func refusedOccupant(dir string, err error) error {
	if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s: %w", ErrSymlinkTarget, dir, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrNotDirectory, dir, err)
}

// statOwnedDir stats the OPEN descriptor and refuses anything that is not a
// directory owned by the effective uid. Stat the handle, not the path: the same
// rule statRegular records, and the reason every check after the open reads fi
// rather than the name.
//
// The type check is not redundant with O_DIRECTORY: it is the postcondition that
// keeps the refusal true if the open flags ever change, and the FileInfo is
// needed for the mode and owner anyway.
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
