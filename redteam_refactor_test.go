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
	"strings"
	"sync"
	"testing"
)

// ═══════════════════════════════════════════════════════════════════
// (A) REFACTOR-SPECIFIC: default-parity, WithX threading, option order
// ═══════════════════════════════════════════════════════════════════

func TestRefactor_WriteFile_NoOptions_Uses0644(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "default-mode.txt")
	if err := WriteFile(context.Background(), path, []byte("hi")); err != nil {
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
	if err := WriteFile(context.Background(), path, []byte("nil"), opts...); err != nil {
		t.Fatalf("WriteFile(nil opts): %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("nil opts mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestRefactor_WriteFile_EmptyOptionSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-opts.txt")
	opts := []Option{}
	if err := WriteFile(context.Background(), path, []byte("empty"), opts...); err != nil {
		t.Fatalf("WriteFile(empty opts): %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("empty opts mode = %o, want 0644", fi.Mode().Perm())
	}
}

func TestRefactor_BuildCfg_Defaults(t *testing.T) {
	t.Parallel()
	c := buildCfg(nil)
	if c.mode != 0o644 {
		t.Errorf("default mode = %o, want 0644", c.mode)
	}
	if c.tempPattern != DefaultTempPrefix {
		t.Errorf("default tempPattern = %q, want %q", c.tempPattern, DefaultTempPrefix)
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
	dir := t.TempDir()
	path := filepath.Join(dir, "mode-thread.txt")
	if err := WriteFile(context.Background(), path, []byte("x"), WithMode(0o755)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("WithMode(0755) → %o", fi.Mode().Perm())
	}
}

func TestRefactor_WithTempPattern_Threads(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{WithTempPattern(".custom-*.xyz")})
	if c.tempPattern != ".custom-*.xyz" {
		t.Fatalf("tempPattern = %q, want .custom-*.xyz", c.tempPattern)
	}
}

func TestRefactor_WithTempPattern_Empty_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	// Passing empty string should fall back to default
	c := buildCfg([]Option{WithTempPattern("")})
	if c.tempPattern != DefaultTempPrefix {
		t.Fatalf("empty tempPattern = %q, want %q", c.tempPattern, DefaultTempPrefix)
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
	// Last WithMode wins, but all other options should still be set
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
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "combined.txt")
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	err := WriteFile(context.Background(), path, []byte("combined"),
		WithLogger(l),
		WithTempPattern(".redteam-*.tmp"),
		WithMode(0o600),
		WithMkdirMode(0o755),
		WithNoSync(),
	)
	if err != nil {
		t.Fatalf("WriteFile combined opts: %v", err)
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

// Verify WriteReader still takes mode positionally
func TestRefactor_WriteReader_ModePositional(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "positional.txt")
	err := WriteReader(context.Background(), path, strings.NewReader("pos"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("WriteReader positional mode = %o, want 0600", fi.Mode().Perm())
	}
}

// Verify SaveBytes still takes perm positionally
func TestRefactor_SaveBytes_ModePositional(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sb-pos.txt")
	err := SaveBytes(path, []byte("x"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("SaveBytes positional mode = %o, want 0600", fi.Mode().Perm())
	}
}

// Verify SaveJSON still takes perm positionally
func TestRefactor_SaveJSON_ModePositional(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sj-pos.json")
	var mu sync.Mutex
	err := SaveJSON(path, &mu, "x", "test", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("SaveJSON positional mode = %o, want 0600", fi.Mode().Perm())
	}
}

// Verify NewPendingFile still takes mode positionally
func TestRefactor_NewPendingFile_ModePositional(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-pos.txt")
	pf, err := NewPendingFile(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Cleanup()
	pf.Write([]byte("x"))
	if err := pf.CommitFile(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("NewPendingFile positional mode = %o, want 0600", fi.Mode().Perm())
	}
}

// ═══════════════════════════════════════════════════════════════════
// (B) FULL RE-ATTACK: temp-leak, null-byte, MaxInt64, symlink, race,
//     NoSync in Commit
// ═══════════════════════════════════════════════════════════════════

func TestReattack_TempLeak_WriteFile_AllErrorPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Path with non-existent parent (no MkdirMode → temp create fails)
	path := filepath.Join(dir, "noparent", "file.txt")
	_ = WriteFile(context.Background(), path, []byte("x"))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leak in noparent case: %s", e.Name())
		}
	}
}

func TestReattack_TempLeak_WriteReader_AllErrorPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "noparent", "file.txt")
	_ = WriteReader(context.Background(), path, strings.NewReader("x"), 0o644)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leak: %s", e.Name())
		}
	}
}

func TestReattack_TempLeak_Prepare_AllErrorPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "noparent", "file.txt")
	_, _, err := Prepare(context.Background(), path, []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leak: %s", e.Name())
		}
	}
}

