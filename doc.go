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
package atomicfile
