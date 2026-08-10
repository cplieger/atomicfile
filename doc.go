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
// # Path validation
//
// ValidatePath is the acceptance check the absolute-path entry points apply,
// exported for callers that have to accept or refuse a path long before they
// write to it — a config value read at startup, a CLI flag, a request field.
// filepath.IsAbs is the usual stand-in and admits a path with an embedded NUL
// that every write here refuses, so validating with it accepts input the write
// path rejects later. The exported gate delegates to the same private validator
// the writes use, so the two verdicts cannot drift; it touches nothing on disk,
// which also means a nil answer says the path is well formed, not that a write
// will succeed (that is ProbeWritable's question).
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
// # Private directories and enforced modes
//
// A mode passed to mkdir(2) or open(2) is a REQUEST. umask narrows it, and a
// filesystem carrying an inheritable ACL can widen the result regardless of what
// was asked — measured on a ZFS nfs4acl dataset, an inheritable group@:rwx ACE
// stores 0770 for a 0o700 mkdir, and tightening the parent does not cover it.
// chmod(2) is the only call that SETS a mode, and a caller that stops there has
// issued a second request rather than confirmed a result.
//
// EnforceMode closes that loop on an OPEN HANDLE: fchmod then fstat the same
// descriptor, returning the mode the filesystem actually stored and refusing a
// mismatch with ErrModeNotStored. It takes an *os.File, so a file and a directory
// are the same call, and it observes no pathname — a chmod-then-stat by name can
// chmod one object and certify another.
//
// The write path uses it on itself, in two places. The staging file is created
// owner-only and PROVED owner-only before the caller's payload goes into it —
// the temp has to share the target's parent directory, so on a widening
// filesystem it would otherwise hold the payload group-readable and
// group-writable for the whole write. Then WithMode's mode is enforced rather
// than merely requested before the rename publishes it, so a filesystem that
// refuses the mode fails the write instead of publishing a wider file.
//
// EnsurePrivateDir is the custody composition around it, for a process
// establishing a directory only its own user may enter inside a parent others can
// write: mkdir 0700 recording whether THIS call created it, open
// O_DIRECTORY|O_NOFOLLOW so a planted symlink is refused rather than followed and
// a planted FIFO cannot stall the open, fstat the handle, require the effective
// uid to own it, then EnforceMode a directory it created (the ACL repair, safe
// because nobody else ever held that name) while REFUSING a pre-existing one that
// grants group or other access (repairing another principal's directory would take
// over their name). One level per call: a multi-level path is the caller's loop,
// because only the caller knows which levels are its own.
//
// The verdict is POINT-IN-TIME. It proves what was true of one inode while a
// handle was open on it; it does not pin the directory for later use, and a caller
// that goes on to ReadDir or Remove through the PATHNAME re-opens the window the
// check closed. Acting through a retained handle (an *os.Root) is what would close
// that, and is a different API shape.
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