func TestReattack_NullByte_Path_AllFunctions(t *testing.T) {
	t.Parallel()
	nullPath := "/tmp/redteam\x00evil"

	if err := WriteFile(context.Background(), nullPath, []byte("x")); err == nil {
		t.Error("WriteFile: expected error")
	}
	if err := WriteReader(context.Background(), nullPath, strings.NewReader("x"), 0o644); err == nil {
		t.Error("WriteReader: expected error")
	}
	if err := SaveBytes(nullPath, []byte("x"), 0o644); err == nil {
		t.Error("SaveBytes: expected error")
	}
	var mu sync.Mutex
	if err := SaveJSON(nullPath, &mu, "x", "test", 0o644); err == nil {
		t.Error("SaveJSON: expected error")
	}
	if _, err := NewPendingFile(nullPath, 0o644); err == nil {
		t.Error("NewPendingFile: expected error")
	}
	if _, _, err := Prepare(context.Background(), nullPath, []byte("x")); err == nil {
		t.Error("Prepare: expected error")
	}
	if err := Commit("/tmp/fake", nullPath); err == nil {
		t.Error("Commit: expected error")
	}
	if _, err := ReadBounded(context.Background(), nullPath, 1024); err == nil {
		t.Error("ReadBounded: expected error")
	}
}

func TestReattack_ReadBounded_MaxInt64_NoOverflow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "small.txt")
	os.WriteFile(path, []byte("abc"), 0o644)
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
	os.WriteFile(real, []byte("original"), 0o644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(real, link)

	err := WriteFile(context.Background(), link, []byte("evil"))
	if !errors.Is(err, ErrSymlinkTarget) {
		t.Fatalf("expected ErrSymlinkTarget, got %v", err)
	}
	// Must not have leaked temp files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leaked on symlink refusal: %s", e.Name())
		}
	}
	// Original untouched
	got, _ := os.ReadFile(real)
	if string(got) != "original" {
		t.Errorf("original modified: %q", got)
	}
}

func TestReattack_SymlinkRefusal_NoTempLeak_SaveBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	os.WriteFile(real, []byte("original"), 0o644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(real, link)

	_ = SaveBytes(link, []byte("evil"), 0o644)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leaked: %s", e.Name())
		}
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
			_ = WriteFile(context.Background(), path, []byte(strings.Repeat("A", idx+1)))
		}(i)
	}
	wg.Wait()
	// Final file must be valid
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected len=%d", len(got))
	}
	// No leaked temps
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leaked after race: %s", e.Name())
		}
	}
}

func TestReattack_Commit_NoSync_Respected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nosync-commit.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("nosync-data"), WithNoSync())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Commit must not panic/fail when NoSync is passed
	if err := Commit(tmpPath, path, WithNoSync()); err != nil {
		t.Fatalf("Commit(NoSync): %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "nosync-data" {
		t.Fatalf("got %q", got)
	}
}

func TestReattack_Commit_WithoutNoSync_AlsoWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sync-commit.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("sync-data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := Commit(tmpPath, path); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "sync-data" {
		t.Fatalf("got %q", got)
	}
}

// Attack: erroring reader with various WithX options — ensure no temp leak
func TestReattack_WriteReader_ErrorWithOptions_NoLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "err-opts.txt")
	r := &errReader{n: 10, err: io.ErrUnexpectedEOF}
	err := WriteReader(context.Background(), path, r, 0o644,
		WithNoSync(), WithMode(0o600), WithTempPattern(".redteam-*.tmp"))
	if err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "redteam") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leaked with options: %s", e.Name())
		}
	}
}

// Attack: PendingFile with NoSync — CommitFile must still work
func TestReattack_PendingFile_NoSync_CommitWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-nosync.txt")
	pf, err := NewPendingFile(path, 0o644, WithNoSync())
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Cleanup()
	pf.Write([]byte("no-sync-pf"))
	if err := pf.CommitFile(); err != nil {
		t.Fatalf("CommitFile(NoSync): %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "no-sync-pf" {
		t.Fatalf("got %q", got)
	}
}

// Verify WithPreserveMode + WithMode — PreserveMode takes priority for existing file
func TestRefactor_PreserveMode_OverridesWithMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pm.txt")
	os.WriteFile(path, []byte("old"), 0o751)
	err := WriteFile(context.Background(), path, []byte("new"),
		WithMode(0o600), WithPreserveMode())
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o751 {
		t.Fatalf("PreserveMode should override WithMode; got %o, want 0751", fi.Mode().Perm())
	}
}

// Verify WithPreserveMode falls back to WithMode when target doesn't exist
func TestRefactor_PreserveMode_FallsBackToWithMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pm-new.txt")
	err := WriteFile(context.Background(), path, []byte("new"),
		WithMode(0o600), WithPreserveMode())
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("PreserveMode fallback; got %o, want 0600", fi.Mode().Perm())
	}
}

// Attack: concurrent PendingFile writes with various options under -race
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
			pf, err := NewPendingFile(path, 0o644, WithNoSync())
			if err != nil {
				return
			}
			defer pf.Cleanup()
			pf.Write([]byte(strings.Repeat("Z", idx+1)))
			pf.CommitFile()
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
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp leaked: %s", e.Name())
		}
	}
}
