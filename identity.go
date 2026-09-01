package atomicfile

import (
	"os"
	"time"
)

// FileIdentity records WHICH generation of a file a reader currently holds, so
// a cache can answer "is what I loaded still what is on disk?" from a stat
// alone, without re-reading the bytes.
//
// Matches compares mtime equality AND os.SameFile identity, and both legs are
// load-bearing: an IN-PLACE writer keeps the same inode but may not advance
// the mtime (inode times come from a coarse clock tick, so two writes inside
// one tick can share an mtime), while a PUBLISH-BY-RENAME writer (every write
// in this package) installs a different inode but can carry an OLD timestamp
// forward (a backup restore, rsync -t). Neither leg alone catches both cases.
// Size is a third leg this type does not hold: it is what catches an
// in-place rewrite that changes the file's LENGTH without advancing its
// mtime; a caller whose file may be rewritten in place should keep its own
// size comparison beside Changed.
//
// The zero value records nothing, which reports Changed — the fail direction
// a cache wants, since a spurious reload costs one read while a missed
// reload serves stale data indefinitely.
//
// FileIdentity is a comparison primitive, not a policy: what to do about a
// changed file, and when to re-stat, stay with the caller.
type FileIdentity struct {
	info os.FileInfo
}

// Identify captures the identity of the file described by info, as returned by
// os.Stat, os.Lstat, or (*os.File).Stat. A nil info records nothing, so the
// result reports Changed against every later stat.
func Identify(info os.FileInfo) FileIdentity {
	return FileIdentity{info: info}
}

// Recorded reports whether an identity was captured at all. A zero FileIdentity
// (nothing loaded yet, or a load that failed before its stat) records nothing.
func (id FileIdentity) Recorded() bool { return id.info != nil }

// Matches reports whether info describes the same file generation this identity
// captured: an equal modification time AND the same file (os.SameFile, which
// compares the underlying device and inode). An unrecorded identity matches
// nothing, and a nil info — no file observed — matches nothing.
func (id FileIdentity) Matches(info os.FileInfo) bool {
	if id.info == nil || info == nil {
		return false
	}
	return info.ModTime().Equal(id.info.ModTime()) && os.SameFile(info, id.info)
}

// Changed is the negation of Matches, for the reload-side reading: it reports
// whether info is a file generation this identity did not capture, and so
// whether a reader holding the captured generation must load again.
func (id FileIdentity) Changed(info os.FileInfo) bool { return !id.Matches(info) }

// ModTime returns the captured modification time, or the zero Time when
// nothing was recorded. For diagnostics only; Matches owns the freshness
// test, since mtime alone is not sufficient.
func (id FileIdentity) ModTime() time.Time {
	if id.info == nil {
		return time.Time{}
	}
	return id.info.ModTime()
}
