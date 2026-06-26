package atomicfile

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildCfg_Defaults pins every default in buildCfg, including that a nil
// option slice is accepted (the no-options path).
func TestBuildCfg_Defaults(t *testing.T) {
	t.Parallel()
	c := buildCfg(nil)
	if c.mode != 0o644 {
		t.Errorf("default mode = %o, want 0644", c.mode)
	}
	if c.logger == nil {
		t.Error("logger is nil, want slog.Default()")
	}
	if c.preserveMode {
		t.Error("preserveMode should be false by default")
	}
	if c.preserveOwnership {
		t.Error("preserveOwnership should be false by default")
	}
	if c.noSync {
		t.Error("noSync should be false by default")
	}
	if c.allowSymlinkTarget {
		t.Error("allowSymlinkTarget should be false by default")
	}
	if c.mkdirMode != 0 {
		t.Errorf("mkdirMode = %o, want 0", c.mkdirMode)
	}
}

// TestOptions_Threading pins that each option threads its value into the
// resolved cfg.
func TestOptions_Threading(t *testing.T) {
	t.Parallel()

	t.Run("WithMode", func(t *testing.T) {
		t.Parallel()
		if c := buildCfg([]Option{WithMode(0o755)}); c.mode != 0o755 {
			t.Errorf("mode = %o, want 0755", c.mode)
		}
	})
	t.Run("WithMkdirMode", func(t *testing.T) {
		t.Parallel()
		if c := buildCfg([]Option{WithMkdirMode(0o750)}); c.mkdirMode != 0o750 {
			t.Errorf("mkdirMode = %o, want 0750", c.mkdirMode)
		}
	})
	t.Run("WithLogger", func(t *testing.T) {
		t.Parallel()
		l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
		if c := buildCfg([]Option{WithLogger(l)}); c.logger != l {
			t.Error("WithLogger did not thread the logger")
		}
	})
	t.Run("WithPreserveMode", func(t *testing.T) {
		t.Parallel()
		if c := buildCfg([]Option{WithPreserveMode()}); !c.preserveMode {
			t.Error("WithPreserveMode() did not set preserveMode")
		}
	})
	t.Run("WithPreserveOwnership", func(t *testing.T) {
		t.Parallel()
		if c := buildCfg([]Option{WithPreserveOwnership()}); !c.preserveOwnership {
			t.Error("WithPreserveOwnership() did not set preserveOwnership")
		}
	})
	t.Run("WithNoSync", func(t *testing.T) {
		t.Parallel()
		if c := buildCfg([]Option{WithNoSync()}); !c.noSync {
			t.Error("WithNoSync() did not set noSync")
		}
	})
	t.Run("WithAllowSymlinkTarget", func(t *testing.T) {
		t.Parallel()
		if c := buildCfg([]Option{WithAllowSymlinkTarget()}); !c.allowSymlinkTarget {
			t.Error("WithAllowSymlinkTarget() did not set allowSymlinkTarget")
		}
	})
}

// TestOptions_OrderLastWins pins that options apply in slice order, so a later
// WithMode overrides an earlier one and unrelated options still take effect.
func TestOptions_OrderLastWins(t *testing.T) {
	t.Parallel()
	c := buildCfg([]Option{
		WithNoSync(),
		WithMode(0o600),
		WithAllowSymlinkTarget(),
		WithMode(0o755), // overrides the earlier mode
		WithMkdirMode(0o700),
		WithPreserveOwnership(),
	})
	if c.mode != 0o755 {
		t.Errorf("mode = %o, want 0755 (last wins)", c.mode)
	}
	if !c.noSync {
		t.Error("noSync not set")
	}
	if !c.allowSymlinkTarget {
		t.Error("allowSymlinkTarget not set")
	}
	if c.mkdirMode != 0o700 {
		t.Errorf("mkdirMode = %o, want 0700", c.mkdirMode)
	}
	if !c.preserveOwnership {
		t.Error("preserveOwnership not set")
	}
}

// TestOptions_NilElement ensures a nil Option in the variadic slice is skipped
// rather than dereferenced (which would panic), at every public entry point.
func TestOptions_NilElement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("WriteFile", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "f.txt")
		if _, err := WriteFile(ctx, p, []byte("x"), nil, WithNoSync(), nil); err != nil {
			t.Fatalf("WriteFile with nil option: %v", err)
		}
		assertContent(t, p, "x")
	})
	t.Run("WriteReader", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "r.txt")
		if _, err := WriteReader(ctx, p, strings.NewReader("hello"), nil, nil); err != nil {
			t.Fatalf("WriteReader with nil option: %v", err)
		}
		assertContent(t, p, "hello")
	})
	t.Run("NewPendingFile", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(t.TempDir(), "pf.txt")
		pf, err := NewPendingFile(ctx, p, nil, nil)
		if err != nil {
			t.Fatalf("NewPendingFile with nil option: %v", err)
		}
		defer func() { _ = pf.Cleanup() }()
		if _, err := pf.Write([]byte("pf")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := pf.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		assertContent(t, p, "pf")
	})
	t.Run("CleanupStaleTemps", func(t *testing.T) {
		t.Parallel()
		if _, err := CleanupStaleTemps(t.TempDir(), time.Hour, nil, nil); err != nil {
			t.Fatalf("CleanupStaleTemps with nil option: %v", err)
		}
	})
}

// TestOptions_AllNil pins that an all-nil option slice falls back to the
// default 0o644 mode rather than panicking.
func TestOptions_AllNil(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	p := filepath.Join(t.TempDir(), "allnil.txt")
	if _, err := WriteFile(context.Background(), p, []byte("x"), nil, nil, nil, nil); err != nil {
		t.Fatalf("WriteFile all nils: %v", err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("all-nil mode = %o, want 0644", fi.Mode().Perm())
	}
}

// TestOptions_DefaultMode_WriteFile pins the 0o644 default end-to-end (not just
// in buildCfg) for a write with no options.
func TestOptions_DefaultMode_WriteFile(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	p := filepath.Join(t.TempDir(), "dm.txt")
	if _, err := WriteFile(context.Background(), p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("WriteFile default mode = %o, want 0644", fi.Mode().Perm())
	}
}

// TestOptions_Logger pins that a custom logger is accepted and the write still
// lands.
func TestOptions_Logger(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "logged.txt")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := WriteFile(context.Background(), path, []byte("data"), WithLogger(logger)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	assertContent(t, path, "data")
}

// TestOptions_AllCombined_WriteFile threads logger + mode + mkdir + nosync
// through a single write and pins that each takes effect together.
func TestOptions_AllCombined_WriteFile(t *testing.T) {
	t.Parallel()
	if isWindows() {
		t.Skip("file mode not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "sub", "combined.txt")
	l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	res, err := WriteFile(context.Background(), path, []byte("combined"),
		WithLogger(l),
		WithMode(0o600),
		WithMkdirMode(0o755),
		WithNoSync(),
	)
	if err != nil {
		t.Fatalf("WriteFile combined opts: %v", err)
	}
	if res.Durable {
		t.Errorf("Result.Durable = true, want false under WithNoSync")
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	assertContent(t, path, "combined")
}
