package atomicfile

import (
	"fmt"
	"path/filepath"
	"strings"
)

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
