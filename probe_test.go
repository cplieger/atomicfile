package atomicfile

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"
)

// msgProbeTeardown is the Debug line tearDown emits for a teardown failure it
// is not reporting; sharing the literal keeps the test in lockstep with it.
const msgProbeTeardown = "atomicfile: writability probe teardown failed"

// skipIfRootCannotBeDenied skips a test whose outcome depends on POSIX
// permission bits being enforced against the running user.
func skipIfRootCannotBeDenied(t *testing.T) {
	t.Helper()
	if isWindows() {
		t.Skip("POSIX mode bits drive directory-write permission")
	}
	if u, err := user.Current(); err == nil && u.Uid == "0" {
		t.Skip("root bypasses EACCES")
	}
}

// assertEmptyDir fails t unless dir has no entries at all, which is the
// stronger form of assertNoTempLeak for the probe's success path: the probe
// must leave the directory exactly as it found it.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q) = %v, want nil", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left %d entries in %q, want 0: %v", len(entries), dir, entries)
	}
}

func TestProbeWritable(t *testing.T) {
	t.Parallel()

	t.Run("success_reports_every_stage_passed_and_leaves_nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		res, err := ProbeWritable(t.Context(), dir)
		if err != nil {
			t.Fatalf("ProbeWritable(%q) = err %v, want nil (a stage failure is never an error)", dir, err)
		}
		if !res.OK() || !res.Writable() {
			t.Errorf("OK()=%v Writable()=%v, want true true (stage %v, err %v)",
				res.OK(), res.Writable(), res.Stage, res.Err)
		}
		if res.Stage != ProbeStageNone || res.Err != nil {
			t.Errorf("Stage=%v Err=%v, want ProbeStageNone and nil", res.Stage, res.Err)
		}
		if res.Leaked {
			t.Error("Leaked = true, want false; the probe removed its own file")
		}
		if res.Dir != dir {
			t.Errorf("Dir = %q, want the probed directory %q", res.Dir, dir)
		}
		if !IsPackageTemp(res.Name) {
			t.Errorf("Name = %q, which IsPackageTemp rejects; a leaked probe would be unreclaimable", res.Name)
		}
		assertEmptyDir(t, dir)
	})

	t.Run("missing_dir_fails_at_create", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		missing := filepath.Join(parent, "nope")

		res, err := ProbeWritable(t.Context(), missing)
		if err != nil {
			t.Fatalf("ProbeWritable(missing) = err %v, want nil", err)
		}
		if res.Stage != ProbeStageCreate {
			t.Errorf("Stage = %v, want ProbeStageCreate (a missing dir without WithMkdirMode)", res.Stage)
		}
		if !errors.Is(res.Err, fs.ErrNotExist) {
			t.Errorf("Err = %v, want an fs.ErrNotExist match", res.Err)
		}
		if res.Writable() || res.Name != "" || res.Leaked {
			t.Errorf("Writable()=%v Name=%q Leaked=%v, want false, \"\", false",
				res.Writable(), res.Name, res.Leaked)
		}
		if _, statErr := os.Stat(missing); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("probe created %q without WithMkdirMode", missing)
		}
	})

	t.Run("missing_dir_with_mkdir_mode_succeeds", func(t *testing.T) {
		t.Parallel()
		created := filepath.Join(t.TempDir(), "sub", "deeper")

		res, err := ProbeWritable(t.Context(), created, WithMkdirMode(0o750))
		if err != nil {
			t.Fatalf("ProbeWritable(missing, WithMkdirMode) = err %v, want nil", err)
		}
		if !res.OK() {
			t.Fatalf("Stage = %v (err %v), want ProbeStageNone", res.Stage, res.Err)
		}
		fi, statErr := os.Stat(created)
		if statErr != nil {
			t.Fatalf("Stat(%q) = %v, want the directory chain created", created, statErr)
		}
		if !fi.IsDir() {
			t.Errorf("%q is not a directory", created)
		}
		assertEmptyDir(t, created)
	})

	t.Run("mkdir_failure_reports_the_mkdir_stage", func(t *testing.T) {
		t.Parallel()
		// A regular file in the parent chain makes MkdirAll fail without any
		// permission trickery, so the stage is reachable as root too.
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed blocker: %v", err)
		}

		res, err := ProbeWritable(t.Context(), filepath.Join(blocker, "under"), WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("ProbeWritable = err %v, want nil", err)
		}
		if res.Stage != ProbeStageMkdir || res.Err == nil {
			t.Errorf("Stage=%v Err=%v, want ProbeStageMkdir and a non-nil error", res.Stage, res.Err)
		}
		if res.Writable() {
			t.Error("Writable() = true, want false; the directory does not exist")
		}
	})

	t.Run("non_directory_fails_at_create", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "bind-mounted-file")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}

		res, err := ProbeWritable(t.Context(), file)
		if err != nil {
			t.Fatalf("ProbeWritable(file) = err %v, want nil", err)
		}
		if res.Stage != ProbeStageCreate || res.Err == nil {
			t.Errorf("Stage=%v Err=%v, want ProbeStageCreate and a non-nil error", res.Stage, res.Err)
		}
	})

	t.Run("unwritable_dir_fails_at_create", func(t *testing.T) {
		skipIfRootCannotBeDenied(t)
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		res, err := ProbeWritable(t.Context(), dir)
		if err != nil {
			t.Fatalf("ProbeWritable(unwritable) = err %v, want nil", err)
		}
		if res.Stage != ProbeStageCreate {
			t.Errorf("Stage = %v, want ProbeStageCreate", res.Stage)
		}
		if !errors.Is(res.Err, fs.ErrPermission) {
			t.Errorf("Err = %v, want an fs.ErrPermission match", res.Err)
		}
		if res.Writable() {
			t.Error("Writable() = true on a directory that refused the create")
		}
	})

	t.Run("empty_dir_is_an_argument_error", func(t *testing.T) {
		t.Parallel()
		res, err := ProbeWritable(t.Context(), "")
		if !errors.Is(err, ErrEmptyPath) {
			t.Errorf("err = %v, want ErrEmptyPath", err)
		}
		if res.Stage != ProbeStageNone {
			t.Errorf("Stage = %v, want the zero value when the probe never ran", res.Stage)
		}
	})

	t.Run("cancelled_context_is_an_argument_error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := ProbeWritable(ctx, dir); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want a context.Canceled match", err)
		}
		assertEmptyDir(t, dir)
	})
}

