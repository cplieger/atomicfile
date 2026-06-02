package atomicfile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestNilOptionElement ensures a nil Option in the variadic slice is skipped
// rather than dereferenced (which would panic).
func TestNilOptionElement(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := WriteFile(context.Background(), p, []byte("x"), nil, WithMode(0o600)); err != nil {
		t.Fatalf("WriteFile with nil option element: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file not written: %v", err)
	}
}
