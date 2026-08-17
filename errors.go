package atomicfile

import "errors"

// Sentinel errors.
var (
	// ErrEmptyPath is returned when a path argument is empty.
	ErrEmptyPath = errors.New("atomicfile: empty path")
	// ErrUnsafePath is returned when a path fails the local safety check.
	ErrUnsafePath = errors.New("atomicfile: unsafe path")
	// ErrFileTooLarge is returned when a file exceeds the ReadBounded /
	// ReadBoundedFile size limit, or when content exceeds a WithMaxBytes
	// write cap (there it arrives wrapped, sometimes inside a *WriteError;
	// match with errors.Is).
	ErrFileTooLarge = errors.New("atomicfile: file too large")
	// ErrSymlinkTarget is returned when the target path is a symlink, which
	// every write entry point and OpenRegular refuse.
	ErrSymlinkTarget = errors.New("atomicfile: target is a symlink")
	// ErrNotRegular is returned by ReadBoundedInRoot when the name resolves to
	// something other than a regular file — a directory, named pipe, device node or
	// socket. It is a distinct sentinel because a caller that maps failures onto
	// protocol responses (an HTTP handler answering 400 for a directory but 404 for a
	// missing file) has to tell this case apart from the others; the error text names
	// the actual mode for diagnosis. Match with errors.Is.
	ErrNotRegular = errors.New("atomicfile: not a regular file")
	// ErrNotDirectory is returned by EnsurePrivateDir when the name is occupied by
	// something other than a directory — a plain file, named pipe, device node or
	// socket. It is a distinct sentinel for the reason ErrNotRegular is: a caller
	// deciding what to do next has to tell "somebody planted an object at my
	// directory's name" (fatal, and a signal worth an operator alert) apart from
	// "the directory is there but its mode or owner is wrong", and the error text
	// names the actual type for diagnosis. Match with errors.Is.
	ErrNotDirectory = errors.New("atomicfile: not a directory")
	// ErrNotOwned is returned by EnsurePrivateDir when the directory's owner is not
	// the effective uid, or when its ownership could not be determined at all. It is
	// distinct from a mode failure because the remedy is: a mode can be repaired by
	// whoever owns the path, while a foreign owner can rename or replace the
	// directory AFTER any check returns, so the only safe response is to abandon that
	// path rather than adjust it. Match with errors.Is.
	ErrNotOwned = errors.New("atomicfile: directory not owned by the effective uid")
	// ErrModeTooOpen is returned by EnsurePrivateDir when a PRE-EXISTING directory
	// grants group or other access. It is distinct from ErrModeNotStored because the
	// two say opposite things about who is at fault: this one means the directory is
	// somebody else's and is not private, which this package deliberately refuses to
	// chmod into compliance, while ErrModeNotStored means the filesystem would not
	// store the mode on a directory this process just created. Match with errors.Is.
	ErrModeTooOpen = errors.New("atomicfile: directory mode grants group or other access")
	// ErrModeNotStored is returned by EnforceMode when the mode read back from the
	// handle after the chmod is not the mode that was asked for — an inheritable ACL
	// widening the result, or any filesystem that treats a mode as advisory. It is a
	// distinct sentinel because it is the one failure in this package that is a
	// property of the MOUNT rather than of the path or the caller: retrying, or
	// picking a different name under the same parent, cannot fix it, so a caller that
	// maps failures onto operator guidance has to tell it apart from a permission
	// error. The error text names both the requested and the stored mode. Match with
	// errors.Is.
	ErrModeNotStored = errors.New("atomicfile: filesystem did not store the requested mode")
	// ErrAborted is returned by PendingFile.Commit when the pending file was
	// already aborted by a prior Cleanup. The temp file was removed and nothing
	// reached the final path, so Commit reports this rather than a zero-value
	// Result with a nil error, which would falsely signal success.
	ErrAborted = errors.New("atomicfile: pending file aborted")
)

// WritePhase identifies which step of an atomic write failed. Each value
// appears only on a WriteError, which is returned exclusively for hard failures
// (the data did not reach its final path). One outcome is deliberately absent
// from this enum because it is not a hard failure: a parent-directory fsync
// failure, which is surfaced via Result.Durable.
type WritePhase int

const (
	// PhaseTempCreate indicates failure opening the destination for writing:
	// opening the target's parent directory (the absolute-path entry points
	// open it as an *os.Root) or creating the temp file inside it.
	PhaseTempCreate WritePhase = iota + 1
	// PhaseTempWrite indicates failure writing to the temp file.
	PhaseTempWrite
	// PhaseTempChmod indicates failure setting permissions on the temp file.
	PhaseTempChmod
	// PhaseTempSync indicates failure syncing the temp file.
	PhaseTempSync
	// PhaseTempClose indicates failure closing the temp file.
	PhaseTempClose
	// PhaseRename indicates failure renaming temp to the final path.
	PhaseRename
)

func (p WritePhase) String() string {
	switch p {
	case PhaseTempCreate:
		return "create temp file"
	case PhaseTempWrite:
		return "write temp file"
	case PhaseTempChmod:
		return "chmod temp file"
	case PhaseTempSync:
		return "sync temp file"
	case PhaseTempClose:
		return "close temp file"
	case PhaseRename:
		return "rename to final path"
	default:
		return "unknown phase"
	}
}

// WriteError wraps a hard atomic-write failure with the phase that failed.
// A WriteError always means the data did NOT reach its final path.
type WriteError struct {
	Err   error
	Phase WritePhase
}

func (e *WriteError) Error() string { return "atomicfile: " + e.Phase.String() + ": " + e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }
