package atomicfile

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireIntactTarget asserts the file at path still holds want, pinning the
// cap's core promise: a rejected write never disturbs the previous target.
func requireIntactTarget(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read target after rejected write: %v", err)
	}
	if string(got) != want {
		t.Errorf("target content = %q, want previous content %q intact", got, want)
	}
}

// requireNoTemps asserts no temp file survived in dir.
func requireNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".atomicfile-") {
			t.Errorf("leaked temp file %q after rejected capped write", e.Name())
		}
	}
}

func TestWriteFileMaxBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		data     string
		maxBytes int64
		wantErr  bool
	}{
		{name: "under cap", data: "abc", maxBytes: 4},
		{name: "exactly at cap", data: "abcd", maxBytes: 4},
		{name: "over cap rejected", data: "abcde", maxBytes: 4, wantErr: true},
		{name: "zero cap means uncapped", data: strings.Repeat("x", 1<<16), maxBytes: 0},
		{name: "negative cap means uncapped", data: "abc", maxBytes: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			const previous = "previous"
			if _, err := WriteFile(t.Context(), path, []byte(previous)); err != nil {
				t.Fatalf("seed write: %v", err)
			}
			_, err := WriteFile(t.Context(), path, []byte(tc.data), WithMaxBytes(tc.maxBytes))
			if tc.wantErr {
				if !errors.Is(err, ErrFileTooLarge) {
					t.Fatalf("err = %v, want ErrFileTooLarge", err)
				}
				requireIntactTarget(t, path, previous)
				requireNoTemps(t, dir)
				return
			}
			if err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			requireIntactTarget(t, path, tc.data)
		})
	}
}

func TestWriteReaderMaxBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	const previous = "previous"
	if _, err := WriteFile(t.Context(), path, []byte(previous)); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// A chunked reader without WriterTo forces the generic io.Copy loop
	// through the capping writer.
	over := io.LimitReader(neverEnding('x'), 100)
	_, err := WriteReader(t.Context(), path, over, WithMaxBytes(64))
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge through the WriteError wrap", err)
	}
	wErr, ok := errors.AsType[*WriteError](err)
	if !ok || wErr.Phase != PhaseTempWrite {
		t.Errorf("err = %v, want a *WriteError in PhaseTempWrite", err)
	}
	requireIntactTarget(t, path, previous)
	requireNoTemps(t, dir)

	// A WriterTo source exercises the fast path; the cap must hold there too.
	overWriterTo := bytes.NewReader(bytes.Repeat([]byte("y"), 100))
	if _, err := WriteReader(t.Context(), path, overWriterTo, WithMaxBytes(64)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("WriterTo path err = %v, want ErrFileTooLarge", err)
	}
	requireIntactTarget(t, path, previous)

	if _, err := WriteReader(t.Context(), path, strings.NewReader("abcd"), WithMaxBytes(4)); err != nil {
		t.Fatalf("at-cap WriteReader: %v", err)
	}
	requireIntactTarget(t, path, "abcd")
}

// TestWriteReaderMaxBytesReportsTheProjectedSize pins that the streaming
// cap's refusal reports the size the file would have REACHED, not the size
// of the crossing chunk: a caller's next question is always "by how much am
// I over", and the chunk size alone does not answer it.
func TestWriteReaderMaxBytesReportsTheProjectedSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f.txt")
	// Two 4-byte chunks against a 6-byte cap: the first is accepted, so the
	// second is refused with 4 bytes already staged behind it.
	chunked := io.MultiReader(strings.NewReader("abcd"), strings.NewReader("efgh"))

	_, err := WriteReader(t.Context(), path, chunked, WithMaxBytes(6))
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("WriteReader(two 4-byte chunks, cap 6) = %v, want ErrFileTooLarge", err)
	}
	if want := "would grow the file to 8 bytes (max 6)"; !strings.Contains(err.Error(), want) {
		t.Errorf("WriteReader(two 4-byte chunks, cap 6) error = %q, want it to contain %q", err, want)
	}
}

// neverEnding is an infinite reader of one repeated byte, chunked at the
// buffer io.Copy hands it.
type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func TestWriteFileInRootMaxBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	if _, err := WriteFileInRoot(t.Context(), root, "f.txt", []byte("abcde"), WithMaxBytes(4)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if _, statErr := root.Stat("f.txt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("target exists after rejected write (stat err = %v), want absent", statErr)
	}
	if _, err := WriteReaderInRoot(t.Context(), root, "f.txt", strings.NewReader("abcde"), WithMaxBytes(4)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("reader err = %v, want ErrFileTooLarge", err)
	}
}

