// Package atomicfile provides crash-safe atomic file writes via the
// temp → fsync → rename → dir-fsync sequence, with path validation and
// bounded reads. Standard-library only.
//
// # Result and durability
//
// The write primitives (WriteFile, WriteReader, WriteFileInRoot,
// WriteReaderInRoot, and PendingFile.Commit)
// return a Result alongside an error. A nil error means the data reached its
// final path; the write either fully succeeded or, at worst, was renamed into
// place but the parent-directory fsync failed. Result.Durable distinguishes
// those two outcomes: it is true only when both the file and its parent
// directory were fsynced, so a caller that cares about crash durability
// inspects Result.Durable rather than decoding error types. A non-nil error
// always means the data did NOT reach its final path.
//
// # Temp files
//
// Every temp file this package creates is named ".atomicfile-<digits>.tmp"
// (the digits are a crypto/rand decimal string). CleanupStaleTemps reclaims
// orphaned temps of exactly that shape and nothing else, so it never deletes
// a caller-owned file.
//
// That shape is part of the API, not an implementation detail, because a
// caller staging its own file in a swept directory has to agree with it:
// TempName produces a name the sweeps reclaim and IsPackageTemp reports
// whether a name is one, so the agreement is a contract a test can pin rather
// than a convention rebuilt from os.CreateTemp's undocumented substitution.
//
// # Writability probes
//
// ProbeWritable and ProbeWritableInRoot answer "can this process actually
// write here?" — the preflight question a stat of the mode bits gets wrong on
// an NFS or FUSE mount, a read-only bind mount, or a Docker volume owned by
// another UID. They walk the full ladder a real write walks (mkdir, create,
// write, sync, close, remove) and report which stage failed in a ProbeResult
// rather than as an error, so a caller that warns on a failed cleanup and one
// that treats it as fatal both get what they need. The probe file carries the
// package temp shape, so a probe leaked by a crash is swept like any other
// orphan.
//
// # Size bounds
//
// ReadBounded and ReadBoundedFile refuse to read a file past a byte limit,
// and WithMaxBytes mirrors that bound on the write side across every write
// primitive: a writer that also owns the reader can refuse to persist a file
// its own read path would refuse to load, instead of failing open on the
// next read. The cap is enforced twice: on the staged stream as bytes
// arrive, and against the staged file's actual size at the durability
// barrier before publication, so content staged outside the streaming
// interfaces cannot publish either. Both directions report ErrFileTooLarge,
// and a capped write always leaves the previous file at the target path
// intact.
//
// # Safe opens
//
// OpenRegularInRoot and OpenRegular hand back the OPEN DESCRIPTOR of a file
// already known to be a regular one, which is the reusable half of the read
// sequence: open (confined to a root, or refusing a final-component symlink
// outright), stat the handle rather than the pathname, refuse a directory,
// FIFO, device node or socket. ReadBoundedInRoot is now literally
// OpenRegularInRoot plus ReadBoundedFile, so the sequence has one home.
//
// A caller that STREAMS the file through a decoder or decryptor never
// materializes its bytes, and a caller caching the file needs the FileInfo the
// bytes came from in order to record a FileIdentity; neither can use a
// []byte-returning helper. Both are the same argument: binding the shape check,
// the identity and the read to ONE descriptor is what closes the window a
// second pathname observation opens, where a rename in between lets a caller
// decode one generation's bytes while recording another's identity and turns
// every non-regular-file rejection back into a check-then-open race.
//
// The two refusals differ because the mechanisms do. The root-confined form
// bounds where the open can land but still traverses a symlink inside the tree
// (that is what an *os.Root promises); the ambient form opens O_NOFOLLOW, so
// the kernel itself refuses a symlink under the name and reports
// ErrSymlinkTarget, the same vocabulary the write side uses. Neither pins the
// ancestor components — OpenParentInRoot does that.
//
// # Reload staleness
//
// FileIdentity answers "is what I loaded still what is on disk?" from a stat
// alone, for a reader caching a file another process publishes. The correct
// form of that test is knowledge about this package's write barrier: mtime
// equality AND os.SameFile identity, because an in-place writer keeps the
// inode while moving the mtime, and a publish-by-rename installs a different
// inode that a restore can hand the OLD timestamp. Either leg alone serves
// stale content for one of the two write mechanisms; a size comparison misses
// exactly the same-size replacement. It is a comparison primitive, not a
// policy — degradation states, stat-error handling, and re-stat cadence stay
// with the caller.
//
// # Confinement
//
// Every write runs through an *os.Root: the *InRoot functions use the
// caller's root directly, and the absolute-path functions open the target's
// parent directory as a root and write through it. The absolute-path surface
// is therefore a thin adapter over one confined write engine.
//
// # Confined traversal and removal
//
// An *os.Root confines a path but does not PIN it: a root deliberately follows
// a symlink component that stays inside its tree, so a multi-component name
// that is stat-ed and then operated on can address two different files if an
// ancestor directory is swapped for a symlink in between. Confinement still
// holds — nothing outside the root is reachable — and inside the tree the
// operation lands somewhere the caller never inspected.
//
// OpenParentInRoot closes that window: it descends component by component,
// refusing a symlink and confirming each directory's identity with
// os.SameFile, and hands back the leaf's own pinned root plus its base name,
// so the operation that follows names no ancestor at all. RemoveFileInRoot is
// that descent applied to an unlink, and CleanupStaleTempsInRoot now reclaims
// every temp through it.
//
// WalkDirInRoot is the traversal counterpart to WriteFileInRoot and
// ReadBoundedInRoot, for a caller enumerating a tree it does not own: entries
// stream in fixed ReadDir batches rather than one materialized (and sorted)
// inventory per directory, exactly one directory handle is open at a time, each
// directory is opened O_DIRECTORY so a planted FIFO is refused instead of
// blocking the caller's goroutine, and a symlinked directory is never
// descended. The callback is an fs.WalkDirFunc and the fs.SkipDir/fs.SkipAll
// sentinels behave as they do in fs.WalkDir; the two deliberate differences
// are that entries arrive in directory order (sorted within a batch only) and
// that a directory's own entries are all visited before the walk descends.
package atomicfile
