// Package atomicfile provides crash-safe atomic file writes (temp, fsync,
// rename, directory fsync, all through an *os.Root), bounded reads,
// regular-file opens, writability probes and stale-temp sweeps, using only the
// standard library. A nil error from a write means the data reached its final
// path.
//
// A write without [WithMode] creates the file like [os.Create] (0666 through
// the umask and any inherited ACL) and never chmods or verifies it. WithMode
// stages the payload in an owner-only file and proves the mode the filesystem
// stored on the staging and final file, failing with [ErrModeNotStored]
// instead. https://github.com/cplieger/atomicfile documents every guarantee.
package atomicfile
