package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// (A) REFACTOR-SPECIFIC: default-parity, WithX threading, option order
// ═══════════════════════════════════════════════════════════════════

func TestRefactor_WriteFile_NoOptions_Uses0644(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "default-mode.txt")
	if _, err := WriteFile(context.Background(), path, []byte("hi")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("default mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestRefactor_WriteFile_NilOptionSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nil-opts.txt")
	var opts []Option
	if _, err := WriteFile(context.Background(), path, []byte("nil"), opts...); err != nil {
		t.Fatalf("WriteFile(nil opts): %v", err)
	}
}

func TestRefactor_WriteFile_EmptyOptionSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-opts.txt")
	opts := []Option{}
	if _, err := WriteFile(context.Background(), path, []byte("empty"), opts...); err != nil {
		t.Fatalf("WriteFile(empty opts): %v", err)
	}
}

func TestRefactor_BuildCfg_Defaults(t *testing.T) {
	t.Parallel()
	c := buildCfg(nil)
	if c.mode != 0o644 {
		t.Errorf("default mode = %o, want 0644", c.mode)
	}
	if c.logger == nil {
		t.Error("logger is nil, want slog.Default()")
	}
	if c.preserveMode {
		t.Error("preserveMode should be false by default")
	}
	if c.preserveOwnership {
		t.Error("preserveOwnership should be false by default")
	}
	if c.noSync {
		t.Error("noSync should be false by default")
	}
	if c.allowSymlinkTarget {
		t.Error("allowSymlinkTarget should be false by default")
	}
	if c.mkdirMode != 0 {
		t.Errorf("mkdirMode = %o, want 0", c.mkdirMode)
	}
}

func TestRefactor_WithMode_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithMode(0o755)})
	if c.mode != 0o755 {
		t.Fatalf("WithMode(0755) threaded as %o", c.mode)
	}
}

func TestRefactor_WithLogger_Threads(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	c := buildCfg([]Option{WithLogger(l)})
	if c.logger != l {
		t.Fatal("WithLogger did not thread correctly")
	}
}

func TestRefactor_WithMkdirMode_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithMkdirMode(0o750)})
	if c.mkdirMode != 0o750 {
		t.Fatalf("mkdirMode = %o, want 0750", c.mkdirMode)
	}
}

func TestRefactor_WithPreserveMode_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithPreserveMode()})
	if !c.preserveMode {
		t.Fatal("WithPreserveMode() did not set preserveMode")
	}
}

func TestRefactor_WithPreserveOwnership_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithPreserveOwnership()})
	if !c.preserveOwnership {
		t.Fatal("WithPreserveOwnership() did not set preserveOwnership")
	}
}

func TestRefactor_WithNoSync_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithNoSync()})
	if !c.noSync {
		t.Fatal("WithNoSync() did not set noSync")
	}
}

func TestRefactor_WithAllowSymlinkTarget_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithAllowSymlinkTarget()})
	if !c.allowSymlinkTarget {
		t.Fatal("WithAllowSymlinkTarget() did not set allowSymlinkTarget")
	}
}

func TestRefactor_OptionOrder_DoesNotMatter(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{
		WithNoSync(),
		WithMode(0o600),
		WithAllowSymlinkTarget(),
		WithMode(0o755), // overrides the earlier mode
		WithMkdirMode(0o700),
		WithPreserveOwnership(),
	})
	if c.mode != 0o755 {
		t.Errorf("mode = %o, want 0755 (last wins)", c.mode)
	}
	if !c.noSync {
		t.Error("noSync not set")
	}
	if !c.allowSymlinkTarget {
		t.Error("allowSymlinkTarget not set")
	}
	if c.mkdirMode != 0o700 {
		t.Errorf("mkdirMode = %o, want 0700", c.mkdirMode)
	}
	if !c.preserveOwnership {
		t.Error("preserveOwnership not set")
	}
}

func TestRefactor_AllOptionsCombined_WriteFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "combined.txt")
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	res, err := WriteFile(context.Background(), path, []byte("combined"),
		WithLogger(l),
		WithMode(0o600),
		WithMkdirMode(0o755),
		WithNoSync(),
	)
	if err != nil {
		t.Fatalf("WriteFile combined opts: %v", err)
	}
	if res.Durable {
		t.Errorf("Result.Durable = true, want false under WithNoSync")
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	got, _ := os.ReadFile(path)
	if string(got) != "combined" {
		t.Errorf("content = %q", got)
	}
}

// WriteReader takes its mode via WithMode (no positional mode arg).
func TestRefactor_WriteReader_ModeViaWithMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "wm.txt")
	if _, err := WriteReader(context.Background(), path, strings.NewReader("pos"), WithMode(0o600)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("WriteReader WithMode = %o, want 0600", fi.Mode().Perm())
	}
}

// NewPendingFile takes its mode via WithMode (no positional mode arg).
func TestRefactor_NewPendingFile_ModeViaWithMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-mode.txt")
	pf, err := NewPendingFile(context.Background(), path, WithMode(0o600))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := pf.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("NewPendingFile WithMode = %o, want 0600", fi.Mode().Perm())
	}
}

