package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// statOrFail stats path, failing the test on error.
func statOrFail(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}

// TestFileIdentityZeroValueRecordsNothing pins the fail direction: an identity
// that captured nothing reports Changed against everything, so a cache holding
// no generation always loads rather than skipping on an empty comparison.
func TestFileIdentityZeroValueRecordsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info := statOrFail(t, path)

	var zero FileIdentity
	if zero.Recorded() {
		t.Error("zero FileIdentity.Recorded() = true, want false")
	}
	if zero.Matches(info) {
		t.Error("zero FileIdentity.Matches(info) = true, want false")
	}
	if !zero.Changed(info) {
		t.Error("zero FileIdentity.Changed(info) = false, want true")
	}
	if !zero.ModTime().IsZero() {
		t.Errorf("zero FileIdentity.ModTime() = %v, want the zero Time", zero.ModTime())
	}
	if Identify(nil).Recorded() {
		t.Error("Identify(nil).Recorded() = true, want false")
	}
}

// TestFileIdentityNilInfoNeverMatches pins that a nil stat result — no file
// observed — matches nothing, so the predicate stays total instead of panicking
// inside os.SameFile.
func TestFileIdentityNilInfoNeverMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := Identify(statOrFail(t, path))

	if id.Matches(nil) {
		t.Error("Matches(nil) = true, want false")
	}
	if !id.Changed(nil) {
		t.Error("Changed(nil) = false, want true")
	}
}

// TestFileIdentityMatchesUntouchedFile pins the skip case the whole primitive
// exists to enable: re-stating a file nothing has written must report unchanged,
// so a cache does not re-read on every poll.
func TestFileIdentityMatchesUntouchedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := Identify(statOrFail(t, path))

	if !id.Matches(statOrFail(t, path)) {
		t.Error("Matches(re-stat of an untouched file) = false, want true")
	}
	if id.Changed(statOrFail(t, path)) {
		t.Error("Changed(re-stat of an untouched file) = true, want false")
	}
	if !id.Recorded() {
		t.Error("Recorded() = false after Identify of a real file")
	}
}

// TestFileIdentityDetectsInPlaceRewrite pins the leg an identity-only check
// misses: an in-place writer keeps the inode and moves the mtime, so
// os.SameFile still holds and only the timestamp comparison catches it.
func TestFileIdentityDetectsInPlaceRewrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := statOrFail(t, path)
	id := Identify(before)

	// Rewrite through the same inode, then force a distinct mtime so the test
	// does not depend on filesystem timestamp granularity.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.WriteString("second"); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	newMod := before.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(path, newMod, newMod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	after := statOrFail(t, path)
	if !os.SameFile(before, after) {
		t.Fatal("the in-place rewrite changed the inode; the test no longer exercises the mtime leg")
	}
	if id.Matches(after) {
		t.Error("Matches(in-place rewrite) = true, want false: the mtime leg did not fire")
	}
}

// TestFileIdentityDetectsTimestampPreservingReplacement pins the leg an
// mtime-only check misses, and the one this package's own write barrier makes
// reachable: publish-by-rename installs a different inode, and a restore can
// carry the OLD timestamp with it, so only os.SameFile catches the swap. This is
// the exact case a mtime+size comparison serves stale.
func TestFileIdentityDetectsTimestampPreservingReplacement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("aaaaa"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := statOrFail(t, path)
	id := Identify(before)

	// Publish different content of the SAME SIZE through this package's own
	// rename barrier, then restore the original timestamp — a backup restore
	// or an rsync -t of an archived generation.
	if _, err := WriteFile(t.Context(), path, []byte("bbbbb")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	after := statOrFail(t, path)
	if os.SameFile(before, after) {
		t.Fatal("the rename did not change the inode; the test no longer exercises the identity leg")
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("the replacement changed mtime or size; the test no longer isolates the identity leg")
	}
	if id.Matches(after) {
		t.Error("Matches(timestamp-preserving replacement) = true, want false: the identity leg did not fire")
	}
	if !id.Changed(after) {
		t.Error("Changed(timestamp-preserving replacement) = false, want true")
	}
}

// TestFileIdentityDetectsOlderMtime pins that the comparison is equality, not
// ordering: a replacement carrying an OLDER timestamp is still a change, so a
// rollback or an out-of-order restore reloads rather than being treated as
// already-current.
func TestFileIdentityDetectsOlderMtime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info := statOrFail(t, path)
	id := Identify(info)

	older := info.ModTime().Add(-time.Hour)
	if err := os.Chtimes(path, older, older); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if id.Matches(statOrFail(t, path)) {
		t.Error("Matches(older mtime on the same inode) = true, want false")
	}
}

// TestFileIdentityModTimeReportsCapturedGeneration pins the diagnostic
// accessor: it reports the timestamp that was captured, not whatever the file
// carries now.
func TestFileIdentityModTimeReportsCapturedGeneration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	info := statOrFail(t, path)
	id := Identify(info)

	newMod := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, newMod, newMod); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if got := id.ModTime(); !got.Equal(info.ModTime()) {
		t.Errorf("ModTime() = %v, want the captured %v", got, info.ModTime())
	}
}