func TestPendingFileMaxBytesRejectsWholeWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	pf, err := NewPendingFile(t.Context(), path, WithMaxBytes(8))
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() {
		if clErr := pf.Cleanup(); clErr != nil {
			t.Errorf("Cleanup: %v", clErr)
		}
	}()

	if _, err := pf.Write([]byte("12345")); err != nil {
		t.Fatalf("under-cap write: %v", err)
	}
	// This write would cross the cap: rejected WHOLE, nothing lands.
	n, err := pf.Write([]byte("6789X"))
	if n != 0 || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("crossing write = (%d, %v), want (0, ErrFileTooLarge)", n, err)
	}
	if want := "would grow the staged file to 10 bytes (max 8)"; !strings.Contains(err.Error(), want) {
		t.Errorf("crossing write error = %q, want it to contain %q", err, want)
	}
	if got := pf.BytesWritten(); got != 5 {
		t.Errorf("BytesWritten = %d, want 5 (rejected write must not advance the count)", got)
	}
	fi, err := os.Stat(pf.Name())
	if err != nil {
		t.Fatalf("stat temp: %v", err)
	}
	if fi.Size() != 5 {
		t.Errorf("temp size = %d, want 5: no byte of the rejected write may land", fi.Size())
	}
	if _, err := pf.Write([]byte("678")); err != nil {
		t.Fatalf("write up to cap: %v", err)
	}
	if _, err := pf.Write([]byte("9")); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("write past cap = %v, want ErrFileTooLarge", err)
	}
}

func TestPendingFileMaxBytesStreamSurface(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pf, err := NewPendingFile(t.Context(), filepath.Join(dir, "f.txt"), WithMaxBytes(4))
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() {
		if clErr := pf.Cleanup(); clErr != nil {
			t.Errorf("Cleanup: %v", clErr)
		}
	}()

	// WriteString routes through the capped Write (the embedded *os.File's
	// WriteString would bypass it).
	if _, err := io.WriteString(pf, "abcde"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("io.WriteString over cap = %v, want ErrFileTooLarge", err)
	}
	if _, err := io.Copy(pf, io.LimitReader(neverEnding('z'), 100)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("io.Copy over cap = %v, want ErrFileTooLarge", err)
	}
	if _, err := io.Copy(pf, bytes.NewReader(bytes.Repeat([]byte("w"), 100))); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("io.Copy (WriterTo source) over cap = %v, want ErrFileTooLarge", err)
	}
	if got := pf.BytesWritten(); got != 0 {
		t.Errorf("BytesWritten = %d after only rejected writes, want 0", got)
	}
	if _, err := io.Copy(pf, strings.NewReader("ab")); err != nil {
		t.Fatalf("under-cap io.Copy: %v", err)
	}
	if got := pf.BytesWritten(); got != 2 {
		t.Errorf("BytesWritten = %d, want 2", got)
	}
}

// TestPendingFileMaxBytesEncoderTruncateDance pins the driving consumer
// shape (a json.Encoder writing one buffer plus a trailing newline into a
// pending file capped at limit+1, with the newline truncated away).
func TestPendingFileMaxBytesEncoderTruncateDance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	const capBytes = 16
	payload := `{"k":"0123456"}` // 15 bytes < capBytes

	pf, err := NewPendingFile(t.Context(), path, WithMaxBytes(capBytes+1))
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte(payload + "\n")); err != nil {
		t.Fatalf("encoder-shaped write: %v", err)
	}
	if err := pf.Truncate(pf.BytesWritten() - 1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := pf.BytesWritten(); got != int64(len(payload)) {
		t.Errorf("BytesWritten after Truncate = %d, want %d (accounting re-synced)", got, len(payload))
	}
	if _, err := pf.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if string(got) != payload {
		t.Errorf("committed content = %q, want %q (newline truncated away)", got, payload)
	}
}

func TestPendingFileTruncateBeyondCapRejected(t *testing.T) {
	t.Parallel()
	pf, err := NewPendingFile(t.Context(), filepath.Join(t.TempDir(), "f.txt"), WithMaxBytes(4))
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if err := pf.Truncate(5); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Truncate beyond cap = %v, want ErrFileTooLarge", err)
	}
	if err := pf.Truncate(4); err != nil {
		t.Fatalf("Truncate at cap: %v", err)
	}
	if got := pf.BytesWritten(); got != 4 {
		t.Errorf("BytesWritten after extend-truncate = %d, want 4", got)
	}
}

