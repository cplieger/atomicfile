package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIsPackageTemp pins the predicate to the sweep it is the exported face of:
// for every name, the answer IsPackageTemp gives is exactly what
// CleanupStaleTemps does to a backdated file of that name. A caller can then
// use the predicate to decide whether its own file is reclaimable — or
// deliberately NOT reclaimable — without reading the sweep's source.
func TestIsPackageTemp(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		".atomicfile-0.tmp":                    true,
		".atomicfile-12345.tmp":                true,
		".atomicfile-18446744073709551615.tmp": true,
		// Caller-owned names that merely share the prefix or suffix. The
		// all-digit middle is what keeps every one of these safe.
		".atomicfile-notes.tmp":  false,
		".atomicfile-.tmp":       false,
		".atomicfile-12a.tmp":    false,
		".atomicfile-1e5.tmp":    false,
		".atomicfile-+1.tmp":     false,
		".atomicfile- 12.tmp":    false,
		".atomicfile-١٢٣.tmp":    false,
		".atomicfile-12.tmp.bak": false,
		".atomicfile-12":         false,
		"atomicfile-12.tmp":      false,
		"12.tmp":                 false,
		"config.tmp-backup":      false,
		".ATOMICFILE-12.TMP":     false,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := IsPackageTemp(name); got != want {
				t.Errorf("IsPackageTemp(%q) = %v, want %v", name, got, want)
			}

			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("staged"), 0o600); err != nil {
				t.Fatalf("seed %q: %v", path, err)
			}
			old := time.Now().Add(-2 * time.Hour)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatalf("Chtimes %q: %v", path, err)
			}
			removed, err := CleanupStaleTemps(t.Context(), dir, time.Hour)
			if err != nil {
				t.Fatalf("CleanupStaleTemps: %v", err)
			}
			if (removed.Removed == 1) != want {
				t.Errorf("CleanupStaleTemps removed %d files named %q, but IsPackageTemp says %v",
					removed, name, want)
			}
		})
	}
}

func TestIsPackageTempEmptyName(t *testing.T) {
	t.Parallel()
	// Not seedable as a file, so it is only a predicate case.
	if IsPackageTemp("") {
		t.Error("IsPackageTemp(\"\") = true, want false")
	}
}

// TestTempName checks the generator's contract: every name it returns is one
// the sweeps reclaim, and two names do not collide. This is the property a
// caller would otherwise be betting on os.CreateTemp's undocumented choice of
// decimal digits for its "*" substitution.
func TestTempName(t *testing.T) {
	t.Parallel()

	const draws = 1000
	seen := make(map[string]bool, draws)
	for range draws {
		name := TempName()
		if !IsPackageTemp(name) {
			t.Fatalf("TempName() = %q, which IsPackageTemp rejects", name)
		}
		if seen[name] {
			t.Fatalf("TempName() repeated %q within %d draws", name, draws)
		}
		seen[name] = true
	}
}

func TestTempNameIsSweptForReal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const files = 8
	old := time.Now().Add(-2 * time.Hour)
	for range files {
		path := filepath.Join(dir, TempName())
		if err := os.WriteFile(path, []byte("staged"), 0o600); err != nil {
			t.Fatalf("seed %q: %v", path, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("Chtimes %q: %v", path, err)
		}
	}

	removed, err := CleanupStaleTemps(t.Context(), dir, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTemps: %v", err)
	}
	if removed.Removed != files {
		t.Errorf("removed = %d, want %d", removed, files)
	}
	assertNoTempLeak(t, dir)
}
