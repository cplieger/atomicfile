package atomicfile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func FuzzIsStaleTempName(f *testing.F) {
	// Seed with test fixtures from TestIsStaleTempName.
	f.Add("data.json.tmp-abc123")
	f.Add("regular.json")
	f.Add("alice.tmp-2024-notes.json")
	f.Add("foo.tmp-a/b")
	f.Add("just.tmp-")
	f.Add("")
	f.Add(".tmp-x")
	f.Add("a.tmp-\x00")

	f.Fuzz(func(t *testing.T, name string) {
		got := isStaleTempName(name, "")

		// Cross-check against known fixtures.
		switch name {
		case "data.json.tmp-abc123":
			if !got {
				t.Fatal("expected true for known positive fixture")
			}
		case "regular.json", "alice.tmp-2024-notes.json", "foo.tmp-a/b", "just.tmp-", "":
			if got {
				t.Fatalf("expected false for known negative fixture %q", name)
			}
		}

		// If true, verify structural invariants for whichever convention matched.
		if got {
			pre, suf, _ := strings.Cut(DefaultTempPrefix, "*")
			defaultStyle := len(name) > len(pre)+len(suf) &&
				strings.HasPrefix(name, pre) && strings.HasSuffix(name, suf)
			if !defaultStyle {
				tag := ".tmp-"
				i := strings.LastIndex(name, tag)
				if i < 0 {
					t.Fatal("true but no recognized temp signature")
				}
				tail := name[i+len(tag):]
				if len(tail) == 0 {
					t.Fatal("true but empty tail")
				}
				if strings.ContainsAny(tail, "./\\") {
					t.Fatal("true but tail has forbidden chars")
				}
			}
		}
	})
}

func FuzzCommit(f *testing.F) {
	f.Add([]byte("hello"), "final.txt")
	f.Add([]byte{}, "../escape")
	f.Add([]byte("x"), "")
	f.Add([]byte("data"), "sub/deep/file")

	f.Fuzz(func(t *testing.T, content []byte, finalName string) {
		baseDir := t.TempDir()

		// Create a real temp file with fuzzed content.
		tmp, err := os.CreateTemp(baseDir, "fuzz-commit-*.tmp")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(content); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return
		}
		tmp.Close()

		// Build finalPath from fuzzed name (absolute under baseDir).
		base := filepath.Base(finalName)
		finalPath := filepath.Join(baseDir, base)
		if finalName == "" || base == "." || base == ".." {
			finalPath = finalName // exercise validation
		}

		err = Commit(tmpPath, finalPath, WithNoSync())
		if err != nil {
			return
		}

		// Success: finalPath must be absolute and clean.
		clean, vErr := validateAbsClean(finalPath)
		if vErr != nil {
			t.Fatalf("Commit succeeded but finalPath invalid: %v", vErr)
		}
		if !filepath.IsAbs(clean) {
			t.Fatalf("Commit succeeded but path not absolute: %q", clean)
		}

		// Verify file contents.
		got, err := os.ReadFile(clean)
		if err != nil {
			t.Fatalf("ReadFile after Commit: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("content mismatch after Commit")
		}
	})
}

func FuzzSaveJSON(f *testing.F) {
	f.Add("/tmp/test.json", "hello", 42)
	f.Add("relative", "", 0)
	f.Add("/tmp/\x00bad", "x", 1)
	f.Add("/tmp/deep/nested/f.json", strings.Repeat("a", 100), 999)

	type payload struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	f.Fuzz(func(t *testing.T, pathSuffix, name string, value int) {
		var mu sync.Mutex
		dir := t.TempDir()
		base := filepath.Base(pathSuffix)
		path := filepath.Join(dir, base)
		if pathSuffix == "" || base == "." || base == ".." {
			path = pathSuffix // exercise validation
		}

		v := payload{Name: name, Value: value}
		err := SaveJSON(path, &mu, v, "fuzz", 0o644, WithNoSync())
		if err != nil {
			return
		}

		// Round-trip: read back and verify JSON is valid.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		var got payload
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Value != v.Value {
			t.Fatalf("round-trip Value mismatch: got %d, want %d", got.Value, v.Value)
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

		pf, err := NewPendingFile(path, 0o644, WithNoSync())
		if err != nil {
			return
		}

		if _, err := pf.Write(content); err != nil {
			pf.Cleanup()
			return
		}

		err = pf.CommitFile()
		if err != nil {
			return
		}

		// Use cleaned path for verification (NewPendingFile cleans internally).
		cleanPath := filepath.Clean(path)
		got, err := os.ReadFile(cleanPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("content mismatch")
		}

		// Verify no temp file leaks in baseDir.
		entries, _ := os.ReadDir(baseDir)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".atomicfile-") && strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("temp file leaked: %s", e.Name())
			}
		}
	})
}

func FuzzCleanupStaleTemps(f *testing.F) {
	f.Add("data.json.tmp-abc123\nregular.json\nfoo.tmp-xyz", uint(60))
	f.Add(".atomicfile-987654321.tmp\n.atomicfilebackup.tmp\nkeep.json", uint(60))

	f.Fuzz(func(t *testing.T, fileNames string, maxAgeSec uint) {
		if maxAgeSec > 86400*365 {
			maxAgeSec = 86400 * 365
		}
		maxAge := time.Duration(maxAgeSec) * time.Second

		dir := t.TempDir()
		names := strings.Split(fileNames, "\n")

		// Create files, limit count to avoid excessive IO.
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
			if base == "." || base == ".." {
				continue
			}
			p := filepath.Join(dir, base)
			if created[base] {
				continue
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				continue
			}
			// Make file old so it qualifies for cleanup.
			old := time.Now().Add(-time.Duration(maxAgeSec+1) * time.Second)
			os.Chtimes(p, old, old)
			created[base] = true
		}

		// Record pre-state.
		preEntries, _ := os.ReadDir(dir)
		preSet := make(map[string]bool, len(preEntries))
		for _, e := range preEntries {
			preSet[e.Name()] = true
		}

		CleanupStaleTemps(dir, maxAge, WithNoSync())

		// Verify: only stale temps older than maxAge should be removed.
		postEntries, _ := os.ReadDir(dir)
		postSet := make(map[string]bool, len(postEntries))
		for _, e := range postEntries {
			postSet[e.Name()] = true
		}

		for name := range preSet {
			removed := !postSet[name]
			isStale := isStaleTempName(name, "")
			if removed && !isStale {
				t.Fatalf("non-stale file %q was removed", name)
			}
		}
	})
}
