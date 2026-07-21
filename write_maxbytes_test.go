package atomicfile

import (
	"bytes"
	"context"
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

// requireNoTemps asserts no temp file survived in dir: a rejected capped
// write must not leak its staged temp.
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
			if _, err := WriteFile(context.Background(), path, []byte(previous)); err != nil {
				t.Fatalf("seed write: %v", err)
			}
			_, err := WriteFile(context.Background(), path, []byte(tc.data), WithMaxBytes(tc.maxBytes))
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
	if _, err := WriteFile(context.Background(), path, []byte(previous)); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// iotest-style chunked reader without WriterTo: forces the generic
	// io.Copy loop through the capping writer.
	over := io.LimitReader(neverEnding('x'), 100)
	_, err := WriteReader(context.Background(), path, over, WithMaxBytes(64))
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge through the WriteError wrap", err)
	}
	var wErr *WriteError
	if !errors.As(err, &wErr) || wErr.Phase != PhaseTempWrite {
		t.Errorf("err = %v, want a *WriteError in PhaseTempWrite", err)
	}
	requireIntactTarget(t, path, previous)
	requireNoTemps(t, dir)

	// A WriterTo source exercises the fast path; the cap must hold there too.
	overWriterTo := bytes.NewReader(bytes.Repeat([]byte("y"), 100))
	if _, err := WriteReader(context.Background(), path, overWriterTo, WithMaxBytes(64)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("WriterTo path err = %v, want ErrFileTooLarge", err)
	}
	requireIntactTarget(t, path, previous)

	// At-cap content passes.
	if _, err := WriteReader(context.Background(), path, strings.NewReader("abcd"), WithMaxBytes(4)); err != nil {
		t.Fatalf("at-cap WriteReader: %v", err)
	}
	requireIntactTarget(t, path, "abcd")
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
	if _, err := WriteFileInRoot(context.Background(), root, "f.txt", []byte("abcde"), WithMaxBytes(4)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v, want ErrFileTooLarge", err)
	}
	if _, statErr := root.Stat("f.txt"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("target exists after rejected write (stat err = %v), want absent", statErr)
	}
	if _, err := WriteReaderInRoot(context.Background(), root, "f.txt", strings.NewReader("abcde"), WithMaxBytes(4)); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("reader err = %v, want ErrFileTooLarge", err)
	}
}

func TestPendingFileMaxBytesRejectsWholeWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	pf, err := NewPendingFile(context.Background(), path, WithMaxBytes(8))
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
	// The stream is still usable up to the cap.
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
	pf, err := NewPendingFile(context.Background(), filepath.Join(dir, "f.txt"), WithMaxBytes(4))
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
	// io.Copy resolves to the ReadFrom override; the cap must hold on both
	// the generic and the WriterTo source path.
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
// shape (seadex-scout's state Save): a json.Encoder writes one buffer plus a
// trailing newline into a pending file capped at limit+1, the newline is
// truncated away, and the committed file is exactly the JSON size — while an
// encoding one byte past the cap is rejected before any byte lands.
func TestPendingFileMaxBytesEncoderTruncateDance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	const capBytes = 16
	payload := `{"k":"0123456"}` // 15 bytes < capBytes

	pf, err := NewPendingFile(context.Background(), path, WithMaxBytes(capBytes+1))
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
	if _, err := pf.Commit(context.Background()); err != nil {
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
	pf, err := NewPendingFile(context.Background(), filepath.Join(t.TempDir(), "f.txt"), WithMaxBytes(4))
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

// TestPendingFileUncappedTracksBytes pins that the accounting (and the
// *os.File ReadFrom fast path) work without a cap: BytesWritten is
// meaningful for every PendingFile, not only capped ones.
func TestPendingFileUncappedTracksBytes(t *testing.T) {
	t.Parallel()
	pf, err := NewPendingFile(context.Background(), filepath.Join(t.TempDir(), "f.txt"))
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
}
