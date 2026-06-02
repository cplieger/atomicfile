package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════════
// NIL-OPTION-ELEMENT GUARD — all entry points
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_NilOption_WriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	err := WriteFile(context.Background(), p, []byte("x"), nil, WithNoSync(), nil)
	if err != nil {
		t.Fatalf("WriteFile with nil option: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_WriteReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "r.txt")
	err := WriteReader(context.Background(), p, strings.NewReader("hello"), 0o644, nil, nil)
	if err != nil {
		t.Fatalf("WriteReader with nil option: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_SaveBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "sb.bin")
	err := SaveBytes(p, []byte("bytes"), 0o644, nil, WithNoSync(), nil)
	if err != nil {
		t.Fatalf("SaveBytes with nil option: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "bytes" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_SaveJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "j.json")
	var mu sync.Mutex
	err := SaveJSON(p, &mu, map[string]int{"a": 1}, "test", 0o644, nil, nil)
	if err != nil {
		t.Fatalf("SaveJSON with nil option: %v", err)
	}
}

func TestConvergence_NilOption_Prepare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "prep.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), p, []byte("prep"), nil, WithNoSync(), nil)
	if err != nil {
		t.Fatalf("Prepare with nil option: %v", err)
	}
	defer cleanup()
	got, _ := os.ReadFile(tmpPath)
	if string(got) != "prep" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_Commit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "commit.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), p, []byte("c"), WithNoSync())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup()
	err = Commit(tmpPath, p, nil, WithNoSync(), nil)
	if err != nil {
		t.Fatalf("Commit with nil option: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "c" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_NewPendingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "pf.txt")
	pf, err := NewPendingFile(p, 0o644, nil, nil)
	if err != nil {
		t.Fatalf("NewPendingFile with nil option: %v", err)
	}
	defer pf.Cleanup()
	pf.Write([]byte("pf"))
	if err := pf.CommitFile(); err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "pf" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_CleanupStaleTemps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Should not panic with nil option elements
	CleanupStaleTemps(dir, time.Hour, nil, nil)
}

// ═══════════════════════════════════════════════════════════════════
// DEFAULT MODE PARITY (0o644) — all constructors
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_DefaultMode_WriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "dm.txt")
	WriteFile(context.Background(), p, []byte("x"))
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("WriteFile default mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestConvergence_DefaultMode_Prepare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "dm.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), p, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	fi, _ := os.Stat(tmpPath)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("Prepare default mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestConvergence_DefaultMode_BuildCfg(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{nil, nil})
	if c.mode != 0o644 {
		t.Fatalf("buildCfg with nils: mode = %o, want 0644", c.mode)
	}
	if c.tempPattern != DefaultTempPrefix {
		t.Fatalf("buildCfg with nils: tempPattern = %q", c.tempPattern)
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: null byte in paths
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_NullByte_AllEntryPoints(t *testing.T) {
	t.Parallel()
	bad := "/tmp/evil\x00path"
	ctx := context.Background()

	if err := WriteFile(ctx, bad, []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("WriteFile null byte: got %v", err)
	}
	if err := WriteReader(ctx, bad, strings.NewReader("x"), 0o644); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("WriteReader null byte: got %v", err)
	}
	if err := SaveBytes(bad, []byte("x"), 0o644); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("SaveBytes null byte: got %v", err)
	}
	var mu sync.Mutex
	if err := SaveJSON(bad, &mu, "x", "t", 0o644); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("SaveJSON null byte: got %v", err)
	}
	if _, _, err := Prepare(ctx, bad, []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("Prepare null byte: got %v", err)
	}
	if _, err := NewPendingFile(bad, 0o644); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("NewPendingFile null byte: got %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: symlink target rejection
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_Symlink_AllEntryPoints(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	os.WriteFile(real, []byte("orig"), 0o644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(real, link)
	ctx := context.Background()

	if err := WriteFile(ctx, link, []byte("x")); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("WriteFile symlink: got %v", err)
	}
	if err := WriteReader(ctx, link, strings.NewReader("x"), 0o644); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("WriteReader symlink: got %v", err)
	}
	if err := SaveBytes(link, []byte("x"), 0o644); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("SaveBytes symlink: got %v", err)
	}
	var mu sync.Mutex
	if err := SaveJSON(link, &mu, "x", "t", 0o644); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("SaveJSON symlink: got %v", err)
	}
	if _, _, err := Prepare(ctx, link, []byte("x")); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("Prepare symlink: got %v", err)
	}
	if _, err := NewPendingFile(link, 0o644); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("NewPendingFile symlink: got %v", err)
	}
	// Verify AllowSymlinkTarget opt works
	if err := WriteFile(ctx, link, []byte("ok"), WithAllowSymlinkTarget()); err != nil {
		t.Errorf("WriteFile with AllowSymlinkTarget: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: MaxInt64 overflow in ReadBounded/saturateAdd
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_SaturateAdd_Boundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b, want int64
	}{
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MaxInt64 - 1, 1, math.MaxInt64},
		{math.MaxInt64 - 1, 2, math.MaxInt64},
		{0, 0, 0},
		{1, 1, 2},
		{math.MaxInt64, 0, math.MaxInt64},
	}
	for _, tc := range cases {
		got := saturateAdd(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("saturateAdd(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestConvergence_ReadBounded_MaxInt64(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "tiny.txt")
	os.WriteFile(p, []byte("hi"), 0o644)
	data, err := ReadBounded(context.Background(), p, math.MaxInt64)
	if err != nil {
		t.Fatalf("ReadBounded(MaxInt64): %v", err)
	}
	if string(data) != "hi" {
		t.Fatalf("got %q", data)
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: temp-leak on failure paths
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_TempLeak_CancelledContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "cancel.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = WriteFile(ctx, p, []byte("data"))
	_ = WriteReader(ctx, p, strings.NewReader("data"), 0o644)
	_, _, _ = Prepare(ctx, p, []byte("data"))

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestConvergence_TempLeak_PendingFile_Cleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "pf.txt")
	pf, err := NewPendingFile(p, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	pf.Write([]byte("data"))
	pf.Cleanup()

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: concurrent writes under -race
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_Concurrent_SaveJSON_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "race.json")
	var mu sync.Mutex
	var wg sync.WaitGroup
	const N = 20
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = SaveJSON(p, &mu, map[string]int{"n": idx}, "race", 0o644)
		}(i)
	}
	wg.Wait()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}

func TestConvergence_Concurrent_WriteReader_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "wr-race.txt")
	var wg sync.WaitGroup
	const N = 15
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = WriteReader(context.Background(), p, bytes.NewReader(bytes.Repeat([]byte{byte(idx)}, idx+1)), 0o644)
		}(i)
	}
	wg.Wait()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: option-ordering edge cases
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_OptionOrder_LastWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "order.txt")
	// Last WithMode should win
	err := WriteFile(context.Background(), p, []byte("x"), WithMode(0o600), WithMode(0o755))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755 (last wins)", fi.Mode().Perm())
	}
}

