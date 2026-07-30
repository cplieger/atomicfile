package atomicfile

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePath checks path against the rule this package's absolute-path entry
// points apply — WriteFile, WriteReader, NewPendingFile, ReadBounded —
// returning nil when the path is acceptable to them, and otherwise the very
// error they would return: ErrEmptyPath for an empty path, ErrUnsafePath for a
// path holding a NUL byte or one that is not absolute once cleaned. It is the
// exported face of validateAbsClean, which stays the single implementation, so
// an acceptance decided here and the rejection a later write applies cannot
// drift apart.
//
// It exists for the acceptance check that happens long before the write — a
// config path validated at startup, a CLI flag refused with a usage message, a
// request field answered with a 400. filepath.IsAbs is the usual stand-in and
// is NOT the same rule: it accepts a path with an embedded NUL ("/tmp/a\x00b"
// is absolute), which every write here refuses, so a caller validating with
// IsAbs admits input its own write path rejects later, at a point far from
// where the value came in. It also answers a bool, leaving the caller to
// invent the two reasons this package already distinguishes.
//
// Nothing on the filesystem is consulted: no stat, no open, no temp file. The
// path need not exist, its parent need not be a directory, and the call is
// cheap enough for a hot loop. That also bounds what nil means — the path is
// well formed, NOT that a write there will succeed. ProbeWritable answers that
// second question, and answers it by actually writing.
//
// Being absolute is not containment. filepath.Clean normalizes ".." in an
// absolute path, so nil says nothing about which subtree the path lands in; a
// caller confining writes to a directory tree uses the *os.Root-backed APIs
// (WriteFileInRoot / WriteReaderInRoot).
func ValidatePath(path string) error {
	_, err := validateAbsClean(path)
	return err
}

// validateAbsClean requires path to be non-empty and null-byte-free and to
// clean to an absolute path, returning the cleaned form. filepath.Clean
// normalizes any ".." in an absolute path (there is nothing to escape), so
// this is not a containment boundary: callers that need writes confined to a
// directory tree use the *os.Root-backed APIs (WriteFileInRoot /
// WriteReaderInRoot), which enforce containment. The absolute-path write
// functions do, however, run every filesystem operation through an *os.Root
// of the target's parent directory (see openParentRoot), so the final
// component cannot be swapped for an escaping symlink mid-write.
func validateAbsClean(path string) (string, error) {
	if path == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: contains null byte", ErrUnsafePath)
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: not absolute: %q", ErrUnsafePath, path)
	}
	return clean, nil
}
