package atomicfile

import "errors"

// Sentinel errors.
var (
	// ErrEmptyPath is returned when a path argument is empty.
	ErrEmptyPath = errors.New("atomicfile: empty path")
	// ErrUnsafePath is returned when a path fails the local safety check.
	ErrUnsafePath = errors.New("atomicfile: unsafe path")
	// ErrFileTooLarge is returned when a file exceeds the read size limit.
	ErrFileTooLarge = errors.New("atomicfile: file too large")
	// ErrSymlinkTarget is returned when the target path is a symlink and
	// WithAllowSymlinkTarget was not set.
	ErrSymlinkTarget = errors.New("atomicfile: target is a symlink")
)

// WritePhase identifies which step of an atomic write failed. Each value
// appears only on a WriteError, which is returned exclusively for hard failures
// (the data did not reach its final path). Two outcomes are deliberately absent
// from this enum because they are not hard failures: a parent-directory fsync
// failure (surfaced via Result.Durable) and a preserve-ownership chown failure
// (best-effort, logged at Warn).
type WritePhase int

const (
	// PhaseTempCreate indicates failure creating the temp file.
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
