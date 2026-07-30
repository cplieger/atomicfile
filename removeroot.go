package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenParentInRoot opens the parent directory of name as its own *os.Root, pinned to
// the directory that was inspected, and returns that root together with name's final
// element.
//
// It exists because *os.Root confines a path but does not pin it. An os.Root
// deliberately FOLLOWS a symlink component that stays inside the root, so a
// multi-component name that is stat-ed and then operated on — the ordinary
// check-then-act any sweep, reaper or cache invalidator performs — can address two
// different files if an ANCESTOR DIRECTORY is swapped for a symlink in between. The
// confinement holds (nothing outside the root is reachable), and that is all it
// promises: inside the tree the operation lands somewhere else than the caller
// checked. Nothing in the os.Root surface closes that window, because there is no way
// to say "this component, and only the directory I already looked at".
//
// The descent is what closes it. Each component is Lstat-ed (so a symlink is REFUSED
// rather than followed), required to be a real directory, opened as its own root, and
// confirmed with os.SameFile against the directory that was inspected — so a component
// replaced while it was being opened is refused too. Naming only the returned base
// through the returned root then removes every ancestor from the operation's path: no
// in-root symlink can redirect it, and the open handle keeps the directory pinned even
// if it is renamed afterwards.
//
// Reach for it whenever two operations on one path have to address the same file and
// the tree is one other writers can modify — a co-mounted Docker volume, a shared NFS
// export, any directory whose contents are not exclusively yours. A single operation
// through the caller's own root needs none of this.
//
// The returned root is ALWAYS a fresh handle the caller must close, including for a
// name that sits directly under root (where it is a second handle on root's own
// directory), so the close is unconditional and never risks closing the caller's root.
// The caller owns root; OpenParentInRoot does not close it.
//
// Errors are the filesystem's own, wrapped: a component that has vanished matches
// fs.ErrNotExist (the benign racing-deletion case a caller usually treats as "already
// gone"), a component the process may not traverse matches fs.ErrPermission, and a
// component that is a symlink, a device node or a file — or one that changed identity
// mid-open — is refused with an error naming the component. name must be relative and
// must name an entry: "." and ".." are refused with ErrUnsafePath.
func OpenParentInRoot(root *os.Root, name string) (*os.Root, string, error) {
	if root == nil {
		return nil, "", errors.New("atomicfile: nil root")
	}
	clean, err := validateRootName(name)
	if err != nil {
		return nil, "", err
	}
	base := filepath.Base(clean)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return nil, "", fmt.Errorf("%w: names no entry: %q", ErrUnsafePath, name)
	}
	parent, err := pinDirInRoot(root, filepath.Dir(clean))
	if err != nil {
		return nil, "", err
	}
	return parent, base, nil
}

// RemoveFileInRoot removes the regular file at name inside root, unlinking it through a
// parent directory pinned by OpenParentInRoot so no ancestor component can redirect the
// removal at another file inside the tree.
//
// It is the removal-side counterpart to WriteFileInRoot: the same name, taken away
// again, with the same confinement and the ancestor-pinning os.Root itself does not
// provide (see OpenParentInRoot for why a plain root.Remove of a multi-component name
// is not the same operation).
//
// Only a regular file is removed. A directory, symlink, device node, socket or named
// pipe wearing the name is refused with ErrNotRegular and left exactly as found, so a
// caller sweeping names it believes it wrote cannot be tricked into unlinking something
// it did not — the same rule ReadBoundedInRoot applies to what it will read. Removing a
// symlink itself, or an empty directory, is deliberately not this function's job; use
// the pinned root from OpenParentInRoot for that.
//
// A missing file is reported (matching fs.ErrNotExist) rather than swallowed: whether a
// name that is already gone is success or a surprise is the caller's judgement.
//
// The caller owns root; RemoveFileInRoot does not close it.
func RemoveFileInRoot(root *os.Root, name string) error {
	parent, base, err := OpenParentInRoot(root, name)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()

	// Lstat, not Stat: a symlink under the name must be reported as what it is, never
	// resolved to the regular file it points at.
	fi, err := parent.Lstat(base)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s (type %s)", ErrNotRegular, name, fi.Mode().Type())
	}
	return parent.Remove(base)
}

// pinDirInRoot descends dir component by component from root and returns the innermost
// directory as its own root, with every component confirmed to be the same directory
// that was inspected. dir is a cleaned, relative path, "." for root itself.
func pinDirInRoot(root *os.Root, dir string) (*os.Root, error) {
	// A fresh handle even for ".", so the returned root is unconditionally the
	// caller's to close. No identity check is needed or possible here: openat(fd, ".")
	// resolves no name, so it cannot be redirected — the caller's own open handle IS
	// the pin.
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, fmt.Errorf("atomicfile: open root: %w", err)
	}
	if dir == "." {
		return current, nil
	}
	for component := range strings.SplitSeq(dir, string(filepath.Separator)) {
		next, pinErr := pinChildInRoot(current, component)
		// Closed unconditionally: the descent holds exactly one handle at a time, and
		// the pin of a child does not depend on its parent staying open.
		_ = current.Close()
		if pinErr != nil {
			return nil, pinErr
		}
		current = next
	}
	return current, nil
}

// pinChildInRoot opens one directory component of parent as its own root, refusing
// anything that is not a real directory and anything whose identity changed between the
// Lstat and the open. Split out of pinDirInRoot so the descent reads as one step per
// component rather than carrying four failure arms inline.
func pinChildInRoot(parent *os.Root, name string) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: inspect path component %q: %w", name, err)
	}
	if !fi.IsDir() {
		// A symlink lands here too, which is the point: os.Root would follow it.
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
