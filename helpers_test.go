package atomicfile

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// isWindows reports whether the test is running on Windows, where POSIX file
// mode bits are not meaningful.
func isWindows() bool { return runtime.GOOS == "windows" }

// assertNoTempLeak fails t if dir contains any atomicfile temp artifacts.
func assertNoTempLeak(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") || strings.Contains(e.Name(), "atomicfile") {
			t.Errorf("temp file leaked: %s", e.Name())
		}
	}
}

// assertContent fails t unless path exists and holds exactly want.
func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v; the rename should have completed", path, err)
	}
	if string(got) != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

// stubFsyncDir replaces the package fsyncDir seam with one that returns err,
// restoring the original when the test finishes. Tests using it must not call
// t.Parallel: they mutate package state.
func stubFsyncDir(t *testing.T, err error) {
	t.Helper()
	orig := fsyncDir
	t.Cleanup(func() { fsyncDir = orig })
	fsyncDir = func(string) error { return err }
}

// stubOsChown replaces the package osChown seam with one that returns err,
// restoring the original when the test finishes. Tests using it must not call
// t.Parallel: they mutate package state.
func stubOsChown(t *testing.T, err error) {
	t.Helper()
	orig := osChown
	t.Cleanup(func() { osChown = orig })
	osChown = func(string, int, int) error { return err }
}

// stubFsyncRootDir replaces the package fsyncRootDir seam with one that returns
// err, restoring the original when the test finishes. Tests using it must not
// call t.Parallel: they mutate package state.
func stubFsyncRootDir(t *testing.T, err error) {
	t.Helper()
	orig := fsyncRootDir
	t.Cleanup(func() { fsyncRootDir = orig })
	fsyncRootDir = func(*os.Root, string) error { return err }
}

// openTestRoot makes a temp dir, opens it as an *os.Root, and registers the
// root's close. It returns the root and its directory path.
func openTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%q) = %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// plainReader wraps an io.Reader so the wrapper does NOT satisfy io.WriterTo,
// forcing WriteReader down the readerCtx (io.Copy) path.
type plainReader struct {
	r io.Reader
}

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

// errReader is an io.Reader that returns err after producing n bytes.
type errReader struct {
	err error
	n   int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, r.err
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.n -= len(p)
	return len(p), nil
}

// errWriterTo is an io.WriterTo that writes a partial chunk then fails, used to
// exercise the WriteReader io.WriterTo error path.
type errWriterTo struct {
	err error
}

func (e *errWriterTo) Read([]byte) (int, error) { return 0, e.err }

func (e *errWriterTo) WriteTo(w io.Writer) (int64, error) {
	n, _ := w.Write([]byte("partial"))
	return int64(n), e.err
}

// seqCancelCtx reports nil for the first cancelAt-1 calls to Err, then
// context.Canceled thereafter (1-indexed). It drives cancellation to a specific
// ctx.Err() checkpoint deep inside a synchronous call chain that exposes no
// other interleaving hook, letting tests cover the mid-barrier ctx guards
// inside finalizeTempFile.
type seqCancelCtx struct {
	context.Context
	mu       sync.Mutex
	calls    int
	cancelAt int
}

func (c *seqCancelCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
