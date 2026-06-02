package atomicfile_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cplieger/atomicfile"
)

func ExampleWriteFile_withMode() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "secret.txt")
	_ = atomicfile.WriteFile(context.Background(), path, []byte("s3cr3t"),
		atomicfile.WithMode(0o600))
	fi, _ := os.Stat(path)
	fmt.Println(fi.Mode().Perm())
	// Output: -rw-------
}

func ExampleWriteFile_withMkdirMode() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "nested", "dir", "file.txt")
	_ = atomicfile.WriteFile(context.Background(), path, []byte("deep"),
		atomicfile.WithMkdirMode(0o755))
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: deep
}

func ExampleWriteFile_withNoSync() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cache.txt")
	_ = atomicfile.WriteFile(context.Background(), path, []byte("fast"),
		atomicfile.WithNoSync())
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: fast
}

func ExampleWriteReader() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "stream.txt")
	_ = atomicfile.WriteReader(context.Background(), path,
		strings.NewReader("streamed"), 0o644)
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: streamed
}

func ExampleNewPendingFile() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "pending.txt")
	pf, _ := atomicfile.NewPendingFile(path, 0o644)
	defer pf.Cleanup()
	pf.Write([]byte("incremental"))
	_ = pf.CommitFile()
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: incremental
}

func ExampleLoadJSON() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	var mu sync.Mutex
	_ = atomicfile.SaveJSON(path, &mu, map[string]string{"key": "value"}, "example", 0o644)
	var cfg map[string]string
	_ = atomicfile.LoadJSON(context.Background(), path, 1<<20, &cfg)
	fmt.Println(cfg["key"])
	// Output: value
}
