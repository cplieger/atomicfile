package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// stubEnforceMode makes enforceMode behave like a filesystem whose inherited
// ACL widens every requested mode, restoring the real one on test end. The
// returned func reports the modes it was asked for, in call order. Callers
// must not use t.Parallel: package state.
func stubEnforceMode(t *testing.T) func() []os.FileMode {
	t.Helper()
	orig := enforceMode
	t.Cleanup(func() { enforceMode = orig })
	var asked []os.FileMode
	enforceMode = func(f *os.File, want os.FileMode) (os.FileMode, error) {
		asked = append(asked, want)
		stored := want | 0o070
		return stored, fmt.Errorf("%w: %s: asked for %#o, filesystem stored %#o",
			ErrModeNotStored, f.Name(), want, stored)
	}
	return func() []os.FileMode { return asked }
}

func TestWriteFile_WithoutWithMode_TheUmaskDecidesTheMode(t *testing.T) {
	cases := []struct {
		name string
		mask int
		want os.FileMode
	}{
		{name: "umask077", mask: 0o077, want: 0o600},
		{name: "umask022", mask: 0o022, want: 0o644},
		{name: "umask002", mask: 0o002, want: 0o664},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withUmask(t, tc.mask)
			path := filepath.Join(t.TempDir(), "f.txt")
			if _, err := WriteFile(t.Context(), path, []byte("x")); err != nil {
				t.Fatalf("WriteFile(no options) under umask %#o = %v", tc.mask, err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("WriteFile(no options) under umask %#o published mode %#o, want %#o", tc.mask, got, tc.want)
			}
		})
	}
}

// The observation runs inside the reader the engine pulls from, between
// creating the staging file and finalizing it.
func TestWrite_WithoutWithMode_StagingFileIsCreatedLikeOSCreate(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	target := filepath.Join(dir, "page.html")

	var (
		observedName string
		observedMode os.FileMode
	)
	probe := readerFunc(func(p []byte) (int, error) {
		if observedName == "" {
			observedName, observedMode = tempInDir(t, dir, "page.html")
		}
		return strings.NewReader("").Read(p)
	})
	if _, err := WriteReader(t.Context(), target, io.MultiReader(strings.NewReader("<p>hi</p>"), probe)); err != nil {
		t.Fatalf("WriteReader: %v", err)
	}
	if observedName == "" {
		t.Fatal("the probe never ran, so nothing was observed mid-write")
	}
	if observedMode != 0o644 {
		t.Errorf("staging file %s was mode %#o mid-write under umask 022, want 0644", observedName, observedMode)
	}
}

// writeEntryPoint runs one write entry point against dir/target.txt, writing
// "new".
type writeEntryPoint struct {
	run  func(ctx context.Context, dir string, opts ...Option) error
	name string
}

func writeEntryPoints() []writeEntryPoint {
	const name = "target.txt"
	inRoot := func(dir string, fn func(root *os.Root) error) error {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		defer root.Close()
		return fn(root)
	}
	commit := func(ctx context.Context, pf *PendingFile, err error) error {
		if err != nil {
			return err
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("new")); err != nil {
			return err
		}
		_, err = pf.Commit(ctx)
		return err
	}
	return []writeEntryPoint{
		{name: "WriteFile", run: func(ctx context.Context, dir string, opts ...Option) error {
			_, err := WriteFile(ctx, filepath.Join(dir, name), []byte("new"), opts...)
			return err
		}},
		{name: "WriteReader", run: func(ctx context.Context, dir string, opts ...Option) error {
			_, err := WriteReader(ctx, filepath.Join(dir, name), strings.NewReader("new"), opts...)
			return err
		}},
		{name: "NewPendingFile", run: func(ctx context.Context, dir string, opts ...Option) error {
			pf, err := NewPendingFile(ctx, filepath.Join(dir, name), opts...)
			return commit(ctx, pf, err)
		}},
		{name: "WriteFileInRoot", run: func(ctx context.Context, dir string, opts ...Option) error {
			return inRoot(dir, func(root *os.Root) error {
				_, err := WriteFileInRoot(ctx, root, name, []byte("new"), opts...)
				return err
			})
		}},
		{name: "WriteReaderInRoot", run: func(ctx context.Context, dir string, opts ...Option) error {
			return inRoot(dir, func(root *os.Root) error {
				_, err := WriteReaderInRoot(ctx, root, name, strings.NewReader("new"), opts...)
				return err
			})
		}},
		{name: "NewPendingFileInRoot", run: func(ctx context.Context, dir string, opts ...Option) error {
			return inRoot(dir, func(root *os.Root) error {
				pf, err := NewPendingFileInRoot(ctx, root, name, opts...)
				return commit(ctx, pf, err)
			})
		}},
	}
}