// TestPendingFileUncappedTracksBytes pins that BytesWritten is meaningful
// for every PendingFile, not only capped ones.
func TestPendingFileUncappedTracksBytes(t *testing.T) {
	t.Parallel()
	pf, err := NewPendingFile(t.Context(), filepath.Join(t.TempDir(), "f.txt"))
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write([]byte("ab")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := io.Copy(pf, strings.NewReader("cdef")); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	if _, err := io.WriteString(pf, "gh"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if got := pf.BytesWritten(); got != 8 {
		t.Errorf("BytesWritten = %d, want 8", got)
	}
	if err := pf.Truncate(5); err != nil {
		t.Fatalf("Truncate(5) on an uncapped pending file = %v, want nil", err)
	}
	if got := pf.BytesWritten(); got != 5 {
		t.Errorf("BytesWritten after Truncate(5) = %d, want 5 (accounting re-synced)", got)
	}
}

// TestPendingFileMaxBytesCommitBarrier pins the barrier-side backstop for
// bytes staged outside the append-stream model: WriteAt, Write-after-Seek,
// and a reopen of the staged temp by path all evade the streaming cap, so
// Commit re-verifies the staged file's actual size. An exactly-at-cap
// staged size still publishes.
func TestPendingFileMaxBytesCommitBarrier(t *testing.T) {
	t.Parallel()
	const capBytes = 4
	cases := []struct {
		name    string
		stage   func(t *testing.T, pf *PendingFile)
		want    string // committed content for the success case
		wantErr bool
	}{
		{
			name: "WriteAt past cap",
			stage: func(t *testing.T, pf *PendingFile) {
				t.Helper()
				if _, err := pf.WriteAt([]byte("0123456789"), 0); err != nil {
					t.Fatalf("WriteAt: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "sparse WriteAt far past cap",
			stage: func(t *testing.T, pf *PendingFile) {
				t.Helper()
				if _, err := pf.WriteAt([]byte("x"), 63); err != nil {
					t.Fatalf("WriteAt: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "Write after Seek",
			stage: func(t *testing.T, pf *PendingFile) {
				t.Helper()
				if _, err := pf.Write([]byte("abcd")); err != nil { // at cap via the stream
					t.Fatalf("stream write: %v", err)
				}
				if _, err := pf.Seek(0, io.SeekStart); err != nil {
					t.Fatalf("Seek: %v", err)
				}
				// The embedded Write bypasses the capped override.
				if _, err := pf.File.Write([]byte("0123456789")); err != nil {
					t.Fatalf("embedded Write: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "reopen staged temp by path",
			stage: func(t *testing.T, pf *PendingFile) {
				t.Helper()
				f, err := os.OpenFile(pf.Name(), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatalf("reopen temp: %v", err)
				}
				if _, err := f.Write([]byte("0123456789")); err != nil {
					f.Close()
					t.Fatalf("write through reopened handle: %v", err)
				}
				if err := f.Close(); err != nil {
					t.Fatalf("close reopened handle: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "exactly at cap through WriteAt",
			stage: func(t *testing.T, pf *PendingFile) {
				t.Helper()
				if _, err := pf.WriteAt([]byte("abcd"), 0); err != nil {
					t.Fatalf("WriteAt: %v", err)
				}
			},
			want: "abcd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "f.txt")
			const previous = "previous"
			if _, err := WriteFile(t.Context(), path, []byte(previous)); err != nil {
				t.Fatalf("seed write: %v", err)
			}
			pf, err := NewPendingFile(t.Context(), path, WithMaxBytes(capBytes))
			if err != nil {
				t.Fatalf("NewPendingFile: %v", err)
			}
			defer func() {
				if clErr := pf.Cleanup(); clErr != nil {
					t.Errorf("Cleanup: %v", clErr)
				}
			}()
			tc.stage(t, pf)
			_, err = pf.Commit(t.Context())
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Commit = %v, want success at cap", err)
				}
				requireIntactTarget(t, path, tc.want)
				requireNoTemps(t, dir)
				return
			}
			if !errors.Is(err, ErrFileTooLarge) {
				t.Fatalf("Commit = %v, want ErrFileTooLarge", err)
			}
			if _, again := pf.Commit(t.Context()); !errors.Is(again, ErrFileTooLarge) {
				t.Errorf("repeated Commit = %v, want the cached ErrFileTooLarge", again)
			}
			requireIntactTarget(t, path, previous)
			requireNoTemps(t, dir)
		})
	}
}

// TestPendingFileInRootMaxBytesCommitBarrier pins that the commit-side cap
// verification also holds for a root-confined PendingFile.
func TestPendingFileInRootMaxBytesCommitBarrier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close()
	pf, err := NewPendingFileInRoot(t.Context(), root, "f.txt", WithMaxBytes(4))
	if err != nil {
		t.Fatalf("NewPendingFileInRoot: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.WriteAt([]byte("0123456789"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := pf.Commit(t.Context()); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Commit = %v, want ErrFileTooLarge", err)
	}
	if _, statErr := root.Stat("f.txt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("target exists after rejected commit (stat err = %v), want absent", statErr)
	}
	requireNoTemps(t, dir)
}

// TestPendingFileUncappedCommitIgnoresStagedSize pins that without a
// WithMaxBytes cap the commit barrier performs no size check.
func TestPendingFileUncappedCommitIgnoresStagedSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f.txt")
	pf, err := NewPendingFile(t.Context(), path)
	if err != nil {
		t.Fatalf("NewPendingFile: %v", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.WriteAt(bytes.Repeat([]byte("x"), 1<<10), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := pf.Commit(t.Context()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat committed file: %v", err)
	}
	if fi.Size() != 1<<10 {
		t.Errorf("committed size = %d, want %d", fi.Size(), 1<<10)
	}
}
