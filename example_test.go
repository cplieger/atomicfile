package atomicfile_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cplieger/atomicfile"
)

func ExampleWriteFile() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "data.txt")
	_ = atomicfile.WriteFile(context.Background(), path, []byte("hello"))
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output: hello
}

func ExampleReadBounded() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "data.txt")
	os.WriteFile(path, []byte("bounded"), 0o644)
	data, _ := atomicfile.ReadBounded(context.Background(), path, 1<<20)
	fmt.Println(string(data))
	// Output: bounded
}

func ExampleSaveJSON() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	var mu sync.Mutex
	_ = atomicfile.SaveJSON(path, &mu, map[string]string{"key": "value"}, "example", 0o644)
	data, _ := os.ReadFile(path)
	fmt.Println(string(data))
	// Output:
	// {
	//   "key": "value"
	// }
}
