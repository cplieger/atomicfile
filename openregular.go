package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// OpenRegularInRoot opens name inside root read-only and returns the open
// descriptor together with the FileInfo of that descriptor, having already
// refused anything that is not a regular file. The caller owns the returned
// file and closes it; root stays the caller's too.
//
// It is the open half of ReadBoundedInRoot, which is now built on it, so the
// two cannot drift: open through the root (a symlink or ".." component in name
// can never redirect the open outside its tree), non-blocking so a named pipe
// planted in the tree cannot stall the caller in open(2), then stat the OPEN
// HANDLE — never the pathname again — and refuse a directory, FIFO, device node
// or socket with ErrNotRegular. Every consumer that hand-rolls that sequence
// re-derives the same three non-obvious details, and one of them getting it
// wrong is a confinement bypass.
//
// The descriptor is what makes it reusable where the bytes are not. A caller
// STREAMING the file through a decoder or a decryptor never materializes it, so
// a []byte-returning helper cannot serve it at all; a caller that caches the
// file and must know when to reload needs the FileInfo the bytes came from,
// which is what Identify turns into a FileIdentity. Both are the same argument:
// binding the shape check, the identity and the read to ONE descriptor is the
// point. Observing the pathname a second time to stat it re-opens the window
// this closes — a rename in between makes a caller decode one generation's
// bytes while recording another's identity, and turns every non-regular-file
// rejection back into a check-then-open race.
//
// For the bytes, hand the descriptor to ReadBoundedFile; that composition IS
// ReadBoundedInRoot. OpenRegular is the ambient-path sibling.
//
// Errors are the filesystem's own, wrapped only where this package adds a
// verdict: name must be relative and null-byte-free (ErrEmptyPath /
// ErrUnsafePath), a missing file or a symlink escaping the root surfaces as the
// root's own error, and a non-regular file is ErrNotRegular with the actual mode
// named for diagnosis. There is no context parameter, matching
// OpenParentInRoot: an open is one syscall, and the caller that goes on to read
// passes its context there.
func OpenRegularInRoot(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	if root == nil {
		return nil, nil, errors.New("atomicfile: nil root")
	}
	clean, err := validateRootName(name)
	if err != nil {
		return nil, nil, err
	}

	// O_NONBLOCK has no effect on a regular file, the only thing this opens, and it
	// is what makes the FIFO case a rejection rather than a hang.
	f, err := root.OpenFile(clean, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := statRegular(f, clean)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// OpenRegular opens the absolute path read-only and returns the open descriptor
// together with the FileInfo of that descriptor, refusing a symlink at the final
// component and anything that is not a regular file. The caller owns the
// returned file and closes it.
//
// It is the ambient-path sibling of OpenRegularInRoot, for a caller holding a
// full path into a directory it trusts — its own config or state directory —
// rather than an *os.Root. The refusals differ because the mechanisms do:
// O_NOFOLLOW makes the KERNEL refuse a symlink under the name, reported as
// ErrSymlinkTarget so the read side speaks the write side's vocabulary, where
// the root-confined form instead lets an in-root symlink resolve (an *os.Root
// bounds escape but still traverses links inside its tree) and only bounds where
// it can land. Neither pins the ANCESTOR components: an ambient path resolves
// them as the filesystem presents them, and a root follows an in-root symlinked
// directory by design. A caller working in a tree others can write to descends
// with OpenParentInRoot first and opens the returned base through the pinned
// root.
//
// This is deliberately NOT how ReadBounded behaves, and does not change it:
// ReadBounded follows symlinks by design (os.Open resolves them) and stays that
// way, so a caller wanting the refusal asks for it here.
//
// Errors: ErrEmptyPath or ErrUnsafePath for a path that is empty, holds a NUL,
// or is not absolute once cleaned (the ValidatePath rule); ErrSymlinkTarget when
// the kernel refuses the open because the final component is a symlink, with the
// syscall error kept in the chain; ErrNotRegular for a directory, FIFO, device
// node or socket, naming the actual mode; and the filesystem's own error
// otherwise, so a missing file matches fs.ErrNotExist and an unreadable one
// fs.ErrPermission. ELOOP raised by a symlink loop in an ANCESTOR component is
// indistinguishable from the final-component refusal at the open boundary and
// takes the same arm; a caller that must tell the two apart reads the wrapped
// error.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	clean, err := validateAbsClean(path)
	if err != nil {
		return nil, nil, err
	}

	// O_NOFOLLOW refuses a final-component symlink at open time, which no
	// check-then-open sequence can do without a race; O_NONBLOCK is what makes a
	// FIFO left at the path an immediate rejection rather than an indefinite hang.
	f, err := os.OpenFile(clean, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, fmt.Errorf("%w: %s: %w", ErrSymlinkTarget, clean, err)
		}
		return nil, nil, err
	}
	fi, err := statRegular(f, clean)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, fi, nil
}

// statRegular stats the OPEN descriptor and refuses anything that is not a
// regular file. Stat the handle, not the path: checking the path and then
// opening it leaves a window in which the two refer to different files. The
// returned FileInfo therefore describes the very inode the caller is about to
// read, which is what makes it safe to record as a FileIdentity.
func statRegular(f *os.File, name string) (os.FileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s (type %s)", ErrNotRegular, name, fi.Mode().Type())
	}
	return fi, nil
}
