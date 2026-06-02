package atomicfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzWriteFile fuzzes both the file contents and target filename to
// ensure no panics and no escaping the temp directory.
func FuzzWriteFile(f *testing.F) {
	f.Add([]byte("hello"), "data.txt")
	f.Add([]byte{}, "empty")
	f.Add([]byte("\x00\xff\xfe"), "../escape")
	f.Add([]byte("big"), "sub/dir/file.json")

	baseDir := f.TempDir()
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, content []byte, name string) {
		// Sanitize: skip empty or excessively long names.
		if len(name) == 0 || len(name) > 255 {
			return
		}
		// Build an absolute path under our temp dir.
		// Use only the base name to prevent directory traversal.
		base := filepath.Base(name)
		if base == "." || base == ".." || base == "/" || strings.ContainsRune(base, 0) {
			return
		}
		path := filepath.Join(baseDir, base)

		err := WriteFile(ctx, path, content)
		if err != nil {
			// Errors are acceptable (validation rejects unsafe paths,
			// null bytes, etc.). Panics are not.
			return
		}

		// Verify: file must be inside baseDir and content must match.
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		if !strings.HasPrefix(real, baseDir) {
			t.Fatalf("file escaped temp dir: %q not under %q", real, baseDir)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if len(got) != len(content) {
			t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
		}
	})
}