// TestProbeWritableRelativeDir pins the dir argument's contract: it may be
// relative, matching CleanupStaleTemps' dir rather than the write functions'
// absolute-path requirement, because a preflight probes whatever path its
// config handed it. It cannot be a parallel test — t.Chdir forbids that — so it
// stands alone rather than inside TestProbeWritable.
func TestProbeWritableRelativeDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	res, err := ProbeWritable(t.Context(), "./.")
	if err != nil {
		t.Fatalf("ProbeWritable(\"./.\") = err %v, want nil", err)
	}
	if !res.OK() {
		t.Errorf("Stage = %v (err %v), want ProbeStageNone", res.Stage, res.Err)
	}
	if res.Dir != "." {
		t.Errorf("Dir = %q, want the cleaned argument %q", res.Dir, ".")
	}
	assertEmptyDir(t, dir)
}

func TestProbeWritableInRoot(t *testing.T) {
	t.Parallel()

	t.Run("probes_the_root_itself", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)

		res, err := ProbeWritableInRoot(t.Context(), root, ".")
		if err != nil {
			t.Fatalf("ProbeWritableInRoot(root, \".\") = err %v, want nil", err)
		}
		if !res.OK() {
			t.Fatalf("Stage = %v (err %v), want ProbeStageNone", res.Stage, res.Err)
		}
		if !IsPackageTemp(res.Name) {
			t.Errorf("Name = %q, which IsPackageTemp rejects", res.Name)
		}
		assertEmptyDir(t, dir)
	})

	t.Run("probes_a_subdirectory", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		sub := filepath.Join(dir, "out")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		res, err := ProbeWritableInRoot(t.Context(), root, "out")
		if err != nil {
			t.Fatalf("ProbeWritableInRoot(root, \"out\") = err %v, want nil", err)
		}
		if !res.OK() {
			t.Fatalf("Stage = %v (err %v), want ProbeStageNone", res.Stage, res.Err)
		}
		if res.Dir != sub {
			t.Errorf("Dir = %q, want root.Name() joined with the name (%q)", res.Dir, sub)
		}
		assertEmptyDir(t, sub)
	})

	t.Run("missing_subdirectory_with_mkdir_mode_succeeds", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)

		res, err := ProbeWritableInRoot(t.Context(), root, "made/here", WithMkdirMode(0o755))
		if err != nil {
			t.Fatalf("ProbeWritableInRoot(WithMkdirMode) = err %v, want nil", err)
		}
		if !res.OK() {
			t.Fatalf("Stage = %v (err %v), want ProbeStageNone", res.Stage, res.Err)
		}
		if fi, statErr := os.Stat(filepath.Join(dir, "made", "here")); statErr != nil || !fi.IsDir() {
			t.Errorf("Stat(made/here) = %v, %v; want the chain created inside the root", fi, statErr)
		}
	})

	t.Run("escaping_name_is_refused", func(t *testing.T) {
		t.Parallel()
		outer := t.TempDir()
		inner := filepath.Join(outer, "inner")
		if err := os.Mkdir(inner, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		root, err := os.OpenRoot(inner)
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		t.Cleanup(func() { _ = root.Close() })

		cases := map[string]struct {
			opts      []Option
			wantStage ProbeStage
		}{
			"create_stage_without_mkdir": {wantStage: ProbeStageCreate},
			"mkdir_stage_with_mkdir":     {opts: []Option{WithMkdirMode(0o755)}, wantStage: ProbeStageMkdir},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				res, probeErr := ProbeWritableInRoot(t.Context(), root, "../escaped", tc.opts...)
				if probeErr != nil {
					t.Fatalf("ProbeWritableInRoot(escaping) = err %v, want nil (the root refuses it as a stage failure)", probeErr)
				}
				if res.Stage != tc.wantStage || res.Err == nil {
					t.Errorf("Stage=%v Err=%v, want %v and a non-nil error", res.Stage, res.Err, tc.wantStage)
				}
				if res.Writable() || res.Name != "" {
					t.Errorf("Writable()=%v Name=%q, want false and \"\"", res.Writable(), res.Name)
				}
			})
		}

		// Nothing may exist outside the root, at any name.
		entries, readErr := os.ReadDir(outer)
		if readErr != nil {
			t.Fatalf("ReadDir(%q) = %v", outer, readErr)
		}
		if len(entries) != 1 || entries[0].Name() != "inner" {
			t.Errorf("escaping probe touched the parent: %v", entries)
		}
		assertEmptyDir(t, inner)
	})

	t.Run("argument_errors", func(t *testing.T) {
		t.Parallel()
		root, _ := openTestRoot(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := ProbeWritableInRoot(t.Context(), nil, "."); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("nil root: err = %v, want ErrUnsafePath", err)
		}
		if _, err := ProbeWritableInRoot(t.Context(), root, ""); !errors.Is(err, ErrEmptyPath) {
			t.Errorf("empty name: err = %v, want ErrEmptyPath", err)
		}
		if _, err := ProbeWritableInRoot(t.Context(), root, "/abs"); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("absolute name: err = %v, want ErrUnsafePath", err)
		}
		if _, err := ProbeWritableInRoot(ctx, root, "."); !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled ctx: err = %v, want a context.Canceled match", err)
		}
	})
}

