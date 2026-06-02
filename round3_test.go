package atomicfile

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// === VERIFY ROUND 1-2 FIXES ===

func TestRound3_SaturateAdd_MaxInt64(t *testing.T) {
	t.Parallel()
	got := saturateAdd(math.MaxInt64, 1)
	if got != math.MaxInt64 {
		t.Fatalf("saturateAdd(MaxInt64, 1) = %d, want MaxInt64", got)
	}
	got = saturateAdd(math.MaxInt64, math.MaxInt64)
	if got != math.MaxInt64 {
		t.Fatalf("saturateAdd(MaxInt64, MaxInt64) = %d, want MaxInt64", got)
	}
}

func TestRound3_ReadBounded_MaxInt64_NoPanic(t *testing.T) {
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

func TestRound3_Commit_NullByte_FinalPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp.txt")
	os.WriteFile(tmp, []byte("x"), 0o644)
	err := Commit(tmp, "/tmp/evil\x00path")
	if err == nil {
		t.Fatal("expected error for null byte in finalPath")
	}
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("expected ErrUnsafePath, got: %v", err)
	}
}

func TestRound3_Commit_NoSync_Verify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "final.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("nosync"), WithNoSync())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup()
	if err := Commit(tmpPath, path, WithNoSync()); err != nil {
		t.Fatalf("Commit(NoSync): %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "nosync" {
		t.Fatalf("got %q", got)
	}
}

// === TEMP LEAK ATTACKS ===

func TestRound3_WriteFile_ContextCancel_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ctx.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = WriteFile(ctx, path, []byte("data"))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestRound3_Prepare_ContextCancel_NoTempLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "prep.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := Prepare(ctx, path, []byte("data"))
	if err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestRound3_Commit_RenameFailure_CleansTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "isdir")
	os.Mkdir(target, 0o755)
	os.WriteFile(filepath.Join(target, "blocker"), []byte("x"), 0o644)

	tmp := filepath.Join(dir, "tmp-file.txt")
	os.WriteFile(tmp, []byte("data"), 0o644)

	err := Commit(tmp, target)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(tmp); !os.IsNotExist(statErr) {
		t.Errorf("temp file not cleaned after rename failure")
	}
}

// === CONCURRENT WRITERS UNDER -race ===

func TestRound3_ConcurrentWriteFile_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.txt")
	var wg sync.WaitGroup
	const N = 20
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			data := []byte(strings.Repeat("x", idx+1))
			_ = WriteFile(context.Background(), path, data)
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected content length: %d", len(got))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestRound3_ConcurrentPendingFile_Race(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pf-race.txt")
	var wg sync.WaitGroup
	const N = 15
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pf, err := NewPendingFile(path, 0o644)
			if err != nil {
				return
			}
			defer pf.Cleanup()
			pf.Write([]byte(strings.Repeat("y", idx+1)))
			pf.CommitFile()
		}(i)
	}
	wg.Wait()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) == 0 || len(got) > N {
		t.Fatalf("unexpected content length: %d", len(got))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

// === PENDINGFILE STATE MACHINE MISUSE ===

func TestRound3_PendingFile_DoubleCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "double.txt")
	pf, err := NewPendingFile(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	pf.Write([]byte("once"))
	if err := pf.CommitFile(); err != nil {
		t.Fatalf("first CommitFile: %v", err)
	}
	if err := pf.CommitFile(); err != nil {
		t.Fatalf("second CommitFile should be no-op: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "once" {
		t.Fatalf("got %q", got)
	}
}

func TestRound3_PendingFile_DoubleCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dblclean.txt")
	pf, err := NewPendingFile(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	pf.Write([]byte("data"))
	pf.Cleanup()
	err = pf.Cleanup()
	if err != nil {
		t.Fatalf("second Cleanup should be no-op: %v", err)
	}
}

func TestRound3_PendingFile_CleanupThenCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cleancommit.txt")
	pf, err := NewPendingFile(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	pf.Write([]byte("data"))
	pf.Cleanup()
	if err := pf.CommitFile(); err != nil {
		t.Fatalf("CommitFile after Cleanup should be no-op: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("file should not exist after Cleanup+Commit")
	}
}

// === READER ERROR ATTACKS ===

func TestRound3_WriteReader_UnexpectedEOF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ueof.txt")
	r := &errReader{n: 50, err: io.ErrUnexpectedEOF}
	err := WriteReader(context.Background(), path, r, 0o644)
	if err == nil {
		t.Fatal("expected error")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestRound3_WriteReader_EmptyReader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty-reader.txt")
	r := strings.NewReader("")
	err := WriteReader(context.Background(), path, r, 0o644)
	if err != nil {
		t.Fatalf("WriteReader(empty): %v", err)
	}
	got, _ := os.ReadFile(path)
	if len(got) != 0 {
		t.Fatalf("expected empty file, got %d bytes", len(got))
	}
}

// === PERMISSION/OWNER EDGE CASES ===

func TestRound3_WriteFile_ZeroPerm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "noperm.txt")
	err := WriteFile(context.Background(), path, []byte("secret"), WithMode(0o000))
	if err != nil {
		t.Fatalf("WriteFile(0o000): %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o000 {
		t.Errorf("mode = %o, want 0000", fi.Mode().Perm())
	}
	os.Chmod(path, 0o644)
}

func TestRound3_PreserveMode_UnusualPerm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "unusual.txt")
	os.WriteFile(path, []byte("old"), 0o751)
	err := WriteFile(context.Background(), path, []byte("new"), WithPreserveMode())
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o751 {
		t.Errorf("mode = %o, want 0751", fi.Mode().Perm())
	}
}

