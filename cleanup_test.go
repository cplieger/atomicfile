package atomicfile

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCleanupStaleTemps(t *testing.T) {
	t.Parallel()

	t.Run("removes_old_keeps_recent_and_returns_count", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		oldTime := time.Now().Add(-2 * time.Hour)

		stale := filepath.Join(dir, ".atomicfile-987654321.tmp")
		if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}

		recent := filepath.Join(dir, ".atomicfile-111222333.tmp")
		if err := os.WriteFile(recent, []byte("new"), 0o644); err != nil {
			t.Fatalf("seed recent: %v", err)
		}

		removed, err := CleanupStaleTemps(dir, time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}
		if removed != 1 {
			t.Errorf("removed = %d, want 1", removed)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale temp not removed: stat err = %v", err)
		}
		if _, err := os.Stat(recent); err != nil {
			t.Errorf("recent temp wrongly removed: %v", err)
		}
	})

	t.Run("missing_dir_is_not_an_error", func(t *testing.T) {
		t.Parallel()
		removed, err := CleanupStaleTemps(filepath.Join(t.TempDir(), "nope"), time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps(missing) = %v, want nil", err)
		}
		if removed != 0 {
			t.Errorf("removed = %d, want 0", removed)
		}
	})

	t.Run("spares_caller_files_that_share_prefix_or_suffix", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		oldTime := time.Now().Add(-2 * time.Hour)

		// Caller-owned files that must never be reclaimed even when old.
		spared := []string{
			".atomicfile-notes.tmp",  // non-digit middle
			"config.tmp-backup",      // not the package shape at all
			".atomicfilebackup.tmp",  // no separating digits
			".atomicfile-12ab34.tmp", // mixed alnum middle
		}
		for _, name := range spared {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
				t.Fatalf("seed %q: %v", name, err)
			}
			if err := os.Chtimes(p, oldTime, oldTime); err != nil {
				t.Fatalf("Chtimes %q: %v", name, err)
			}
		}

		// A genuine package temp, old enough to reclaim.
		realTemp := filepath.Join(dir, ".atomicfile-123456.tmp")
		if err := os.WriteFile(realTemp, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed real temp: %v", err)
		}
		if err := os.Chtimes(realTemp, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes real temp: %v", err)
		}

		removed, err := CleanupStaleTemps(dir, time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}
		if removed != 1 {
			t.Errorf("removed = %d, want 1 (only the digit-suffixed temp)", removed)
		}
		for _, name := range spared {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("caller file %q wrongly removed: %v", name, err)
			}
		}
		if _, err := os.Stat(realTemp); !os.IsNotExist(err) {
			t.Errorf("real temp not removed")
		}
	})

	t.Run("real_writes_leave_reclaimable_temps_only_when_orphaned", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "live.txt")
		if _, err := WriteFile(context.Background(), path, []byte("data")); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		// A successful write leaves no temp, so nothing to reclaim.
		removed, err := CleanupStaleTemps(dir, 0)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}
		if removed != 0 {
			t.Errorf("removed = %d, want 0 (no orphaned temps after a clean write)", removed)
		}
	})
}

func TestCleanupStaleTemps_readdir_error_does_not_panic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive readdir permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses EACCES")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "inaccessible")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir error = %v", err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := CleanupStaleTemps(dir, time.Hour); err == nil {
		t.Error("CleanupStaleTemps(EACCES dir) = nil error, want readdir error")
	}
}

func TestCleanupStaleTemps_continues_after_remove_failure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive unlink permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses directory-write EACCES")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, ".atomicfile-111.tmp")
	other := filepath.Join(dir, ".atomicfile-222.tmp")
	for _, p := range []string{blocked, other} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", p, err)
		}
		oldTime := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatalf("Chtimes(%q) error = %v", p, err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	// A read-only dir blocks unlink; CleanupStaleTemps must not panic and must
	// report zero removed since every remove fails.
	removed, err := CleanupStaleTemps(dir, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil (per-file failures are skipped)", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (all removes blocked)", removed)
	}
	_ = os.Chmod(dir, 0o755)
	for _, p := range []string{blocked, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("CleanupStaleTemps removed %q despite EACCES: %v", p, err)
		}
	}
}

func TestCleanupStaleTemps_RemoveFailure_WarnsWithCount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits drive unlink permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses directory-write EACCES")
	}
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	for _, name := range []string{".atomicfile-111.tmp", ".atomicfile-222.tmp"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes %q: %v", name, err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	h := &captureHandler{}
	removed, err := CleanupStaleTemps(dir, time.Hour, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	var warnCount int
	var failedAttr int64 = -1
	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			warnCount++
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "failed" {
					failedAttr = a.Value.Int64()
				}
				return true
			})
		}
	}
	if warnCount != 1 {
		t.Errorf("warn count = %d, want 1", warnCount)
	}
	if failedAttr != 2 {
		t.Errorf("failed attr = %d, want 2", failedAttr)
	}
}

