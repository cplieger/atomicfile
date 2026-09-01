package atomicfile

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePath checks path against the rule this package's absolute-path
// entry points apply (WriteFile, WriteReader, NewPendingFile, ReadBounded),
// returning nil when acceptable and otherwise ErrEmptyPath or ErrUnsafePath —
// the exported face of validateAbsClean, so acceptance here and a later
// write's rejection cannot drift apart.
//
// Unlike filepath.IsAbs, this also rejects an embedded NUL byte, matching
// what every write here refuses. Nothing on the filesystem is consulted: nil
// means the path is well formed, not that a write there will succeed
// (ProbeWritable answers that by actually writing).
//
// Being absolute is not containment: filepath.Clean normalizes ".." in an
// absolute path, so nil says nothing about which subtree the path lands in.
// A caller confining writes to a directory tree uses the *os.Root-backed
// APIs (WriteFileInRoot / WriteReaderInRoot).
func ValidatePath(path string) error {
	_, err := validateAbsClean(path)
	return err
}

// validateAbsClean requires path to be non-empty and null-byte-free and to
// clean to an absolute path, returning the cleaned form. This is not a
// containment boundary (filepath.Clean normalizes ".." in an absolute path);
// callers needing writes confined to a directory tree use the
// *os.Root-backed APIs (WriteFileInRoot / WriteReaderInRoot).
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
