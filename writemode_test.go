package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempInDir returns the mode of the single staging file in dir, i.e. the entry
// that is not the target. It exists so a test can observe the temp WHILE the
// write is in flight, which is the only moment the exposure below is visible.
func tempInDir(t *testing.T, dir, target string) (string, os.FileMode) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.Name() == target {
			continue
		}
		fi, statErr := os.Lstat(filepath.Join(dir, e.Name()))
		if statErr != nil {
			t.Fatalf("lstat %s: %v", e.Name(), statErr)
		}
		return e.Name(), fi.Mode().Perm()
	}
	t.Fatalf("no staging file found in %s alongside %q", dir, target)
	return "", 0
}

// TestWrite_StagingFileIsOwnerOnlyBeforeAnyDataIsWritten pins the confidentiality
// window createTempInRoot's enforcement closes.
//
// The temp has to live in the TARGET's parent directory, because publishing is a
// same-filesystem rename. The 0o600 in its O_CREATE|O_EXCL is only a REQUEST, and
// on a filesystem carrying an inheritable group ACE the kernel stores 0770 —
// measured on the ZFS nfs4acl dataset this library is developed on. The caller's
// payload is written into that descriptor, so without enforcement at creation a
// secret written through WithMode(0o600) is group-readable AND group-writable for
// the whole duration of the write, and only narrowed afterwards.
//
// The observation happens INSIDE the reader, which the write engine pulls from
// between creating the temp and finalizing it. Asserting the mode after the write
// returned would prove nothing: by then finalizeTempFile has already applied the
// caller's mode, so the window would have closed whether or not it was ever open.
func TestWrite_StagingFileIsOwnerOnlyBeforeAnyDataIsWritten(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.json")

	var (
		observedName string
		observedMode os.FileMode
	)
	probe := readerFunc(func(p []byte) (int, error) {
		if observedName == "" {
			observedName, observedMode = tempInDir(t, dir, "secret.json")
		}
		return strings.NewReader("").Read(p)
	})

	if _, err := WriteReader(t.Context(), target, io.MultiReader(strings.NewReader("token=hunter2\n"), probe),
		WithMode(0o600)); err != nil {
		t.Fatalf("WriteReader: %v", err)
	}
	if observedName == "" {
		t.Fatal("the probe never ran, so nothing was observed mid-write")
	}
	if observedMode&0o077 != 0 {
		t.Errorf("staging file %s was mode %#o while the payload was being written: "+
			"group or other could read the cleartext, and write to it before the rename published it",
			observedName, observedMode)
	}
}

// readerFunc adapts a func to io.Reader so the test can run an assertion at the
// exact point the write engine pulls data.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestWrite_ModeMismatchIsAWriteErrorMatchingErrModeNotStored pins that the write
// path's mode step is a VERIFIED postcondition and that its failure stays
// classifiable. finalizeTempFile enforces on the handle, so a filesystem that
// refuses the mode fails the write instead of publishing a wider file and
// reporting success. The failure arrives as a *WriteError at PhaseTempChmod, and
// its chain must still match ErrModeNotStored — that is what lets a caller tell
// "the mode did not take" apart from every other write failure, and it only works
// because WriteError implements Unwrap.
//
// The mismatch leg is asserted against a constructed error rather than a real
// write, deliberately: no filesystem reachable from this test refuses a chmod
// (measured — the ZFS nfs4acl dataset widens the CREATE and honours the chmod),
// so the question worth pinning here is the error CHAIN the write path now
// returns, not EnforceMode's own comparison, which privatedir_test.go owns.
func TestWrite_ModeMismatchIsAWriteErrorMatchingErrModeNotStored(t *testing.T) {
	t.Parallel()

	werr := error(&WriteError{
		Phase: PhaseTempChmod,
		Err:   fmt.Errorf("%w: /x: asked for 0600, filesystem stored 0660", ErrModeNotStored),
	})
	if !errors.Is(werr, ErrModeNotStored) {
		t.Error("errors.Is(*WriteError, ErrModeNotStored) = false; a caller cannot tell a refused mode from any other write failure")
	}
	var we *WriteError
	if !errors.As(werr, &we) {
		t.Fatal("errors.As(*WriteError) = false")
	}
	if we.Phase != PhaseTempChmod {
		t.Errorf("phase = %v, want PhaseTempChmod", we.Phase)
	}

	// And the happy path: an explicit mode is what ends up on disk, read back
	// rather than assumed.
	target := filepath.Join(t.TempDir(), "cfg.json")
	if _, err := WriteFile(t.Context(), target, []byte("{}"), WithMode(0o600)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("published mode = %#o, want 0600", got)
	}
}
