package atomicfile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v3"
)

// stalePFXTemp writes a temp file of this package's own shape, aged past any
// plausible cutoff.
func stalePFXTemp(t *testing.T, dir, name string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(full, old, old); err != nil {
		t.Fatal(err)
	}
	return full
}

// TestCleanupStaleTempsInRoot_recurses_when_asked covers WithRecursive: a caller whose
// tree is nested needs every level swept, because a temp is staged in the SAME directory
// as its final target. An orphan left in a subdirectory is exactly what a hand-rolled
// walk beside a single-directory sweep tends to miss.
func TestCleanupStaleTempsInRoot_recurses_when_asked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "example.com", "deeper")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	top := stalePFXTemp(t, dir, ".atomicfile-1111111111.tmp")
	mid := stalePFXTemp(t, filepath.Join(dir, "example.com"), ".atomicfile-2222222222.tmp")
	deep := stalePFXTemp(t, nested, ".atomicfile-3333333333.tmp")

	// Caller-owned files must survive, whatever their age or depth.
	keep := filepath.Join(nested, "cert.pfx")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, err := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, time.Hour, atomicfile.WithRecursive(true))
	if err != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", err)
	}
	if got.Removed != 3 {
		t.Errorf("Removed = %d, want 3 (one per level)", got.Removed)
	}
	for _, gone := range []string{top, mid, deep} {
		if _, statErr := os.Stat(gone); statErr == nil {
			t.Errorf("%s survived the sweep", gone)
		}
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Errorf("caller-owned file was removed: %v", statErr)
	}
}

// TestCleanupStaleTempsInRoot_spares_a_fresh_temp pins the age gate: a temp from a
// write still in flight must not be reaped out from under it.
func TestCleanupStaleTempsInRoot_spares_a_fresh_temp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fresh := filepath.Join(dir, ".atomicfile-4444444444.tmp")
	if err := os.WriteFile(fresh, []byte("in flight"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, err := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", err)
	}
	if got.Removed != 0 {
		t.Errorf("Removed = %d, want 0: a temp younger than maxAge belongs to a live write", got.Removed)
	}
	if _, statErr := os.Stat(fresh); statErr != nil {
		t.Errorf("fresh temp was removed: %v", statErr)
	}
}

// TestCleanupStaleTempsInRoot_spares_a_non_regular_temp_name pins that the name
// shape alone is not enough to unlink something: a DIRECTORY wearing a temp name is
// left alone, because removing it is not the operation this function promises.
func TestCleanupStaleTempsInRoot_spares_a_non_regular_temp_name(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	trap := filepath.Join(dir, ".atomicfile-5555555555.tmp")
	if err := os.Mkdir(trap, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(trap, old, old); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, err := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", err)
	}
	if got.Removed != 0 {
		t.Errorf("Removed = %d, want 0", got.Removed)
	}
	if _, statErr := os.Stat(trap); statErr != nil {
		t.Errorf("a directory wearing a temp name was removed: %v", statErr)
	}
}

// TestCleanupStaleTempsInRoot_non_positive_maxAge pins that a misconfigured age
// skips the sweep rather than reaping everything — the same choice
// CleanupStaleTemps makes, and the safe direction for a destructive operation.
func TestCleanupStaleTempsInRoot_non_positive_maxAge(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	temp := stalePFXTemp(t, dir, ".atomicfile-6666666666.tmp")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, err := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, 0)
	if err != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", err)
	}
	if got.Removed != 0 {
		t.Errorf("Removed = %d, want 0", got.Removed)
	}
	if _, statErr := os.Stat(temp); statErr != nil {
		t.Errorf("stale temp removed despite a non-positive maxAge: %v", statErr)
	}
}

func TestCleanupStaleTempsInRoot_nil_root(t *testing.T) {
	t.Parallel()
	if _, err := atomicfile.CleanupStaleTempsInRoot(t.Context(), nil, time.Hour); err == nil {
		t.Error("nil root = nil error, want a rejection")
	}
}

// TestCleanupStaleTempsInRoot_cancelled pins that a done context stops the sweep
// instead of traversing (and unlinking across) a whole tree while shutdown waits.
func TestCleanupStaleTempsInRoot_cancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	temp := stalePFXTemp(t, dir, ".atomicfile-7777777777.tmp")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, sweepErr := atomicfile.CleanupStaleTempsInRoot(ctx, root, time.Hour)
	if !errors.Is(sweepErr, context.Canceled) {
		t.Errorf("CleanupStaleTempsInRoot(cancelled) = %v, want context.Canceled", sweepErr)
	}
	if got.Removed != 0 {
		t.Errorf("Removed = %d, want 0: an already-cancelled sweep must do no work", got.Removed)
	}
	if _, statErr := os.Stat(temp); statErr != nil {
		t.Errorf("cancelled sweep still removed a temp: %v", statErr)
	}
}

