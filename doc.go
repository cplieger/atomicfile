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
// # Confinement
//
// Every write runs through an *os.Root: the *InRoot functions use the
// caller's root directly, and the absolute-path functions open the target's
// parent directory as a root and write through it. The absolute-path surface
// is therefore a thin adapter over one confined write engine.
package atomicfile