// TestProbeData covers the two stages that run while the probe file is open.
// They are exercised at probeData rather than through ProbeWritable because
// neither is constructible on a healthy directory the test owns: a write
// failure needs a device that refuses data (/dev/full), and a sync failure
// needs a handle that cannot be flushed (a pipe). Both are real filesystem
// refusals, not injected ones.
func TestProbeData(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		open      func(t *testing.T) *os.File
		wantStage ProbeStage
	}{
		"regular_file_passes": {
			open:      func(t *testing.T) *os.File { return openProbeTemp(t) },
			wantStage: ProbeStageNone,
		},
		"closed_handle_fails_at_write": {
			open: func(t *testing.T) *os.File {
				f := openProbeTemp(t)
				if err := f.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				return f
			},
			wantStage: ProbeStageWrite,
		},
		"unflushable_handle_fails_at_sync": {
			open:      openUnflushable,
			wantStage: ProbeStageSync,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stage, err := probeData(tc.open(t))
			if stage != tc.wantStage {
				t.Errorf("stage = %v, want %v (err %v)", stage, tc.wantStage, err)
			}
			if (err != nil) != (tc.wantStage != ProbeStageNone) {
				t.Errorf("err = %v, want non-nil only for a failing stage", err)
			}
		})
	}
}

// openProbeTemp creates a package-shaped probe file in a fresh temp directory
// and returns the open handle.
func openProbeTemp(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), TempName()))
	if err != nil {
		t.Fatalf("create probe file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// openUnflushable returns a handle that accepts a write and refuses a flush:
// the write end of a pipe, whose Sync fails with EINVAL. It stands in for the
// network filesystem that reports a deferred error only at fsync.
func openUnflushable(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return w
}

// TestProbeTearDown covers the two teardown stages and the leak accounting. A
// same-owner test cannot make a real directory accept a create and refuse the
// unlink (both need the same directory-write bit), so the refusal is
// constructed where it is constructible: an entry replaced by a NON-EMPTY
// directory makes Remove fail with ENOTEMPTY on every platform, and an
// already-closed handle makes Close fail. No production seam is added for it.
func TestProbeTearDown(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		// prepare mutates the created probe file (and may pre-close the
		// handle) to force a teardown failure.
		prepare    func(t *testing.T, path string, f *os.File)
		firstStage ProbeStage
		wantStage  ProbeStage
		wantLeaked bool
		wantDebug  int
	}{
		"clean_teardown": {
			prepare:   func(*testing.T, string, *os.File) {},
			wantStage: ProbeStageNone,
		},
		"already_gone_is_success": {
			prepare: func(t *testing.T, path string, _ *os.File) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("pre-remove: %v", err)
				}
			},
			wantStage: ProbeStageNone,
		},
		"close_failure_is_reported": {
			prepare: func(t *testing.T, _ string, f *os.File) {
				if err := f.Close(); err != nil {
					t.Fatalf("pre-close: %v", err)
				}
			},
			wantStage: ProbeStageClose,
		},
		"remove_failure_is_reported_and_leaks": {
			prepare: func(t *testing.T, path string, _ *os.File) {
				replaceWithNonEmptyDir(t, path)
			},
			wantStage:  ProbeStageRemove,
			wantLeaked: true,
		},
		"first_failure_wins_and_the_second_is_logged": {
			prepare: func(t *testing.T, path string, f *os.File) {
				if err := f.Close(); err != nil {
					t.Fatalf("pre-close: %v", err)
				}
				replaceWithNonEmptyDir(t, path)
			},
			wantStage:  ProbeStageClose,
			wantLeaked: true,
			wantDebug:  1,
		},
		"an_earlier_stage_failure_is_never_overwritten": {
			prepare: func(t *testing.T, path string, _ *os.File) {
				replaceWithNonEmptyDir(t, path)
			},
			firstStage: ProbeStageWrite,
			wantStage:  ProbeStageWrite,
			wantLeaked: true,
			wantDebug:  1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, dir := openTestRoot(t)
			tmpName := TempName()
			f, err := root.Create(tmpName)
			if err != nil {
				t.Fatalf("create probe file: %v", err)
			}
			t.Cleanup(func() { _ = f.Close() })
			tc.prepare(t, filepath.Join(dir, tmpName), f)

			h := &captureHandler{}
			res := ProbeResult{Dir: dir, Name: tmpName, Leaked: true, Stage: tc.firstStage}
			if tc.firstStage != ProbeStageNone {
				res.Err = errors.New("earlier stage")
			}
			res.tearDown(root, f, tmpName, buildCfg([]Option{WithLogger(slog.New(h))}))

			if res.Stage != tc.wantStage {
				t.Errorf("Stage = %v, want %v (err %v)", res.Stage, tc.wantStage, res.Err)
			}
			if (res.Err != nil) != (tc.wantStage != ProbeStageNone) {
				t.Errorf("Err = %v, want non-nil only for a failing stage", res.Err)
			}
			if res.Leaked != tc.wantLeaked {
				t.Errorf("Leaked = %v, want %v", res.Leaked, tc.wantLeaked)
			}
			if got := h.CountLevelExact(slog.LevelDebug, msgProbeTeardown); got != tc.wantDebug {
				t.Errorf("Debug %q count = %d, want %d", msgProbeTeardown, got, tc.wantDebug)
			}
		})
	}
}

