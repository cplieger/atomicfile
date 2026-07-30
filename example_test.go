package atomicfile_test

import (
	"context"
	"fmt"
	"io/fs"
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

func ExampleNewPendingFileInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	root, _ := os.OpenRoot(dir)
	defer func() { _ = root.Close() }()
	// The pending temp and its eventual rename are confined to the root's
	// tree; the caller keeps ownership of the root.
	pf, _ := atomicfile.NewPendingFileInRoot(context.Background(), root, "staged.txt")
	defer func() { _ = pf.Cleanup() }()
	_, _ = pf.Write([]byte("confined incremental"))
	res, _ := pf.Commit(context.Background())
	data, _ := os.ReadFile(res.Path)
	fmt.Println(string(data))
	// Output: confined incremental
}

func ExampleReadBoundedInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	_ = os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("-----BEGIN..."), 0o600)

	// A root confines every read to this tree: a symlink or ".." in the name cannot
	// redirect the read outside it.
	root, _ := os.OpenRoot(dir)
	defer root.Close()

	data, _ := atomicfile.ReadBoundedInRoot(context.Background(), root, "cert.pem", 1<<20)
	fmt.Println(len(data))
	// Output: 13
}

func ExampleCleanupStaleTempsInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	// An orphaned temp left by an interrupted write, in a NESTED directory: temps are
	// staged next to their target, so only a recursive sweep finds this one.
	nested := filepath.Join(dir, "example.com")
	_ = os.MkdirAll(nested, 0o750)
	stale := filepath.Join(nested, ".atomicfile-123456.tmp")
	_ = os.WriteFile(stale, []byte("partial"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	root, _ := os.OpenRoot(dir)
	defer root.Close()

	res, _ := atomicfile.CleanupStaleTempsInRoot(context.Background(), root, time.Hour, atomicfile.WithRecursive())
	fmt.Println(res.Removed, res.Failed, res.Unreadable)
	// Output: 1 0 0
}

func ExampleProbeWritable() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)

	res, err := atomicfile.ProbeWritable(context.Background(), dir)
	if err != nil {
		return // the probe could not be attempted (empty dir, cancelled ctx)
	}
	// The stage decides the policy, not the library: this caller refuses to
	// start when the directory cannot take a write, and only warns when the
	// write worked but the probe file could not be cleaned up.
	switch {
	case !res.Writable():
		fmt.Println("not writable:", res.Stage, res.Err)
	case !res.OK():
		fmt.Println("writable, but cleanup failed:", res.Stage, "leaked:", res.Leaked)
	default:
		fmt.Println("writable, nothing left behind")
	}
	// The probe file carries this package's temp shape, so a leaked one is
	// reclaimed by CleanupStaleTemps.
	fmt.Println(atomicfile.IsPackageTemp(res.Name))
	// Output:
	// writable, nothing left behind
	// true
}

func ExampleProbeWritableInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	root, _ := os.OpenRoot(dir)
	defer func() { _ = root.Close() }()

	// Probing through the root a caller already holds checks the same object
	// its writes will use; "." is the root itself.
	res, _ := atomicfile.ProbeWritableInRoot(context.Background(), root, ".")
	fmt.Println(res.OK(), res.Leaked)

	// An escaping name is refused by the root, reported as a stage failure.
	escaped, _ := atomicfile.ProbeWritableInRoot(context.Background(), root, "../outside")
	fmt.Println(escaped.Stage, escaped.Writable())
	// Output:
	// true false
	// create probe file false
}

func ExampleTempName() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)

	// A caller staging its own file in a directory this package sweeps takes
	// the name from TempName instead of rebuilding the convention, so an
	// orphan is reclaimed like any other temp.
	path := filepath.Join(dir, atomicfile.TempName())
	_ = os.WriteFile(path, []byte("mine"), 0o600)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(path, old, old)

	removed, _ := atomicfile.CleanupStaleTemps(dir, time.Hour)
	fmt.Println(atomicfile.IsPackageTemp(filepath.Base(path)), removed)
	// Output: true 1
}

func ExampleWalkDirInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	_ = os.MkdirAll(filepath.Join(dir, "example.com"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "example.com", "cert.pfx"), []byte("bundle"), 0o600)
	// A symlinked directory is reported and never descended, so nothing is
	// enumerated under a path whose ancestors do not physically hold it.
	_ = os.Symlink("example.com", filepath.Join(dir, "alias"))

	root, _ := os.OpenRoot(dir)
	defer root.Close()

	var found []string
	_ = atomicfile.WalkDirInRoot(context.Background(), root, func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			// A sub-path the walk cannot enter: count it and carry on with the
			// rest of the tree, or return the error to abort.
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(rel, ".pfx") {
			found = append(found, rel)
		}
		return nil
	})
	fmt.Println(found)
	// Output: [example.com/cert.pfx]
}

func ExampleRemoveFileInRoot() {
	dir, _ := os.MkdirTemp("", "example")
	defer os.RemoveAll(dir)
	_ = os.MkdirAll(filepath.Join(dir, "example.com"), 0o750)
	_ = os.WriteFile(filepath.Join(dir, "example.com", "cert.pfx"), []byte("bundle"), 0o600)

	root, _ := os.OpenRoot(dir)
	defer root.Close()

	// The unlink runs through a parent directory pinned component by component, so
	// an ancestor swapped for a symlink cannot redirect it at another file in the
	// tree — which a plain root.Remove of this same name would allow.
	err := atomicfile.RemoveFileInRoot(root, "example.com/cert.pfx")
	_, statErr := os.Lstat(filepath.Join(dir, "example.com", "cert.pfx"))
	fmt.Println(err, os.IsNotExist(statErr))
	// Output: <nil> true
}