// === OVERSIZED/TRUNCATED READS ===

func TestRound3_ReadBounded_OneByteOver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "over.txt")
	os.WriteFile(path, []byte("abcde"), 0o644)
	_, err := ReadBounded(context.Background(), path, 4)
	if err == nil {
		t.Fatal("expected ErrFileTooLarge")
	}
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("expected ErrFileTooLarge, got: %v", err)
	}
}

func TestRound3_ReadBounded_ExactOneByte(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "one.txt")
	os.WriteFile(path, []byte("x"), 0o644)
	data, err := ReadBounded(context.Background(), path, 1)
	if err != nil {
		t.Fatalf("ReadBounded: %v", err)
	}
	if string(data) != "x" {
		t.Fatalf("got %q", data)
	}
}

func TestRound3_LoadJSON_ValidButOversized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.json")
	content := `{"key":"` + strings.Repeat("v", 90) + `"}`
	os.WriteFile(path, []byte(content), 0o644)
	var v map[string]string
	err := LoadJSON(context.Background(), path, 10, &v)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("expected ErrFileTooLarge, got: %v", err)
	}
}

// === EDGE CASES ===

func TestRound3_Commit_NonexistentTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	err := Commit("/nonexistent/tmp-file", target)
	if err == nil {
		t.Fatal("expected error for non-existent tmp")
	}
}

func TestRound3_WriteFile_LargeData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 256)
	}
	err := WriteFile(context.Background(), path, data)
	if err != nil {
		t.Fatalf("WriteFile(1MB): %v", err)
	}
	got, _ := os.ReadFile(path)
	if len(got) != len(data) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(data))
	}
}

func TestRound3_SaveJSON_NilOpts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nilopt.json")
	var mu sync.Mutex
	err := SaveJSON(path, &mu, map[string]int{"a": 1}, "test", 0o644)
	if err != nil {
		t.Fatalf("SaveJSON: %v", err)
	}
}

func TestRound3_Prepare_NilOpts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nilopt.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("data"))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup()
	got, _ := os.ReadFile(tmpPath)
	if string(got) != "data" {
		t.Fatalf("got %q", got)
	}
}

func TestRound3_Commit_EmptyTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	err := Commit("", target)
	if err == nil {
		t.Fatal("expected error for empty tmpPath")
	}
}
