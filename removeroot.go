package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenParentInRoot opens the parent directory of name as its own *os.Root,
// pinned to the directory that was inspected, and returns that root
// together with name's final element.
//
// *os.Root confines a path but does not pin it: it FOLLOWS an in-root
// symlink component, so a multi-component name that is stat-ed and then
// operated on can address two different files if an ancestor directory is
// swapped for a symlink in between. The descent here closes that window:
// each component is Lstat-ed (a symlink is refused, not followed), required
// to be a real directory, opened as its own root, and confirmed with
// os.SameFile against what was inspected. Naming only the returned base
// through the returned root then removes every ancestor from the
// operation's path.
//
// Reach for it whenever two operations on one path must address the same
// file and the tree is one other writers can modify. A single operation
// through the caller's own root needs none of this.
//
// The returned root is ALWAYS a fresh handle the caller must close, even
// for a name directly under root, so the close never risks closing the
// caller's root. The caller owns root; OpenParentInRoot does not close it.
//
// A vanished component matches fs.ErrNotExist, a denied one matches
// fs.ErrPermission, and a component that is not a real directory — or
// changed identity mid-open — is refused naming the component. name must
// be relative and name an entry: "." and ".." are refused with
// ErrUnsafePath.
func OpenParentInRoot(root *os.Root, name string) (*os.Root, string, error) {
	if root == nil {
		return nil, "", errors.New("atomicfile: nil root")
	}
	clean, err := validateRootEntry(name)
	if err != nil {
		return nil, "", err
	}
	parent, err := pinDirInRoot(root, filepath.Dir(clean))
	if err != nil {
		return nil, "", err
	}
	return parent, filepath.Base(clean), nil
}

// RemoveFileInRoot removes the regular file at name inside root, unlinking
// it through a parent directory pinned by OpenParentInRoot so no ancestor
// component can redirect the removal at another file. It is the removal
// counterpart to WriteFileInRoot.
//
// Only a regular file is removed. A directory, symlink, device node, socket
// or named pipe wearing the name is refused with ErrNotRegular and left as
// found — the same rule ReadBoundedInRoot applies to what it reads.
// Removing a symlink itself, or an empty directory, is not this function's
// job; use the pinned root from OpenParentInRoot for that.
//
// A missing file is reported (matching fs.ErrNotExist) rather than
// swallowed. The caller owns root; RemoveFileInRoot does not close it.
func RemoveFileInRoot(root *os.Root, name string) error {
	parent, base, err := OpenParentInRoot(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()

	// Lstat, not Stat: a symlink must be reported as itself, never resolved.
	fi, err := parent.Lstat(base)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s (type %s)", ErrNotRegular, name, fi.Mode().Type())
	}
	return parent.Remove(base)
}

// pinDirInRoot descends dir component by component from root and returns
// the innermost directory as its own root, each component confirmed to be
// the same directory that was inspected. dir is cleaned and relative, "."
// for root itself.
func pinDirInRoot(root *os.Root, dir string) (*os.Root, error) {
	// A fresh handle even for ".": openat(fd, ".") resolves no name, so it
	// cannot be redirected — the open handle IS the pin.
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, fmt.Errorf("atomicfile: open root: %w", err)
	}
	if dir == "." {
		return current, nil
	}
	for component := range strings.SplitSeq(dir, string(filepath.Separator)) {
		next, pinErr := pinChildInRoot(current, component)
		// Closed unconditionally: the descent holds one handle at a time.
		_ = current.Close()
		if pinErr != nil {
			return nil, pinErr
		}
		current = next
	}
	return current, nil
}

// pinChildInRoot opens one directory component of parent as its own root,
// refusing anything that is not a real directory and anything whose
// identity changed between the Lstat and the open.
func pinChildInRoot(parent *os.Root, name string) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: inspect path component %q: %w", name, err)
	}
	if !fi.IsDir() {
		// A symlink lands here too: os.Root would otherwise follow it.
		return nil, fmt.Errorf("atomicfile: path component %q is not a directory (mode %s)",
			name, fi.Mode().String())
	}
	next, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: open path component %q: %w", name, err)
	}
	opened, err := next.Stat(".")
	if err != nil {
		_ = next.Close()
		return nil, fmt.Errorf("atomicfile: confirm path component %q: %w", name, err)
	}
	if !os.SameFile(fi, opened) {
		_ = next.Close()
		return nil, fmt.Errorf("atomicfile: path component %q changed while it was being opened", name)
	}
	return next, nil
}