func TestCleanupStaleTemps_NonPositiveMaxAge_SparesOldTemp(t *testing.T) {
	t.Parallel()
	for _, maxAge := range []time.Duration{0, -time.Hour} {
		dir := t.TempDir()
		stale := filepath.Join(dir, ".atomicfile-987654321.tmp")
		if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(stale, old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
		removed, err := CleanupStaleTemps(dir, maxAge)
		if err != nil {
			t.Fatalf("CleanupStaleTemps(%v) = %v", maxAge, err)
		}
		if removed != 0 {
			t.Errorf("CleanupStaleTemps(maxAge=%v) removed = %d, want 0", maxAge, removed)
		}
		if _, err := os.Stat(stale); err != nil {
			t.Errorf("CleanupStaleTemps(maxAge=%v) wrongly removed old temp: %v", maxAge, err)
		}
	}
}

func TestIsStaleTempName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"digit suffix temp", ".atomicfile-123456.tmp", true},
		{"single digit", ".atomicfile-7.tmp", true},
		{"word tail spared", ".atomicfile-notes.tmp", false},
		{"backup tail spared", ".atomicfile-backup.tmp", false},
		{"mixed alnum spared", ".atomicfile-12ab34.tmp", false},
		{"empty middle spared", ".atomicfile-.tmp", false},
		{"no prefix", "atomicfile-123.tmp", false},
		{"wrong prefix", ".atomicfilebackup.tmp", false},
		{"wrong suffix", ".atomicfile-123.bak", false},
		{"unrelated", "config.tmp-backup", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaleTempName(tt.in); got != tt.want {
				t.Errorf("isStaleTempName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsAllDigits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{"0", true},
		{"123456", true},
		{"", false},
		{"12a", false},
		{"a12", false},
		{" 12", false},
		{"-12", false},
	}
	for _, tt := range tests {
		if got := isAllDigits(tt.in); got != tt.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// CleanupStaleTemps must never reclaim a non-regular entry that merely shares
// the .atomicfile-<digits>.tmp shape (os.CreateTemp only makes regular files).
// Pins the info.Mode().IsRegular() guard at cleanup.go:74 (added in the
// per-concern split, previously hits=0). The dir is aged past the cutoff so the
// ONLY thing sparing it is the non-regular skip, not the mtime guard; without
// the guard os.Remove would rmdir the empty aged dir and removed would be 1.
func TestCleanupStaleTemps_SkipsNonRegularTempNamedDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	staleDir := filepath.Join(dir, ".atomicfile-555000111.tmp")
	if err := os.Mkdir(staleDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := CleanupStaleTemps(dir, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTemps: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 (a temp-named directory must never be reclaimed)", removed)
	}
	if fi, statErr := os.Stat(staleDir); statErr != nil || !fi.IsDir() {
		t.Errorf("temp-named directory removed/altered (stat err=%v); IsRegular guard must spare it", statErr)
	}
}

// With exactly one stale temp reclaimed and none failing, the Info "removed"
// line fires once and the Warn "could not be removed" line never fires.
func TestCleanupStaleTemps_RemovedTemps_LogsInfoNotWarn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, ".atomicfile-123456.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed stale temp: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	h := &captureHandler{}

	removed, err := CleanupStaleTemps(dir, time.Hour, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if got := countLogByMessage(h.records, slog.LevelInfo, msgStaleRemoved); got != 1 {
		t.Errorf("Info %q count = %d, want 1 (removed=1 must log the summary)", msgStaleRemoved, got)
	}
	if got := countLogByMessage(h.records, slog.LevelWarn, msgStaleRemoveFail); got != 0 {
		t.Errorf("Warn %q count = %d, want 0 (failed=0 must not warn)", msgStaleRemoveFail, got)
	}
}

// A run that reclaims nothing emits no summary logs.
func TestCleanupStaleTemps_NothingRemoved_DoesNotLogInfo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.json"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}
	h := &captureHandler{}

	removed, err := CleanupStaleTemps(dir, time.Hour, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatalf("CleanupStaleTemps = %v, want nil", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if got := countLogByMessage(h.records, slog.LevelInfo, msgStaleRemoved); got != 0 {
		t.Errorf("Info %q count = %d, want 0 (removed=0 must not log the summary)", msgStaleRemoved, got)
	}
	if got := countLogByMessage(h.records, slog.LevelWarn, msgStaleRemoveFail); got != 0 {
		t.Errorf("Warn %q count = %d, want 0 (failed=0 must not warn)", msgStaleRemoveFail, got)
	}
}
