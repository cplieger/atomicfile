package atomicfile_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
)

func ExampleWriteFile() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "data.txt")
	res, _ := atomicfile.WriteFile(context.Background(), path, []byte("hello"))
	data, _ := os.ReadFile(path)
	fmt.Println(string(data), res.Durable)
	// Output: hello true
}

func ExampleReadBounded() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "data.txt")
	_ = os.WriteFile(path, []byte("bounded"), 0o644)
	data, _ := atomicfile.ReadBounded(context.Background(), path, 1<<20)
	fmt.Println(string(data))
	// Output: bounded
}

func ExampleWriteReader() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "stream.txt")
	_, _ = atomicfile.WriteReader(context.Background(), path,
		strings.NewReader("streamed"), atomicfile.WithMode(0o600))
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: streamed
}

func ExampleNewPendingFile() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "pending.txt")
	pf, _ := atomicfile.NewPendingFile(context.Background(), path)
	defer func() { _ = pf.Cleanup() }()
	_, _ = pf.Write([]byte("incremental"))
	res, _ := pf.Commit(context.Background())
	data, _ := os.ReadFile(res.Path)
	fmt.Println(string(data))
	// Output: incremental
}

func ExampleCleanupStaleTemps() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	// Simulate an orphaned temp left by an interrupted write.
	stale := filepath.Join(dir, ".atomicfile-123456.tmp")
	_ = os.WriteFile(stale, []byte("partial"), 0o644)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	removed, _ := atomicfile.CleanupStaleTemps(dir, time.Hour)
	fmt.Println(removed)
	// Output: 1
}

func ExampleWriteFile_withMode() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "secret.txt")
	_, _ = atomicfile.WriteFile(context.Background(), path, []byte("s3cr3t"),
		atomicfile.WithMode(0o600))
	fi, _ := os.Stat(path)
	fmt.Println(fi.Mode().Perm())
	// Output: -rw-------
}

func ExampleWriteFile_withMkdirMode() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "nested", "dir", "file.txt")
	_, _ = atomicfile.WriteFile(context.Background(), path, []byte("deep"),
		atomicfile.WithMkdirMode(0o755))
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: deep
}

func ExampleWriteFile_withNoSync() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cache.txt")
	res, _ := atomicfile.WriteFile(context.Background(), path, []byte("fast"),
		atomicfile.WithNoSync())
	// WithNoSync trades durability for speed, so the result is not durable.
	fmt.Println(res.Durable)
	// Output: false
}

func ExampleWriteReader_withMode() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "stream.txt")
	_, _ = atomicfile.WriteReader(context.Background(), path,
		strings.NewReader("streamed"), atomicfile.WithMode(0o644))
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: streamed
}

func ExamplePendingFile() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "pending.txt")
	pf, _ := atomicfile.NewPendingFile(context.Background(), path)
	defer func() { _ = pf.Cleanup() }()
	_, _ = pf.Write([]byte("incremental"))
	_, _ = pf.Commit(context.Background())
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: incremental
}

func ExampleWriteFileInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	root, _ := os.OpenRoot(dir)
	defer func() { _ = root.Close() }()
	// name is relative to the root; a symlink or ".." in it cannot escape the
	// root's tree (Go 1.24+).
	_, _ = atomicfile.WriteFileInRoot(context.Background(), root, "rooted.txt",
		[]byte("confined"))
	// Read it back through the same root with the bounded-read seam.
	f, _ := root.Open("rooted.txt")
	defer func() { _ = f.Close() }()
	data, _ := atomicfile.ReadBoundedFile(context.Background(), f, 1<<20)
	fmt.Println(string(data))
	// Output: confined
}