// TestCleanupStaleTempsInRoot_counts_unreadable_separately pins the distinction the
// SweepResult exists for: a subdirectory that cannot be entered is Unreadable, not
// Failed, because it is a different thing for an operator to go fix.
func TestCleanupStaleTempsInRoot_counts_unreadable_separately(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.Mkdir(blocked, 0o750); err != nil {
		t.Fatal(err)
	}
	stalePFXTemp(t, blocked, ".atomicfile-8888888888.tmp")
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o750) })

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, sweepErr := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, time.Hour, atomicfile.WithRecursive(true))
	if sweepErr != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil: one unreadable subdir must not abort the sweep", sweepErr)
	}
	if got.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want 1", got.Unreadable)
	}
	if got.Failed != 0 {
		t.Errorf("Failed = %d, want 0: an unenterable subdir is not a failed reclaim", got.Failed)
	}
}

// TestCleanupStaleTempsInRoot_refused_candidate_counts_as_failed pins the case a
// confined sweep exists for: an output subdirectory swapped for a symlink pointing
// out of the tree. The root refuses to stat through it, so the candidate must be
// counted Failed (an operator signal that orphans may be accumulating) and nothing
// outside the root may be unlinked.
func TestCleanupStaleTempsInRoot_refused_candidate_counts_as_failed(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dir, outside := filepath.Join(base, "out"), filepath.Join(base, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	victim := stalePFXTemp(t, outside, ".atomicfile-9999999999.tmp")
	if err := os.Symlink(outside, filepath.Join(dir, "swapped")); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	got, sweepErr := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, time.Hour, atomicfile.WithRecursive(true))
	if sweepErr != nil {
		t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", sweepErr)
	}
	if got.Removed != 0 {
		t.Errorf("Removed = %d, want 0", got.Removed)
	}
	if _, statErr := os.Stat(victim); statErr != nil {
		t.Errorf("a temp outside the root was unlinked: %v", statErr)
	}
}

// TestSweepDepthIsOptIn pins the property WithRecursive exists for: BOTH sweeps behave
// identically without it — one directory, no descent — so how much of the filesystem a
// DESTRUCTIVE operation touches is stated at the call site rather than inferred from
// which function the caller reached for.
//
// The confined and ambient sweeps are asserted side by side deliberately: they differ in
// confinement and cancellation, and must NOT differ in depth.
func TestSweepDepthIsOptIn(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (dir, top, nested string) {
		t.Helper()
		dir = t.TempDir()
		sub := filepath.Join(dir, "example.com")
		if err := os.MkdirAll(sub, 0o750); err != nil {
			t.Fatal(err)
		}
		return dir, stalePFXTemp(t, dir, ".atomicfile-1010101010.tmp"),
			stalePFXTemp(t, sub, ".atomicfile-2020202020.tmp")
	}

	t.Run("confined sweep is flat by default", func(t *testing.T) {
		t.Parallel()
		dir, top, nested := setup(t)
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()

		got, err := atomicfile.CleanupStaleTempsInRoot(t.Context(), root, time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTempsInRoot = %v, want nil", err)
		}
		if got.Removed != 1 {
			t.Errorf("Removed = %d, want 1: without WithRecursive the sweep is one directory deep", got.Removed)
		}
		assertGone(t, top)
		assertPresent(t, nested, "a nested orphan must survive a flat sweep")
	})

	t.Run("ambient sweep is flat by default", func(t *testing.T) {
		t.Parallel()
		dir, top, nested := setup(t)

		removed, err := atomicfile.CleanupStaleTemps(t.Context(), dir, time.Hour)
		if err != nil {
			t.Fatalf("CleanupStaleTemps = %v, want nil", err)
		}
		if removed.Removed != 1 {
			t.Errorf("Removed = %d, want 1", removed.Removed)
		}
		assertGone(t, top)
		assertPresent(t, nested, "a nested orphan must survive a flat sweep")
	})

	t.Run("ambient sweep descends with WithRecursive", func(t *testing.T) {
		t.Parallel()
		dir, top, nested := setup(t)

		removed, err := atomicfile.CleanupStaleTemps(t.Context(), dir, time.Hour, atomicfile.WithRecursive(true))
		if err != nil {
			t.Fatalf("CleanupStaleTemps = %v, want nil", err)
		}
		if removed.Removed != 2 {
			t.Errorf("Removed = %d, want 2 (both levels)", removed.Removed)
		}
		assertGone(t, top)
		assertGone(t, nested)
	})

	t.Run("a caller-owned file survives a recursive ambient sweep", func(t *testing.T) {
		t.Parallel()
		dir, _, _ := setup(t)
		keep := filepath.Join(dir, "example.com", "cert.pfx")
		if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-72 * time.Hour)
		if err := os.Chtimes(keep, old, old); err != nil {
			t.Fatal(err)
		}

		if _, err := atomicfile.CleanupStaleTemps(t.Context(), dir, time.Hour, atomicfile.WithRecursive(true)); err != nil {
			t.Fatalf("CleanupStaleTemps = %v, want nil", err)
		}
		assertPresent(t, keep, "only this package's own temp shape is ever a candidate")
	})
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s survived the sweep", path)
	}
}

func assertPresent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s was removed (%v): %s", path, err, why)
	}
}
