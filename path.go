package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// validateAbsClean checks that path is non-empty, free of null bytes and ".."
// traversal, and absolute, returning the cleaned form.
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
	if strings.Contains(clean, ".."+string(filepath.Separator)) ||
		strings.HasSuffix(clean, string(filepath.Separator)+"..") ||
		clean == ".." {
		return "", fmt.Errorf("%w: contains traversal: %q", ErrUnsafePath, path)
	}
	return clean, nil
}

// checkSymlink returns ErrSymlinkTarget if target is a symlink and
// WithAllowSymlinkTarget was not set. Best-effort: the Lstat precedes the
// eventual os.Rename, so an attacker who can write the parent directory may
// create a symlink afterward. Because os.Rename does not follow the final
// component, the worst case is replacing the attacker's symlink rather than
// writing through it.
func checkSymlink(target string, c *cfg) error {
	if c.allowSymlinkTarget {
		return nil
	}
	fi, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("atomicfile: stat target %q: %w", target, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkTarget, target)
	}
	return nil
}

// checkWritePath runs the shared pre-write path-safety preamble: validate and
// clean path, honor ctx cancellation, and refuse symlink targets. It is the
// single source of truth for the write-side path-safety contract.
func checkWritePath(ctx context.Context, path string, c *cfg) (string, error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return "", err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("atomicfile: %w", ctxErr)
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return "", symErr
	}
	return cleanPath, nil
}
