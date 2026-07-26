package atomicfile

import (
	"os"
	"time"
)

// FileIdentity records WHICH generation of a file a reader currently holds, so
// a cache can answer "is what I loaded still what is on disk?" from a stat
// alone, without re-reading the bytes.
//
// The correct form of that test is knowledge about this package's own write
// barrier, which is why it lives here. Two different write mechanisms can
// change a file's content, and each defeats one half of the naive check:
//
//   - An IN-PLACE writer (a plain os.WriteFile, an editor, a truncate-and-write)
//     keeps the same inode and moves the mtime forward. Comparing identity
//     alone would call that unchanged.
//   - A PUBLISH-BY-RENAME writer — every write in this package — installs a
//     DIFFERENT inode. Normally its mtime differs too, but it need not: a
//     backup restore, an rsync with -t, a tar extract with -p, or any
//     re-publication of an archived generation lands new content carrying the
//     OLD timestamp. Comparing mtime alone would call that unchanged, and the
//     stale copy would then be served until something else happened to touch
//     the file.
//
// So the test is mtime equality AND os.SameFile identity, and both legs are
// load-bearing. Size is not part of it: a same-size replacement is exactly the
// case a size comparison misses, and a differing size is already covered by
// one of the two legs.
//
// The zero value records nothing, which reports Changed — the fail direction a
// cache wants, since a spurious reload costs one read while a missed reload
// serves stale data indefinitely.
//
// FileIdentity retains the os.FileInfo it was given. It is a comparison
// primitive, not a policy: what to do about a changed file, whether a stat
// error is fatal, and when to re-stat at all stay with the caller.
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
// nothing was recorded. It is exposed for diagnostics — a log line or an
// assertion naming which generation is held — not for the freshness test, which
// Matches owns because the mtime alone is not sufficient.
func (id FileIdentity) ModTime() time.Time {
	if id.info == nil {
		return time.Time{}
	}
	return id.info.ModTime()
}
