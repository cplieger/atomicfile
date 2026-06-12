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
	if _, err := WriteFile(context.Background(), p, []byte("x"), nil, WithNoSync(), nil); err != nil {
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
	if _, err := WriteReader(context.Background(), p, strings.NewReader("hello"), nil, nil); err != nil {
		t.Fatalf("WriteReader with nil option: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_NewPendingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "pf.txt")
	pf, err := NewPendingFile(context.Background(), p, nil, nil)
	if err != nil {
		t.Fatalf("NewPendingFile with nil option: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte("pf")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := pf.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "pf" {
		t.Fatalf("got %q", got)
	}
}

func TestConvergence_NilOption_CleanupStaleTemps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Must not panic with nil option elements.
	if _, err := CleanupStaleTemps(dir, time.Hour, nil, nil); err != nil {
		t.Fatalf("CleanupStaleTemps with nil option: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// DEFAULT MODE PARITY (0o644) — constructors and buildCfg
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_DefaultMode_WriteFile(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "dm.txt")
	if _, err := WriteFile(context.Background(), p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("WriteFile default mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestConvergence_DefaultMode_BuildCfg(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{nil, nil})
	if c.mode != 0o644 {
		t.Fatalf("buildCfg with nils: mode = %o, want 0644", c.mode)
	}
	if c.logger == nil {
		t.Fatal("buildCfg with nils: logger is nil, want slog.Default()")
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: null byte in paths
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_NullByte_AllEntryPoints(t *testing.T) {
	t.Parallel()
	bad := "/tmp/evil\x00path"
	ctx := context.Background()

	if _, err := WriteFile(ctx, bad, []byte("x")); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("WriteFile null byte: got %v", err)
	}
	if _, err := WriteReader(ctx, bad, strings.NewReader("x")); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("WriteReader null byte: got %v", err)
	}
	if _, err := NewPendingFile(ctx, bad); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("NewPendingFile null byte: got %v", err)
	}
	if _, err := ReadBounded(ctx, bad, 1024); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("ReadBounded null byte: got %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: symlink target rejection
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_Symlink_AllEntryPoints(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("orig"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	ctx := context.Background()

	if _, err := WriteFile(ctx, link, []byte("x")); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("WriteFile symlink: got %v", err)
	}
	if _, err := WriteReader(ctx, link, strings.NewReader("x")); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("WriteReader symlink: got %v", err)
	}
	if _, err := NewPendingFile(ctx, link); !errors.Is(err, ErrSymlinkTarget) {
		t.Errorf("NewPendingFile symlink: got %v", err)
	}
	// WithAllowSymlinkTarget opts back in.
	if _, err := WriteFile(ctx, link, []byte("ok"), WithAllowSymlinkTarget()); err != nil {
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
	if err := os.WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
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

	_, _ = WriteFile(ctx, p, []byte("data"))
	_, _ = WriteReader(ctx, p, strings.NewReader("data"))

	assertNoTempLeak(t, dir)
}

func TestConvergence_TempLeak_PendingFile_Cleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "pf.txt")
	pf, err := NewPendingFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := pf.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	assertNoTempLeak(t, dir)
}

// ═══════════════════════════════════════════════════════════════════
// RE-ATTACK: concurrent writes under -race
// ═══════════════════════════════════════════════════════════════════

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
			_, _ = WriteReader(context.Background(), p, bytes.NewReader(bytes.Repeat([]byte{byte(idx)}, idx+1)))
		}(i)
	}
	wg.Wait()
	assertNoTempLeak(t, dir)
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: option-ordering edge cases
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_OptionOrder_LastWins(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "order.txt")
	// Last WithMode should win.
	if _, err := WriteFile(context.Background(), p, []byte("x"), WithMode(0o600), WithMode(0o755)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755 (last wins)", fi.Mode().Perm())
	}
}

func TestConvergence_OptionOrder_NilInterspersed(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "interspersed.txt")
	if _, err := WriteFile(context.Background(), p, []byte("x"), nil, WithMode(0o600), nil, WithMode(0o755), nil); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 0755", fi.Mode().Perm())
	}
}

func TestConvergence_AllNilOptions(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "allnil.txt")
	if _, err := WriteFile(context.Background(), p, []byte("x"), nil, nil, nil, nil); err != nil {
		t.Fatalf("WriteFile all nils: %v", err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("all-nil mode = %o, want 0644", fi.Mode().Perm())
	}
}

// ═══════════════════════════════════════════════════════════════════
// ADVERSARIAL: path traversal and empty path
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_PathTraversal(t *testing.T) {
	t.Parallel()
	// /tmp/../../../etc/passwd -> Clean -> /etc/passwd (no ".." remains), which
	// validateAbsClean accepts since the cleaned form has no traversal.
	clean := filepath.Clean("/tmp/../../../etc/passwd")
	if clean != "/etc/passwd" {
		t.Fatalf("unexpected clean: %q", clean)
	}
	if got, err := validateAbsClean("/tmp/../../../etc/passwd"); err != nil || got != "/etc/passwd" {
		t.Fatalf("validateAbsClean = (%q, %v), want (/etc/passwd, nil)", got, err)
	}
	// A relative path is always rejected.
	if _, err := validateAbsClean("relative/path"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("validateAbsClean(relative) = %v, want ErrUnsafePath", err)
	}
}

func TestConvergence_EmptyPath_AllEntryPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := WriteFile(ctx, "", []byte("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteFile empty: %v", err)
	}
	if _, err := WriteReader(ctx, "", strings.NewReader("x")); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("WriteReader empty: %v", err)
	}
	if _, err := NewPendingFile(ctx, ""); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("NewPendingFile empty: %v", err)
	}
	if _, err := ReadBounded(ctx, "", 1024); !errors.Is(err, ErrEmptyPath) {
		t.Errorf("ReadBounded empty: %v", err)
	}
}

// ═══════════════════════════════════════════════════════════════════
// CleanupStaleTemps end-to-end
// ═══════════════════════════════════════════════════════════════════

func TestConvergence_CleanupStaleTemps_Works(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, ".atomicfile-123456.tmp")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	removed, err := CleanupStaleTemps(dir, time.Hour)
	if err != nil {
		t.Fatalf("CleanupStaleTemps: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale temp not cleaned up")
	}
}
