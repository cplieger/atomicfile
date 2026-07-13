package atomicfile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

	ctx := context.Background()

	f.Fuzz(func(t *testing.T, content []byte, maxBytes int64) {
		if maxBytes < 0 {
			maxBytes = 0
		}
		path := filepath.Join(t.TempDir(), "fuzz_read.dat")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		// The harness always writes a valid absolute path, so the only
		// behaviorally acceptable outcomes are an exact read when the file
		// fits within maxBytes or ErrFileTooLarge when it does not.
		got, err := ReadBounded(ctx, path, maxBytes)
		if int64(len(content)) > maxBytes {
			if !errors.Is(err, ErrFileTooLarge) {
				t.Fatalf("ReadBounded(%d bytes, max %d) = %v, want ErrFileTooLarge", len(content), maxBytes, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("ReadBounded(%d bytes, max %d) = %v, want nil", len(content), maxBytes, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("ReadBounded(%d bytes, max %d) returned different content", len(content), maxBytes)
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
		// Encode the acceptance/rejection oracle for CURRENT behavior: reject
		// empty and null-bearing inputs, reject anything that does not clean to
		// an absolute path, and reject a cleaned path that still holds a ".."
		// segment. Scanning segments of wantClean (not the raw input) tracks the
		// production post-Clean traversal check.
		wantErr := path == "" || strings.ContainsRune(path, 0)
		wantClean := ""
		if !wantErr {
			wantClean = filepath.Clean(path)
			if !filepath.IsAbs(wantClean) {
				wantErr = true
			}
			for seg := range strings.SplitSeq(wantClean, string(filepath.Separator)) {
				if seg == ".." {
					wantErr = true
					break
				}
			}
		}

		clean, err := validateAbsClean(path)
		if wantErr {
			if err == nil {
				t.Fatalf("validateAbsClean(%q) = %q, nil error; want rejection", path, clean)
			}
			return
		}
		if err != nil {
			t.Fatalf("validateAbsClean(%q) = error %v, want nil", path, err)
		}
		if clean != wantClean {
			t.Fatalf("validateAbsClean(%q) = %q, want %q", path, clean, wantClean)
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
		// Encode the acceptance/rejection oracle for CURRENT validateRootName
		// behavior: reject empty, null-bearing, and absolute names; otherwise
		// accept and return the cleaned relative form. An internal ".." that
		// stays inside the tree is allowed (the *os.Root refuses escapes at
		// operation time), so it is NOT a rejection here.
		wantErr := name == "" || strings.ContainsRune(name, 0) || filepath.IsAbs(name)
		wantClean := filepath.Clean(name)

		clean, err := validateRootName(name)
		if wantErr {
			if err == nil {
				t.Fatalf("validateRootName(%q) = %q, nil error; want rejection", name, clean)
			}
			return
		}
		if err != nil {
			t.Fatalf("validateRootName(%q) = error %v, want nil", name, err)
		}
		if clean != wantClean {
			t.Fatalf("validateRootName(%q) = %q, want cleaned form %q", name, clean, wantClean)
		}
		if filepath.IsAbs(clean) {
			t.Fatalf("validateRootName(%q) = %q, want a relative path", name, clean)
		}
	})
}

func FuzzIsStaleTempName(f *testing.F) {
	// Seed with the fixtures from TestIsStaleTempName.
	f.Add(".atomicfile-123456.tmp")
	f.Add(".atomicfile-7.tmp")
	f.Add(".atomicfile-notes.tmp")
	f.Add(".atomicfile-backup.tmp")
	f.Add(".atomicfile-12ab34.tmp")
	f.Add(".atomicfile-.tmp")
	f.Add(".atomicfilebackup.tmp")
	f.Add("config.tmp-backup")
	f.Add("")
	f.Add("a.tmp-\x00")

	f.Fuzz(func(t *testing.T, name string) {
		got := isStaleTempName(name)

		// Cross-check the structural invariant: a true result requires the exact
		// ".atomicfile-<digits>.tmp" shape with a non-empty all-digit middle.
		if got {
			if !strings.HasPrefix(name, tempPrefix) || !strings.HasSuffix(name, tempSuffix) {
				t.Fatalf("true but %q lacks the package prefix/suffix", name)
			}
			middle := name[len(tempPrefix) : len(name)-len(tempSuffix)]
			if !isAllDigits(middle) {
				t.Fatalf("true but middle %q is not all-digit", middle)
			}
		}
	})
}

func FuzzPendingFileRoundTrip(f *testing.F) {
	f.Add("file.txt", []byte("content"))
	f.Add("", []byte{})
	f.Add("../bad", []byte("x"))

	f.Fuzz(func(t *testing.T, name string, content []byte) {
		baseDir := t.TempDir()
		base := filepath.Base(name)
		path := filepath.Join(baseDir, base)
		if name == "" || base == "." || base == ".." {
			path = name // exercise validation
		}

		pf, err := NewPendingFile(context.Background(), path, WithNoSync())
		if err != nil {
			return
		}

		if _, err := pf.Write(content); err != nil {
			_ = pf.Cleanup()
			return
		}

		res, err := pf.Commit(context.Background())
		if err != nil {
			return
		}

		got, err := os.ReadFile(res.Path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("content mismatch")
		}

		// No temp file may leak in baseDir.
		entries, _ := os.ReadDir(baseDir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), tempPrefix) && strings.HasSuffix(e.Name(), tempSuffix) {
				t.Fatalf("temp file leaked: %s", e.Name())
			}
		}
	})
}

func FuzzCleanupStaleTemps(f *testing.F) {
	f.Add(".atomicfile-123456.tmp\nregular.json\n.atomicfile-notes.tmp", uint(60))
	f.Add(".atomicfile-987654321.tmp\n.atomicfilebackup.tmp\nkeep.json", uint(60))

	f.Fuzz(func(t *testing.T, fileNames string, maxAgeSec uint) {
		if maxAgeSec > 86400*365 {
			maxAgeSec = 86400 * 365
		}
		maxAge := time.Duration(maxAgeSec) * time.Second

		dir := t.TempDir()
		names := strings.Split(fileNames, "\n")
		if len(names) > 20 {
			names = names[:20]
		}

		created := make(map[string]bool)
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" || strings.ContainsAny(n, "/\\") || len(n) > 200 {
				continue
			}
			base := filepath.Base(n)
			if base == "." || base == ".." || created[base] {
				continue
			}
			p := filepath.Join(dir, base)
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				continue
			}
			// Backdate so any genuine stale temp qualifies for cleanup.
			old := time.Now().Add(-time.Duration(maxAgeSec+1) * time.Second)
			_ = os.Chtimes(p, old, old)
			created[base] = true
		}

		preEntries, _ := os.ReadDir(dir)
		preSet := make(map[string]bool, len(preEntries))
		for _, e := range preEntries {
			preSet[e.Name()] = true
		}

		removed, err := CleanupStaleTemps(dir, maxAge)
		if err != nil {
			t.Fatalf("CleanupStaleTemps: %v", err)
		}

		postEntries, _ := os.ReadDir(dir)
		postSet := make(map[string]bool, len(postEntries))
		for _, e := range postEntries {
			postSet[e.Name()] = true
		}

		// Only genuine stale temps may be removed, and the count must match.
		gone := 0
		for name := range preSet {
			if postSet[name] {
				continue
			}
			gone++
			if !isStaleTempName(name) {
				t.Fatalf("non-stale file %q was removed", name)
			}
		}
		if gone != removed {
			t.Fatalf("removed count = %d, but %d files disappeared", removed, gone)
		}
	})
}

func FuzzWriteFileInRoot(f *testing.F) {
	f.Add([]byte("payload"), "out.pfx")
	f.Add([]byte{}, "a/../b.txt")
	f.Add([]byte("x"), "../escape")
	f.Add([]byte("y"), "nested/deep/file")
	f.Add([]byte("z"), "/etc/passwd")
	f.Add([]byte("n"), "has\x00null")
	f.Add([]byte("\x00\xff"), "bin.dat")

	f.Fuzz(func(t *testing.T, content []byte, name string) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatalf("OpenRoot(%q) = %v", dir, err)
		}
		defer root.Close()

		res, err := WriteFileInRoot(context.Background(), root, name, content, WithMkdirMode(0o755))
		if err != nil {
			return
		}

		got, err := os.ReadFile(res.Path)
		if err != nil {
			t.Fatalf("ReadFile(%q) = %v", res.Path, err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("content mismatch for name %q: got %d bytes, want %d", name, len(got), len(content))
		}

		real, err := filepath.EvalSymlinks(res.Path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q) = %v", res.Path, err)
		}
		realDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks(%q) = %v", dir, err)
		}
		if !strings.HasPrefix(real, realDir) {
			t.Fatalf("write escaped root: %q not under %q", real, realDir)
		}
	})
}
