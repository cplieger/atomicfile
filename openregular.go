package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// OpenRegularInRoot opens name inside root read-only and returns the open
// descriptor together with the FileInfo of that same descriptor, having
// already refused anything that is not a regular file. The caller owns the
// returned file and closes it; root stays the caller's too.
//
// It is the open half of ReadBoundedInRoot, which is built on it: open
// through the root (so a symlink or ".." in name cannot escape its tree),
// non-blocking so a planted FIFO cannot stall the caller in open(2), then
// stat the OPEN HANDLE — never the pathname again — refusing a directory,
// FIFO, device node or socket with ErrNotRegular. Stating the handle rather
// than the path closes the window where a rename between a check and an
// open would let a caller decode one generation's bytes while recording
// another's identity as a FileIdentity.
//
// The descriptor lets a caller stream the file through a decoder or
// decryptor without materializing it, which a []byte-returning helper
// cannot serve. For the bytes, hand the descriptor to ReadBoundedFile — that
// composition IS ReadBoundedInRoot. OpenRegular is the ambient-path sibling.
//
// Errors are the filesystem's own except where this package adds a verdict:
// name must be relative and null-byte-free (ErrEmptyPath / ErrUnsafePath), a
// missing file or an escaping symlink surfaces as the root's own error, and
// a non-regular file is ErrNotRegular naming the actual mode. No context
// parameter (matching OpenParentInRoot): an open is one syscall, and a
// caller that goes on to read passes its context there.
func OpenRegularInRoot(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	if root == nil {
		return nil, nil, errors.New("atomicfile: nil root")
	}
	clean, err := validateRootName(name)
	if err != nil {
		return nil, nil, err
	}

	// O_NONBLOCK makes an open on a planted FIFO a rejection rather than a hang.
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

// OpenRegular opens the absolute path read-only and returns the open
// descriptor together with the FileInfo of that same descriptor, refusing a
// symlink at the final component and anything that is not a regular file.
// The caller owns the returned file and closes it.
//
// It is the ambient-path sibling of OpenRegularInRoot, for a caller holding
// a full path into a directory it trusts rather than an *os.Root. The
// refusals differ because the mechanisms do: O_NOFOLLOW makes the KERNEL
// refuse a final-component symlink (reported as ErrSymlinkTarget), where the
// root-confined form lets an in-root symlink resolve and only bounds where
// it can land. Neither pins the ANCESTOR components.
//
// This does not change ReadBounded, which follows symlinks by design; a
// caller wanting the refusal asks for it here.
//
// Errors: ErrEmptyPath or ErrUnsafePath for an empty, NUL-holding, or
// non-absolute path; ErrSymlinkTarget when the kernel refuses the open
// because the final component is a symlink; ErrNotRegular for a directory,
// FIFO, device node or socket, naming the actual mode; the filesystem's own
// error otherwise (a missing file matches fs.ErrNotExist). ELOOP from a
// symlink loop in an ANCESTOR component takes the same ErrSymlinkTarget arm
// as the final-component refusal; a caller that must tell them apart reads
// the wrapped error.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	clean, err := validateAbsClean(path)
	if err != nil {
		return nil, nil, err
	}

	// O_NOFOLLOW refuses a final-component symlink without a check-then-open race;
	// O_NONBLOCK turns a FIFO at the path into a rejection rather than a hang.
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

// statRegular stats the OPEN descriptor, not the path (a path-then-open
// sequence leaves a window where the two refer to different files), and
// refuses anything that is not a regular file.
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
