package atomicfile

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