func TestProbeStageString(t *testing.T) {
	t.Parallel()
	cases := map[ProbeStage]string{
		ProbeStageNone:   "no failure",
		ProbeStageMkdir:  "create directory",
		ProbeStageCreate: "create probe file",
		ProbeStageWrite:  "write probe file",
		ProbeStageSync:   "sync probe file",
		ProbeStageClose:  "close probe file",
		ProbeStageRemove: "remove probe file",
		ProbeStage(99):   "unknown stage",
	}
	for stage, want := range cases {
		if got := stage.String(); got != want {
			t.Errorf("ProbeStage(%d).String() = %q, want %q", int(stage), got, want)
		}
	}
}

func TestProbeResultWritable(t *testing.T) {
	t.Parallel()
	// The split every consumer's policy is built on: a teardown failure still
	// proves the directory took the bytes; anything earlier does not.
	cases := map[ProbeStage]bool{
		ProbeStageNone:   true,
		ProbeStageMkdir:  false,
		ProbeStageCreate: false,
		ProbeStageWrite:  false,
		ProbeStageSync:   false,
		ProbeStageClose:  true,
		ProbeStageRemove: true,
	}
	for stage, want := range cases {
		res := ProbeResult{Stage: stage}
		if got := res.Writable(); got != want {
			t.Errorf("ProbeResult{Stage: %v}.Writable() = %v, want %v", stage, got, want)
		}
		if got := res.OK(); got != (stage == ProbeStageNone) {
			t.Errorf("ProbeResult{Stage: %v}.OK() = %v, want %v", stage, got, stage == ProbeStageNone)
		}
	}
}