func seedTarget(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("Setup: seed target: %v", err)
	}
	return path
}

func TestWrite_WithoutWithMode_SucceedsOnAWideningFilesystem(t *testing.T) {
	for _, ep := range writeEntryPoints() {
		t.Run(ep.name, func(t *testing.T) {
			asked := stubEnforceMode(t)
			dir := t.TempDir()
			path := seedTarget(t, dir)
			if err := ep.run(t.Context(), dir); err != nil {
				t.Fatalf("%s(no WithMode) on a widening filesystem = %v, want nil", ep.name, err)
			}
			if got := asked(); len(got) != 0 {
				t.Errorf("%s(no WithMode) asked enforceMode for %#o, want no mode enforced", ep.name, got)
			}
			assertContent(t, path, "new")
			assertNoTempLeak(t, dir)
		})
	}
}

func TestWrite_WithMode_FailsOnAWideningFilesystem(t *testing.T) {
	for _, ep := range writeEntryPoints() {
		t.Run(ep.name, func(t *testing.T) {
			asked := stubEnforceMode(t)
			dir := t.TempDir()
			path := seedTarget(t, dir)
			err := ep.run(t.Context(), dir, WithMode(0o600))
			if !errors.Is(err, ErrModeNotStored) {
				t.Fatalf("%s(WithMode(0o600)) on a widening filesystem = %v, want errors.Is ErrModeNotStored", ep.name, err)
			}
			if we, ok := errors.AsType[*WriteError](err); !ok || we.Phase != PhaseTempCreate {
				t.Errorf("%s(WithMode(0o600)) on a widening filesystem = %v, want a *WriteError at PhaseTempCreate (refused before any data is staged)", ep.name, err)
			}
			if got := asked(); len(got) != 1 || got[0] != 0o600 {
				t.Errorf("%s(WithMode(0o600)) asked enforceMode for %#o, want exactly [0600] (the staging file)", ep.name, got)
			}
			assertContent(t, path, "old")
			assertNoTempLeak(t, dir)
		})
	}
}

func TestWriteFile_WithMode_RefusedFinalModeFailsAtPhaseTempChmod(t *testing.T) {
	orig := enforceMode
	t.Cleanup(func() { enforceMode = orig })
	enforceMode = func(f *os.File, want os.FileMode) (os.FileMode, error) {
		if want == 0o600 {
			return orig(f, want)
		}
		return want | 0o070, fmt.Errorf("%w: %s: asked for %#o", ErrModeNotStored, f.Name(), want)
	}
	dir := t.TempDir()
	path := seedTarget(t, dir)
	_, err := WriteFile(t.Context(), path, []byte("new"), WithMode(0o640))
	if we, ok := errors.AsType[*WriteError](err); !ok || we.Phase != PhaseTempChmod || !errors.Is(err, ErrModeNotStored) {
		t.Fatalf("WriteFile(WithMode(0o640)) with the final mode refused = %v, want a *WriteError at PhaseTempChmod matching ErrModeNotStored", err)
	}
	assertContent(t, path, "old")
	assertNoTempLeak(t, dir)
}

func TestProbeWritable_FollowsTheWriteModeRule(t *testing.T) {
	t.Run("without_WithMode", func(t *testing.T) {
		asked := stubEnforceMode(t)
		dir := t.TempDir()
		res, err := ProbeWritable(t.Context(), dir)
		if err != nil || !res.OK() {
			t.Fatalf("ProbeWritable(no WithMode) on a widening filesystem = %+v, %v; want OK", res, err)
		}
		if got := asked(); len(got) != 0 {
			t.Errorf("ProbeWritable(no WithMode) asked enforceMode for %#o, want no mode enforced", got)
		}
		assertNoTempLeak(t, dir)
	})
	t.Run("with_WithMode", func(t *testing.T) {
		stubEnforceMode(t)
		dir := t.TempDir()
		res, err := ProbeWritable(t.Context(), dir, WithMode(0o600))
		if err != nil {
			t.Fatalf("ProbeWritable(WithMode(0o600)) error = %v, want the outcome in ProbeResult", err)
		}
		if res.Stage != ProbeStageCreate || !errors.Is(res.Err, ErrModeNotStored) {
			t.Errorf("ProbeWritable(WithMode(0o600)) on a widening filesystem = Stage %v, Err %v; want ProbeStageCreate matching ErrModeNotStored",
				res.Stage, res.Err)
		}
		if res.Leaked {
			t.Error("ProbeWritable(WithMode(0o600)) reports a leaked probe file, want the refused temp removed")
		}
		assertNoTempLeak(t, dir)
	})
}

