package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzWriteFile(f *testing.F) {
	f.Add([]byte("hello"), "data.txt")
	f.Add([]byte{}, "empty")
	f.Add([]byte("\x00\xff\xfe"), "../escape")
	f.Add([]byte("big"), "sub/dir/file.json")

	baseDir := f.TempDir()
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, content []byte, name string) {
		if len(name) == 0 || len(name) > 255 {
			return
		}
		base := filepath.Base(name)
		if base == "." || base == ".." || base == "/" || strings.ContainsRune(base, 0) {
			return
		}
		path := filepath.Join(baseDir, base)

		res, err := WriteFile(ctx, path, content)
		if err != nil {
			return
		}
		if res.Path != path {
			t.Fatalf("Result.Path = %q, want %q", res.Path, path)
		}

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
		if !bytes.Equal(got, content) {
			t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
		}
	})
}

func FuzzReadBounded(f *testing.F) {
	f.Add([]byte("hello world"), int64(100))
	f.Add([]byte("x"), int64(0))
	f.Add([]byte{}, int64(1))
	f.Add([]byte("\x00\xff"), int64(1))

	baseDir := f.TempDir()
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, content []byte, maxBytes int64) {
		if maxBytes < 0 {
			maxBytes = 0
		}
		path := filepath.Join(baseDir, "fuzz_read.dat")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		got, err := ReadBounded(ctx, path, maxBytes)
		if err != nil {
			if maxBytes < int64(len(content)) && errors.Is(err, ErrFileTooLarge) {
				return
			}
			// Other errors (e.g. path validation) are acceptable; panics are not.
			return
		}
		if int64(len(got)) > maxBytes {
			t.Fatalf("result length %d exceeds maxBytes %d", len(got), maxBytes)
		}
		if maxBytes >= int64(len(content)) && !bytes.Equal(got, content) {
			t.Fatalf("content mismatch when maxBytes >= len(content)")
		}
	})
}

func FuzzValidateAbsClean(f *testing.F) {
	f.Add("/tmp/safe")
	f.Add("/tmp/../etc/passwd")
	f.Add("relative/path")
	f.Add("/has\x00null")
	f.Add("")
	f.Add("/..")

	f.Fuzz(func(t *testing.T, path string) {
		clean, err := validateAbsClean(path)
		if err != nil {
			return
		}
		if !filepath.IsAbs(clean) {
			t.Fatalf("returned path is not absolute: %q", clean)
		}
		if strings.Contains(clean, "\x00") {
			t.Fatalf("returned path contains null byte: %q", clean)
		}
		for seg := range strings.SplitSeq(clean, string(filepath.Separator)) {
			if seg == ".." {
				t.Fatalf("returned path contains '..' segment: %q", clean)
			}
		}
	})
}

func FuzzWriteReader(f *testing.F) {
	f.Add([]byte("hello"))
	f.Add([]byte{})
	f.Add([]byte("\x00\xff\xfe\xfd"))

	baseDir := f.TempDir()
	ctx := context.Background()

	f.Fuzz(func(t *testing.T, content []byte) {
		path := filepath.Join(baseDir, "fuzz_writer.dat")

		if _, err := WriteReader(ctx, path, bytes.NewReader(content)); err != nil {
			return
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
		}

		// No temp file may leak after a successful write.
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), tempPrefix) && strings.HasSuffix(e.Name(), tempSuffix) {
				t.Fatalf("temp file leaked: %s", e.Name())
			}
		}
	})
}

func FuzzValidateRootName(f *testing.F) {
	f.Add("out.pfx")
	f.Add("a/../b.txt")
	f.Add("../escape")
	f.Add("/etc/passwd")
	f.Add("has\x00null")
	f.Add("")
	f.Add(".")
	f.Add("nested/deep/file")

	f.Fuzz(func(t *testing.T, name string) {
		clean, err := validateRootName(name)
		if err != nil {
			return
		}
		if clean == "" {
			t.Fatalf("validateRootName(%q) = %q with nil error, want non-empty", name, clean)
		}
		if filepath.IsAbs(clean) {
			t.Fatalf("validateRootName(%q) = %q, want a relative path", name, clean)
		}
		if strings.ContainsRune(clean, 0) {
			t.Fatalf("validateRootName(%q) = %q, want no null byte", name, clean)
		}
		if got := filepath.Clean(clean); got != clean {
			t.Fatalf("validateRootName(%q) = %q, want cleaned form %q", name, clean, got)
		}
	})
}