func TestProbeCause(t *testing.T) {
	t.Parallel()
	// A probe reports the filesystem error, not a *WriteError: that type means
	// "the data did not reach its final path", a claim about a write the probe
	// never performed.
	cause := errors.New("EACCES")
	if got := probeCause(&WriteError{Phase: PhaseTempCreate, Err: cause}); !errors.Is(got, cause) {
		t.Errorf("probeCause(*WriteError) = %v, want the unwrapped %v", got, cause)
	}
	if got := probeCause(cause); !errors.Is(got, cause) {
		t.Errorf("probeCause(plain) = %v, want it unchanged", got)
	}
	if got := probeCause(nil); got != nil {
		t.Errorf("probeCause(nil) = %v, want nil", got)
	}
}

// TestProbeLeakIsReclaimable ties the probe's own file name to the sweeps: a
// probe left behind by a crash — or by a directory that denies the unlink — is
// reclaimed by the library's normal stale-temp sweep, so no consumer has to
// reconstruct the name shape to make its probe reclaimable.
func TestProbeLeakIsReclaimable(t *testing.T) {
	t.Parallel()

	t.Run("ambient_sweep", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		res, err := ProbeWritable(t.Context(), dir)
		if err != nil || !res.OK() {
			t.Fatalf("ProbeWritable = %+v, err %v; want a clean probe", res, err)
		}
		leaked := seedLeakedProbe(t, dir, res.Name)

		if !IsPackageTemp(res.Name) {
			t.Fatalf("IsPackageTemp(%q) = false; the probe named a file no sweep reclaims", res.Name)
		}
		removed, sweepErr := CleanupStaleTemps(t.Context(), dir, time.Hour)
		if sweepErr != nil {
			t.Fatalf("CleanupStaleTemps: %v", sweepErr)
		}
		if removed.Removed != 1 {
			t.Errorf("removed = %d, want 1", removed)
		}
		if _, statErr := os.Stat(leaked); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("leaked probe %q survived the sweep: %v", leaked, statErr)
		}
	})

	t.Run("root_confined_sweep", func(t *testing.T) {
		t.Parallel()
		root, dir := openTestRoot(t)
		res, err := ProbeWritableInRoot(t.Context(), root, ".")
		if err != nil || !res.OK() {
			t.Fatalf("ProbeWritableInRoot = %+v, err %v; want a clean probe", res, err)
		}
		leaked := seedLeakedProbe(t, dir, res.Name)

		sweep, sweepErr := CleanupStaleTempsInRoot(t.Context(), root, time.Hour)
		if sweepErr != nil {
			t.Fatalf("CleanupStaleTempsInRoot: %v", sweepErr)
		}
		if sweep.Removed != 1 || sweep.Failed != 0 {
			t.Errorf("sweep = %+v, want Removed 1 and Failed 0", sweep)
		}
		if _, statErr := os.Stat(leaked); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("leaked probe %q survived the sweep: %v", leaked, statErr)
		}
	})
}

// seedLeakedProbe recreates a probe file under the exact name a probe chose and
// backdates it past any sweep cutoff, standing in for the crash between create
// and unlink that the test cannot schedule.
func seedLeakedProbe(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte{0}, 0o600); err != nil {
		t.Fatalf("seed leaked probe %q: %v", path, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes %q: %v", path, err)
	}
	return path
}