func TestWrite_WithMkdirModeButNoWithMode_EnforcesTheDirectoryOnly(t *testing.T) {
	t.Run("widening_filesystem", func(t *testing.T) {
		asked := stubEnforceMode(t)
		path := filepath.Join(t.TempDir(), "sub", "f.txt")
		_, err := WriteFile(t.Context(), path, []byte("x"), WithMkdirMode(0o750))
		if !errors.Is(err, ErrModeNotStored) {
			t.Fatalf("WriteFile(WithMkdirMode(0o750)) on a widening filesystem = %v, want errors.Is ErrModeNotStored", err)
		}
		if got := asked(); len(got) != 1 || got[0] != 0o750 {
			t.Errorf("enforceMode asked for %#o, want exactly [0750] (the created directory)", got)
		}
	})
	t.Run("umask077", func(t *testing.T) {
		withUmask(t, 0o077)
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "f.txt")
		if _, err := WriteFile(t.Context(), path, []byte("x"), WithMkdirMode(0o750)); err != nil {
			t.Fatalf("WriteFile(WithMkdirMode(0o750)) under umask 077 = %v", err)
		}
		assertDirMode(t, filepath.Join(dir, "sub"), 0o750)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("file mode under umask 077 without WithMode = %#o, want 0600", got)
		}
	})
}

func TestWriteFile_WithoutWithMode_KeepsEveryOtherRefusal(t *testing.T) {
	cases := []struct {
		setup   func(t *testing.T, dir string) (path string, unchanged func() error)
		ctx     func(t *testing.T) context.Context
		wantErr error
		name    string
		opts    []Option
	}{
		{
			name:    "symlink_target",
			wantErr: ErrSymlinkTarget,
			setup: func(t *testing.T, dir string) (string, func() error) {
				realPath := seedTarget(t, dir)
				link := filepath.Join(dir, "link.txt")
				if err := os.Symlink(realPath, link); err != nil {
					t.Fatalf("Setup: symlink: %v", err)
				}
				return link, func() error { return holds(realPath, "old") }
			},
		},
		{
			name:    "fifo_target",
			wantErr: ErrNotRegular,
			setup: func(t *testing.T, dir string) (string, func() error) {
				fifo := filepath.Join(dir, "pipe")
				if err := syscall.Mkfifo(fifo, 0o600); err != nil {
					t.Fatalf("Setup: mkfifo: %v", err)
				}
				return fifo, func() error {
					fi, err := os.Lstat(fifo)
					if err != nil {
						return err
					}
					if fi.Mode()&os.ModeNamedPipe == 0 {
						return fmt.Errorf("%s is now %v, want the FIFO left in place", fifo, fi.Mode())
					}
					return nil
				}
			},
		},
		{
			name:    "over_WithMaxBytes",
			wantErr: ErrFileTooLarge,
			opts:    []Option{WithMaxBytes(2)},
			setup: func(t *testing.T, dir string) (string, func() error) {
				path := seedTarget(t, dir)
				return path, func() error { return holds(path, "old") }
			},
		},
		{
			name:    "cancelled_ctx",
			wantErr: context.Canceled,
			ctx: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			setup: func(t *testing.T, dir string) (string, func() error) {
				path := seedTarget(t, dir)
				return path, func() error { return holds(path, "old") }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, unchanged := tc.setup(t, dir)
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}
			_, err := WriteFile(ctx, path, []byte("new"), tc.opts...)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WriteFile(%s, no WithMode) = %v, want errors.Is %v", tc.name, err, tc.wantErr)
			}
			if uErr := unchanged(); uErr != nil {
				t.Errorf("WriteFile(%s, no WithMode) disturbed the target: %v", tc.name, uErr)
			}
			assertNoTempLeak(t, dir)
		})
	}
}

func TestWriteFile_WithoutWithMode_ReportsADirFsyncFailureAsNotDurable(t *testing.T) {
	stubFsyncRootDir(t, errors.New("injected dir fsync failure"))
	path := filepath.Join(t.TempDir(), "f.txt")
	res, err := WriteFile(t.Context(), path, []byte("payload"))
	if err != nil {
		t.Fatalf("WriteFile(no WithMode, dir-fsync fail) = %v, want nil error", err)
	}
	if res.Durable {
		t.Error("Result.Durable = true after a dir-fsync failure, want false")
	}
	assertContent(t, path, "payload")
}

// holds reports whether path still holds exactly want.
func holds(path, want string) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(got) != want {
		return fmt.Errorf("%s holds %q, want %q", path, got, want)
	}
	return nil
}
