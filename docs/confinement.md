# Confinement, symlinks and special files

This page explains how atomicfile keeps reads, writes and removals inside a directory tree, and what it does with symlinks and special files. It is for a developer working in a directory that other users or processes can also write to.

## Writing inside a directory tree

Every write runs through an `*os.Root`. The `*InRoot` functions use the root you pass, and the absolute-path functions open the target's parent directory as a root and write through it. A symlink or `..` component in a name passed to an `*InRoot` function cannot reach outside the root's tree.

`ReadBoundedInRoot` is the read-side counterpart. It reads only a regular file, and its open does not block. A FIFO planted in the tree is refused with `ErrNotRegular` instead of stalling the caller. You keep ownership of the root and close it yourself.

## The path check is not containment

The package-level path check does not confine anything. `filepath.Clean` normalizes any `..` in an absolute path rather than rejecting it, so `ErrUnsafePath` only guards against a non-absolute or NUL-holding path.

To confine writes to a tree, use `WriteFileInRoot` or `WriteReaderInRoot`. To confine reads, use `ReadBoundedInRoot`. You can also open the file through an `*os.Root` and pass that handle to `ReadBoundedFile`, which applies the same size and context limits.

## Symlinks and special files on write

Every write refuses a target path that is currently a symlink, returning `ErrSymlinkTarget`, and there is no option to allow it. `os.Rename` replaces the symlink itself rather than the file it points to, which is rarely what the caller wants and can lose data. When the link's target is a path you already trust, resolve it with `filepath.EvalSymlinks` and write to the resolved path.

Writes also refuse a target occupied by anything else that is not a regular file, returning `ErrNotRegular`. `rename(2)` overwrites any non-directory, so without the check a write could replace a FIFO, a socket or a device node this package never created. An existing regular file is still overwritten, which is the point of the package.

The check reads the target before the rename, because `rename(2)` has no mode that replaces only a regular file. It refuses the states that are there to be found. It does not win a race against a writer that swaps the target in between.

## Symlinks on read

`ReadBounded` follows symlinks by design, because `os.Open` resolves them. When you read from a directory a less-trusted user can write to, use `ReadBoundedInRoot` instead.

Three openers return the open descriptor together with the `FileInfo` of that same descriptor. A caller streaming the file through a decoder or decryptor needs the descriptor. A caller caching the file needs the `FileInfo` the bytes came from to record a [`FileIdentity`](reload-staleness.md).

- `OpenRegularInRoot(root, name)` opens the name through the root, read-only and non-blocking, and reads the mode from the open handle rather than the pathname. It refuses a directory, FIFO, device node or socket with `ErrNotRegular`. It resolves an in-root symlink at the final component. `ReadBoundedInRoot` is exactly this plus `ReadBoundedFile`.
- `OpenRegularInRootNoFollow(root, name)` refuses a symlink at the final component with `ErrSymlinkTarget`. `os.Root` resolves in-root symlinks before it opens the last component, so `O_NOFOLLOW` has no effect through it. The refusal comes from `Root.Lstat` instead, and the descriptor is checked against that result. A name repointed in between returns `ErrRaced`.
- `OpenRegular(path)` takes a full path into a directory you trust. `O_NOFOLLOW` makes the kernel refuse a symlink at the final component with `ErrSymlinkTarget`, which no check-then-open sequence can do without a race. The path itself must pass `ValidatePath`.

You own the returned file and close it. None of the three pins the ancestor directories of the name. `OpenParentInRoot` does that.

## Pinning a name in a shared tree

An `*os.Root` confines a path but does not pin it. A root follows a symlink component that stays inside its tree. So a name of several components that is checked and then acted on can address two different files. That happens when an ancestor directory is swapped for a symlink in between.

Confinement still holds and nothing outside the root is reachable, but the operation can land somewhere inside the tree the caller never inspected. The functions below close that gap for a tree others can write to. Examples are a Docker volume mounted into several containers and a shared NFS export.

`OpenParentInRoot(root, name)` opens the parent directory of `name` as its own root, pinned. It checks every component with `Lstat`, refuses a symlink rather than following it, and requires a real directory. It opens each as a root and confirms it with `os.SameFile` against the directory it inspected, so a component replaced mid-open is refused too. Naming only the returned base through the returned parent then removes every ancestor from the operation's path.

The returned root is always a fresh handle you close. For a name directly under the root, it is a second handle on the root's own directory. So the close is unconditional and never closes your root. A vanished component matches `fs.ErrNotExist`, which is the harmless case of a racing deletion. A refused component does not.

`RemoveFileInRoot(root, name)` applies that descent to an unlink. It removes only a regular file. A directory, symlink, device node, socket or FIFO at the name is refused with `ErrNotRegular` and left as found. A missing file is reported as `fs.ErrNotExist` rather than swallowed, because whether an already-gone name counts as success is the caller's call.

## Walking a shared tree

`WalkDirInRoot(ctx, root, fn)` walks a confined tree and never descends into a symlinked directory. It reads each directory in batches of 256 entries rather than one full sorted listing. A large or hostile directory is never held in memory before the callback can refuse it. It keeps one directory handle open at a time and opens each one with `O_DIRECTORY`. A named pipe planted where a directory was is refused instead of blocking the walk.

The callback is an `fs.WalkDirFunc`, called the way `fs.WalkDir` calls one. The root comes first as `"."`, then each entry in pre-order. A directory that cannot be opened or finished is reported through the callback for its own path, and `fs.SkipDir` and `fs.SkipAll` work as in `fs.WalkDir`.

Two differences are deliberate. Entries arrive in directory order, sorted only within a batch. A directory's own entries are all visited before the walk descends into its subdirectories.

The walk stops between batches once `ctx` is done. A caller that wants to stop after any single entry checks `ctx` in the callback. To walk a subtree, pass `root.OpenRoot(sub)`.

`CleanupStaleTempsInRoot` is built on both functions. It lists the tree through `WalkDirInRoot` and unlinks each candidate through a pinned parent. An ancestor swapped for a symlink cannot redirect a removal at another file inside the tree. [Orphaned temp files](temp-files.md) describes the sweep.
