package atomicfile_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cplieger/atomicfile/v2"
)

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
