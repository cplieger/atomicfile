package atomicfile

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// isWindows reports whether the test is running on Windows, where POSIX file
// mode bits are not meaningful.
func isWindows() bool { return runtime.GOOS == "windows" }

// Log messages asserted by the best-effort logging tests; sharing the literals
// keeps the tests in lockstep with the production strings they pin.
//
// msgStaleRemoved and msgStaleRemoveFail are the two aggregate lines
// CleanupStaleTemps used to emit before it returned a SweepResult. They are kept
// deliberately: the test that pins their ABSENCE needs the exact strings, and a
// regression that re-adds either line would otherwise pass.
const (
	msgRemoveTempFailed = "atomicfile: temp file cleanup failed"
	msgStaleRemoved     = "atomicfile.CleanupStaleTemps: removed stale temps"
	msgStaleRemoveFail  = "atomicfile.CleanupStaleTemps: some stale temps could not be removed"

	msgPrivateDirRepaired = "atomicfile: created directory did not keep the requested mode; repaired"
)

// replaceWithNonEmptyDir deletes the file at path and puts a non-empty
// directory in its place. os.Remove of a non-empty directory fails with a
// non-ErrNotExist error (ENOTEMPTY) on every platform and is NOT bypassed by
// root, so it forces a temp-removal to fail without any permission tricks.
func replaceWithNonEmptyDir(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove temp %q = %v, want nil", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %q = %v, want nil", path, err)
	}
	child := filepath.Join(path, "child")
	if err := os.WriteFile(child, []byte("x"), 0o644); err != nil {
		t.Fatalf("write child %q = %v, want nil", child, err)
	}
}

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

// stubFsyncRootDir replaces the package fsyncRootDir seam with one that returns
// err, restoring the original when the test finishes. Every write entry point
// (absolute-path adapters included) commits through this seam. Tests using it
// must not call t.Parallel: they mutate package state.
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

// captureHandler is a slog.Handler that records every emitted record, letting
// the best-effort logging tests assert which Debug/Info/Warn lines fired.
//
// It is deliberately atomicfile-local rather than a dependency on the fleet
// reference implementation, github.com/cplieger/slogx/capture (package
// capture in the slogx repo — compare against it when changing this helper):
// atomicfile is a foundational zero-dep library, and a ~40-line test recorder
// is not worth a new module edge — the same call auth made with its
// internal/capture copy.
// It matches the reference's semantics for the subset atomicfile exercises;
// the two gaps are deliberate and safe today:
//
//   - WithAttrs/WithGroup return the receiver unchanged, so derivation
//     context is NOT captured; atomicfile never logs through Logger.With or
//     WithGroup.
//   - Records are cloned verbatim, not normalized (no LogValuer resolution,
//     no empty-group elision); atomicfile logs only plain resolved
//     key-values.
//
// If production logging ever adopts With/WithGroup or LogValuer attrs,
// re-check this helper against slogx/capture's derivation semantics.
type captureHandler struct {
	records []slog.Record
	mu      sync.Mutex
}

// Enabled reports true for every level so nothing is filtered before capture.
func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle records a clone of r. It satisfies slog.Handler.
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// WithAttrs returns the receiver unchanged; base attributes are not captured.
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup returns the receiver unchanged; groups are not captured.
func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// Records returns a locked snapshot copy of the captured records, in order,
// matching the reference's snapshot-accessor contract (tests never read the
// raw slice).
func (h *captureHandler) Records() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.records)
}

// CountLevelExact returns how many captured records match both level and
// message exactly. It is the level-scoped exact matcher for log lines pinned
// by an external contract (the Loki alert shapes): exact equality on the
// message per the reference's CountExact rationale — a substring match would
// false-pass a positive must-emit assertion on a superstring message.
func (h *captureHandler) CountLevelExact(level slog.Level, message string) int {
	n := 0
	for _, r := range h.Records() {
		if r.Level == level && r.Message == message {
			n++
		}
	}
	return n
}

// CountLevel returns how many captured records were emitted at level,
// regardless of message. It is the matcher for an assertion about a level's
// PRESENCE or ABSENCE as a whole — "no aggregate Warn at all" — where naming a
// message would let a reworded or newly added line slip through.
func (h *captureHandler) CountLevel(level slog.Level) int {
	n := 0
	for _, r := range h.Records() {
		if r.Level == level {
			n++
		}
	}
	return n
}

// recordFsyncRootDir replaces the package fsyncRootDir seam with one that
// records every (root name, dir) pair it is asked to sync and then runs the
// original, restoring it when the test finishes. It returns a function that
// reports the recorded dirs in call order.
//
// Tests using it must not call t.Parallel: they mutate package state.
func recordFsyncRootDir(t *testing.T) func() []string {
	t.Helper()
	orig := fsyncRootDir
	t.Cleanup(func() { fsyncRootDir = orig })
	var mu sync.Mutex
	var seen []string
	fsyncRootDir = func(root *os.Root, dir string) error {
		mu.Lock()
		seen = append(seen, dir)
		mu.Unlock()
		return orig(root, dir)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// withUmask sets the process umask for the duration of the test and restores it
// afterwards. The umask is process-global, so a test using it must not call
// t.Parallel.
//
// It exists because a mode passed to mkdir(2) is a REQUEST that umask narrows,
// which is the whole reason mkdirAllInRoot enforces the mode instead of trusting
// it. A test run under the usual 022 cannot tell an enforced mode from a
// requested one.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	prev := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(prev) })
}

// assertDirMode fails t unless the directory at path stores exactly want in the
// bits chmod(2) can set.
func assertDirMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) = %v", path, err)
	}
	if !fi.IsDir() {
		t.Fatalf("Stat(%q) is not a directory (mode %s)", path, fi.Mode())
	}
	if got := chmodBits(fi.Mode()); got != want {
		t.Errorf("mode of %q = %#o, want %#o", path, got, want)
	}
}

// captureWarn returns a logger that records Warn and above into a buffer, plus a
// function that reports what has been logged. It is an explicit logger passed
// through WithLogger, not a swap of slog.Default(), so it needs no serialization
// of its own.
func captureWarn() (logger *slog.Logger, logged func() string) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(h), buf.String
}

// occupyOnRead runs occupy once, on the first Read, before delegating. It exists
// to land a filesystem change in the window between the pre-write target guard
// and the rename — the one window the guard cannot close — so the PhaseRename arm
// stays reachable deterministically. It deliberately does NOT implement
// io.WriterTo, keeping the copy on the per-Read path.
type occupyOnRead struct {
	r      io.Reader
	occupy func()
	done   bool
}

func (o *occupyOnRead) Read(p []byte) (int, error) {
	if !o.done {
		o.done = true
		o.occupy()
	}
	return o.r.Read(p)
}