func TestConvergence_OptionOrder_NilInterspersed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "interspersed.txt")
	err := WriteFile(context.Background(), p, []byte("x"), nil, WithMode(0o600), nil, WithMode(0o755), nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755", fi.Mode().Perm())
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: all-nil option slice
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_AllNilOptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "allnil.txt")
	err := WriteFile(context.Background(), p, []byte("x"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("WriteFile all nils: %v", err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("all-nil mode = %o, want 0644", fi.Mode().Perm())
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: path traversal
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_PathTraversal(t *testing.T) {
	t.Parallel()
	traversals := []string{
		"/tmp/../../../etc/passwd",
		"/tmp/foo/../../etc/shadow",
		"relative/path",
		"",
		"/tmp/a\x00b",
	}
	for _, p := range traversals {
		_, err := validateAbsClean(p)
		if err == nil && p != "" {
			// relative or traversal paths should fail
			if !filepath.IsAbs(p) || strings.Contains(filepath.Clean(p), "..") {
				t.Errorf("validateAbsClean(%q) should have failed", p)
			}
		}
	}
	// /tmp/../../../etc/passwd -> Clean -> /etc/passwd (no ".." remains)
	// validateAbsClean accepts it since the cleaned form has no traversal.
	clean := filepath.Clean("/tmp/../../../etc/passwd")
	if clean != "/etc/passwd" {
		t.Fatalf("unexpected clean: %q", clean)
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: empty path
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_EmptyPath_AllEntryPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if err := WriteFile(ctx, "", []byte("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteFile empty: %v", err)
	}
	if err := WriteReader(ctx, "", strings.NewReader("x"), 0o644); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteReader empty: %v", err)
	}
	if err := SaveBytes("", []byte("x"), 0o644); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("SaveBytes empty: %v", err)
	}
	var mu sync.Mutex
	if err := SaveJSON("", &mu, "x", "t", 0o644); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("SaveJSON empty: %v", err)
	}
	if _, _, err := Prepare(ctx, "", []byte("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("Prepare empty: %v", err)
	}
	if _, err := NewPendingFile("", 0o644); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("NewPendingFile empty: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: SaveJSON nil mutex
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_SaveJSON_NilMutex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "nilmu.json")
	err := SaveJSON(p, nil, "x", "t", 0o644)
	if err == nil {
		t.Fatal("expected error for nil mutex")
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: CleanupStaleTemps with stale files
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_CleanupStaleTemps_Works(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Create a file matching temp pattern
	stale := filepath.Join(dir, "data.tmp-123456")
	os.WriteFile(stale, []byte("stale"), 0o644)
	// Backdate mtime
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(stale, past, past)

	CleanupStaleTemps(dir, time.Hour)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale temp not cleaned up")
	}
}
