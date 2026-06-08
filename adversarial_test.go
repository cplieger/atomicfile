package atomicfile

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// errReader is an io.Reader that returns an error after n bytes.
type errReader struct {
	err error
	n   int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, r.err
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.n -= len(p)
	return len(p), nil
}

func TestWriteReader_ErroringReader_CleansUpTemp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "errreader.txt")
	r := &errReader{n: 100, err: errors.New("simulated IO error")}
	err := WriteReader(context.Background(), path, r, 0o644)
	if err == nil {
		t.Fatal("expected error from erroring reader")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked after reader error: %s", e.Name())
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("target file should not exist after reader error")
	}
}

func TestPendingFile_AbandonedLeak(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "abandoned.txt")
	pf, err := NewPendingFile(path, 0o644)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	pf.Write([]byte("data that will be abandoned"))
	tmpName := pf.Name()

	if _, err := os.Stat(tmpName); err != nil {
		t.Fatalf("temp file should still exist when abandoned: %v", err)
	}

	pf.Close()
	os.Remove(tmpName)
}

func TestSaveJSON_ConcurrentSamePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.json")
	var mu sync.Mutex

	var wg sync.WaitGroup
	const goroutines = 10
	errs := make([]error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = SaveJSON(path, &mu, map[string]int{"i": idx}, "concurrent", 0o644)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var v map[string]int
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("final file is not valid JSON: %v\ncontent: %q", err, data)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("stale temp file: %s", e.Name())
		}
	}
}

func TestLoadJSON_TruncatedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated.json")
	os.WriteFile(path, []byte(`{"key": "val`), 0o644)
	var v map[string]string
	err := LoadJSON(context.Background(), path, 1<<20, &v)
	if err == nil {
		t.Fatal("expected error for truncated JSON")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		if !strings.Contains(err.Error(), "unexpected end") {
			t.Logf("error type: %T, value: %v", err, err)
		}
	}
}

func TestLoadJSON_ExactMaxBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "exact.json")
	data := []byte(`{"x":1}`)
	os.WriteFile(path, data, 0o644)
	var v map[string]int
	err := LoadJSON(context.Background(), path, 7, &v)
	if err != nil {
		t.Fatalf("LoadJSON at exact limit: %v", err)
	}
	if v["x"] != 1 {
		t.Errorf("got %v", v)
	}
}

func TestWriteFile_SymlinkInParentDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	realDir := filepath.Join(dir, "realdir")
	os.Mkdir(realDir, 0o755)

	linkDir := filepath.Join(dir, "linkdir")
	os.Symlink(realDir, linkDir)

	path := filepath.Join(linkDir, "file.txt")
	err := WriteFile(context.Background(), path, []byte("through symlink parent"))
	if err != nil {
		t.Fatalf("WriteFile through symlink parent: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(realDir, "file.txt"))
	if err != nil {
		t.Fatalf("ReadFile from realdir: %v", err)
	}
	if string(got) != "through symlink parent" {
		t.Errorf("got %q", got)
	}
}

func TestPreserveOwnership_WithoutPrivilege(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("test requires non-root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "owned.txt")
	os.WriteFile(path, []byte("old"), 0o644)

	err := WriteFile(context.Background(), path, []byte("new"), WithPreserveOwnership())
	if err != nil {
		t.Fatalf("PreserveOwnership to same user should succeed: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("got %q", got)
	}
}

type errWriterTo struct {
	err error
}

func (e *errWriterTo) Read(p []byte) (int, error) { return 0, e.err }
func (e *errWriterTo) WriteTo(w io.Writer) (int64, error) {
	w.Write([]byte("partial"))
	return 7, e.err
}

func TestWriteReader_WriterTo_Error_CleansUp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "writerto-err.txt")
	r := &errWriterTo{err: errors.New("WriterTo failure")}
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

func TestMkdirMode_BlockedByFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("I am a file"), 0o644)

	path := filepath.Join(blocker, "sub", "file.txt")
	err := WriteFile(context.Background(), path, []byte("data"), WithMkdirMode(0o755))
	if err == nil {
		t.Fatal("expected error when MkdirAll blocked by file")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

func TestReadBounded_ZeroMax(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nonempty.txt")
	os.WriteFile(path, []byte("x"), 0o644)
	_, err := ReadBounded(context.Background(), path, 0)
	if err == nil {
		t.Fatal("expected error for maxBytes=0 with non-empty file")
	}
}

func TestReadBounded_ZeroMax_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	os.WriteFile(path, nil, 0o644)
	data, err := ReadBounded(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ReadBounded(empty, 0): %v", err)
	}
	if len(data) != 0 {
		t.Errorf("got %d bytes", len(data))
	}
}

func TestNullByte_AllEntryPoints(t *testing.T) {
	t.Parallel()
	nullPath := "/tmp/test\x00evil"

	t.Run("WriteFile", func(t *testing.T) {
		err := WriteFile(context.Background(), nullPath, []byte("x"))
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("WriteReader", func(t *testing.T) {
		err := WriteReader(context.Background(), nullPath, strings.NewReader("x"), 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("SaveBytes", func(t *testing.T) {
		err := SaveBytes(nullPath, []byte("x"), 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("SaveJSON", func(t *testing.T) {
		var mu sync.Mutex
		err := SaveJSON(nullPath, &mu, "x", "test", 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("NewPendingFile", func(t *testing.T) {
		_, err := NewPendingFile(nullPath, 0o644)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("Prepare", func(t *testing.T) {
		_, _, err := Prepare(context.Background(), nullPath, []byte("x"))
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("Commit", func(t *testing.T) {
		err := Commit("/tmp/fake", nullPath)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("LoadJSON", func(t *testing.T) {
		var v any
		err := LoadJSON(context.Background(), nullPath, 1024, &v)
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("ReadBounded", func(t *testing.T) {
		_, err := ReadBounded(context.Background(), nullPath, 1024)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadBounded_NegativeMax(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	os.WriteFile(path, []byte("hello"), 0o644)
	_, err := ReadBounded(context.Background(), path, -1)
	if err == nil {
		t.Fatal("expected error for negative maxBytes")
	}
}

func TestReadBounded_NegativeMax_EmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	os.WriteFile(path, nil, 0o644)
	_, err := ReadBounded(context.Background(), path, -1)
	if err == nil {
		t.Fatal("expected error for negative maxBytes even on empty file")
	}
}

func TestSaturateAdd_EdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int64
		want int64
	}{
		{"zero+zero", 0, 0, 0},
		{"zero+one", 0, 1, 1},
		{"max+0", 9223372036854775807, 0, 9223372036854775807},
		{"max+1_saturates", 9223372036854775807, 1, 9223372036854775807},
		{"max-1+1", 9223372036854775806, 1, 9223372036854775807},
		{"max-1+2_saturates", 9223372036854775806, 2, 9223372036854775807},
		{"negative_a", -1, 1, 0},
		{"both_positive", 100, 200, 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := saturateAdd(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("saturateAdd(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestCommit_RespectsNoSync(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nosync-commit.txt")
	tmpPath, cleanup, err := Prepare(context.Background(), path, []byte("data"), WithNoSync())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer cleanup()
	if err := Commit(tmpPath, path, WithNoSync()); err != nil {
		t.Fatalf("Commit with NoSync: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "data" {
		t.Errorf("got %q, want %q", got, "data")
	}
}
