package atomicfile

import "errors"

var (
	// ErrEmptyPath is returned when a path argument is empty.
	ErrEmptyPath = errors.New("atomicfile: empty path")
	// ErrUnsafePath is returned when a path fails the local safety check.
	ErrUnsafePath = errors.New("atomicfile: unsafe path")
	// ErrFileTooLarge is returned when a file exceeds the ReadBounded /
	// ReadBoundedFile size limit, or content exceeds a WithMaxBytes write cap
	// (there it may arrive wrapped, sometimes inside a *WriteError; match with
	// errors.Is).
	ErrFileTooLarge = errors.New("atomicfile: file too large")
	// ErrSymlinkTarget is returned when the target path is a symlink, which
	// every write entry point and OpenRegular refuse.
	ErrSymlinkTarget = errors.New("atomicfile: target is a symlink")
	// ErrNotRegular is returned when a name resolves to something other than a
	// regular file. ReadBoundedInRoot refuses to read it, RemoveFileInRoot
	// refuses to unlink it, and every write entry point refuses to publish over
	// it. Match with errors.Is.
	ErrNotRegular = errors.New("atomicfile: not a regular file")
	// ErrNotDirectory is returned by EnsurePrivateDir when the name is occupied
	// by something other than a directory. Match with errors.Is.
	ErrNotDirectory = errors.New("atomicfile: not a directory")
	// ErrNotOwned is returned by EnsurePrivateDir when the directory's owner is
	// not the effective uid, or ownership could not be determined. Match with
	// errors.Is.
	ErrNotOwned = errors.New("atomicfile: directory not owned by the effective uid")
	// ErrModeTooOpen is returned by EnsurePrivateDir when a pre-existing
	// directory grants group or other access; it is deliberately not repaired
	// unless WithRepairOwnedDir is set. Match with errors.Is.
	ErrModeTooOpen = errors.New("atomicfile: directory mode grants group or other access")
	// ErrModeNotStored is returned by EnforceMode when the mode read back from
	// the handle after chmod does not match what was requested — a mount
	// property, not a permission error. Match with errors.Is.
	ErrModeNotStored = errors.New("atomicfile: filesystem did not store the requested mode")
	// ErrAborted is returned by PendingFile.Commit when the pending file was
	// already aborted by a prior Cleanup.
	ErrAborted = errors.New("atomicfile: pending file aborted")
)

// WritePhase identifies which step of an atomic write failed. Each value
// appears only on a WriteError. A parent-directory fsync failure is not in
// this enum; it is surfaced via Result.Durable instead.
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