// ═══════════════════════════════════════════════════════════════════
// (B) FULL RE-ATTACK: temp-leak, null-byte, MaxInt64, symlink, race
// ═══════════════════════════════════════════════════════════════════

func TestReattack_TempLeak_WriteFile_AllErrorPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Missing parent (no MkdirMode) → temp create fails before any temp lands.
	path := filepath.Join(dir, "noparent", "file.txt")
	_, _ = WriteFile(context.Background(), path, []byte("x"))
	assertNoTempLeak(t, dir)
}

func TestReattack_TempLeak_WriteReader_AllErrorPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "noparent", "file.txt")
	_, _ = WriteReader(context.Background(), path, strings.NewReader("x"))
	assertNoTempLeak(t, dir)
}

func TestReattack_NullByte_Path_AllFunctions(t *testing.T) {
	t.Parallel()
	nullPath := "/tmp/redteam\x00evil"

	if _, err := WriteFile(context.Background(), nullPath, []byte("x")); err == nil {
		t.Error("WriteFile: expected error")
	}
	if _, err := WriteReader(context.Background(), nullPath, strings.NewReader("x")); err == nil {
		t.Error("WriteReader: expected error")
	}
	if _, err := NewPendingFile(context.Background(), nullPath); err == nil {
		t.Error("NewPendingFile: expected error")
	}
	if _, err := ReadBounded(context.Background(), nullPath, 1024); err == nil {
		t.Error("ReadBounded: expected error")
	}
}

func TestReattack_ReadBounded_MaxInt64_NoOverflow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	data, err := ReadBounded(context.Background(), path, math.MaxInt64)
	if err != nil {
		t.Fatalf("ReadBounded(MaxInt64): %v", err)
	}
	if string(data) != "abc" {
		t.Fatalf("got %q", data)
	}
}

func TestReattack_SymlinkRefusal_WriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte("original"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := WriteFile(context.Background(), link, []byte("evil"))
	if !errors.Is(err, ErrSymlinkTarget) {
		t.Fatalf("expected ErrSymlinkTarget, got %v", err)
	}
	assertNoTempLeak(t, dir)
	got, _ := os.ReadFile(real)
	if string(got) != "original" {
		t.Errorf("original modified: %q", got)
	}
}

func TestReattack_ConcurrentWriteFile_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "race.txt")
	var wg sync.WaitGroup
	const N = 30
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = WriteFile(context.Background(), path, []byte(strings.Repeat("A", idx+1)))
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected len=%d", len(got))
	}
	assertNoTempLeak(t, dir)
}

// Erroring reader with various options — ensure no temp leak.
func TestReattack_WriteReader_ErrorWithOptions_NoLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "err-opts.txt")
	r := plainReader{r: &errReader{n: 10, err: io.ErrUnexpectedEOF}}
	_, err := WriteReader(context.Background(), path, r,
		WithNoSync(), WithMode(0o600))
	if err == nil {
		t.Fatal("expected error")
	}
	assertNoTempLeak(t, dir)
}

func TestReattack_PendingFile_NoSync_CommitWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-nosync.txt")
	pf, err := NewPendingFile(context.Background(), path, WithNoSync())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte("no-sync-pf")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	res, err := pf.Commit(context.Background())
	if err != nil {
		t.Fatalf("Commit(NoSync): %v", err)
	}
	if res.Durable {
		t.Errorf("Result.Durable = true, want false under WithNoSync")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "no-sync-pf" {
		t.Fatalf("got %q", got)
	}
}

// WithPreserveMode takes priority over WithMode for an existing target.
func TestRefactor_PreserveMode_OverridesWithMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pm.txt")
	if err := os.WriteFile(path, []byte("old"), 0o751); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := WriteFile(context.Background(), path, []byte("new"),
		WithMode(0o600), WithPreserveMode()); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o751 {
		t.Fatalf("PreserveMode should override WithMode; got %o, want 0751", fi.Mode().Perm())
	}
}

func TestRefactor_PreserveMode_FallsBackToWithMode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file mode not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pm-new.txt")
	if _, err := WriteFile(context.Background(), path, []byte("new"),
		WithMode(0o600), WithPreserveMode()); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("PreserveMode fallback; got %o, want 0600", fi.Mode().Perm())
	}
}

func TestReattack_ConcurrentPendingFile_WithOptions_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-race.txt")
	var wg sync.WaitGroup
	const N = 20
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pf, err := NewPendingFile(context.Background(), path, WithNoSync())
			if err != nil {
				return
			}
			defer func() { _ = pf.Cleanup() }()
			if _, err := pf.Write([]byte(strings.Repeat("Z", idx+1))); err != nil {
				return
			}
			_, _ = pf.Commit(context.Background())
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected len=%d", len(got))
	}
	assertNoTempLeak(t, dir)
}
