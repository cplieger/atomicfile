// Package atomicfile provides crash-safe atomic file writes via
// temp→fsync→rename→dir-fsync, path-traversal validation, bounded
// reads, and JSON save helpers. Standard-library only.
package atomicfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultTempPrefix is the default pattern used for temp files.
const DefaultTempPrefix = ".atomicfile-*.tmp"

// ErrEmptyPath is returned when a path argument is empty.
var ErrEmptyPath = errors.New("atomic write: empty path")

// ErrUnsafePath is returned when a path fails the local safety check.
var ErrUnsafePath = errors.New("atomic write: unsafe path")

// ErrFileTooLarge is returned when a file exceeds the size limit.
var ErrFileTooLarge = errors.New("file too large")

// Options configures atomic write behavior.
type Options struct {
	// TempPattern is the os.CreateTemp pattern for temp files.
	// Defaults to DefaultTempPrefix if empty.
	TempPattern string
}

func (o *Options) pattern() string {
	if o != nil && o.TempPattern != "" {
		return o.TempPattern
	}
	return DefaultTempPrefix
}

// validateAbsClean checks that path is absolute, free of ".." traversal,
// and returns the cleaned form.
func validateAbsClean(path string) (string, error) {
	if path == "" {
		return "", ErrEmptyPath
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: not absolute: %q", ErrUnsafePath, path)
	}
	if strings.Contains(clean, ".."+string(filepath.Separator)) ||
		strings.HasSuffix(clean, string(filepath.Separator)+"..") ||
		clean == ".." {
		return "", fmt.Errorf("%w: contains traversal: %q", ErrUnsafePath, path)
	}
	return clean, nil
}

// WritePhase identifies which step of an atomic write failed.
type WritePhase int

const (
	PhaseTempCreate WritePhase = iota + 1
	PhaseTempWrite
	PhaseTempChmod
	PhaseTempSync
	PhaseTempClose
	PhaseRename
)

func (p WritePhase) String() string {
	switch p {
	case PhaseTempCreate:
		return "create temp file"
	case PhaseTempWrite:
		return "write temp file"
	case PhaseTempChmod:
		return "chmod temp file"
	case PhaseTempSync:
		return "sync temp file"
	case PhaseTempClose:
		return "close temp file"
	case PhaseRename:
		return "rename to final path"
	default:
		return "unknown phase"
	}
}

// WriteError wraps an atomic-write failure with the phase that failed.
type WriteError struct {
	Err   error
	Phase WritePhase
}

func (e *WriteError) Error() string { return e.Phase.String() + ": " + e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }

// WriteFile writes data to path atomically with mode 0644.
func WriteFile(ctx context.Context, path string, data []byte) error {
	return WriteFileMode(ctx, path, data, 0o644, nil)
}

// WriteFileMode writes data to path atomically with the specified permissions.
func WriteFileMode(ctx context.Context, path string, data []byte, mode os.FileMode, opts *Options) error {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write: %w", err)
	}
	dir := filepath.Dir(cleanPath)
	tmp, err := os.CreateTemp(dir, opts.pattern())
	if err != nil {
		return &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			slog.Debug("atomic write: temp file cleanup failed", "path", tmpName, "error", rmErr)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return &WriteError{Phase: PhaseTempWrite, Err: err}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("atomic write: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return &WriteError{Phase: PhaseTempSync, Err: err}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("atomic write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return &WriteError{Phase: PhaseTempClose, Err: err}
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		cleanup()
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	fsyncDir(dir)
	return nil
}

// Prepare creates a temp file with data written, synced, and closed —
// ready for a final rename via Commit.
func Prepare(ctx context.Context, path string, data []byte, opts *Options) (tmpPath string, cleanup func(), err error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return "", nil, err
	}
	if err := ctx.Err(); err != nil {
		return "", nil, fmt.Errorf("atomic write: %w", err)
	}
	dir := filepath.Dir(cleanPath)
	tmp, err := os.CreateTemp(dir, opts.pattern())
	if err != nil {
		return "", nil, &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	tmpName := tmp.Name()
	doCleanup := func() {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			slog.Debug("atomic write: temp file cleanup failed", "path", tmpName, "error", rmErr)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		doCleanup()
		return "", nil, &WriteError{Phase: PhaseTempWrite, Err: err}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		doCleanup()
		return "", nil, fmt.Errorf("atomic write: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		doCleanup()
		return "", nil, &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		doCleanup()
		return "", nil, &WriteError{Phase: PhaseTempSync, Err: err}
	}
	if err := tmp.Close(); err != nil {
		doCleanup()
		return "", nil, &WriteError{Phase: PhaseTempClose, Err: err}
	}
	return tmpName, doCleanup, nil
}

// Commit renames the prepared temp file to the final path and fsyncs
// the parent directory.
func Commit(tmpPath, finalPath string) error {
	cleanFinal, err := validateAbsClean(finalPath)
	if err != nil {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			slog.Debug("atomic write: temp cleanup after path validation error", "path", tmpPath, "error", rmErr)
		}
		return err
	}
	if err := os.Rename(tmpPath, cleanFinal); err != nil {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			slog.Debug("atomic write: temp cleanup after rename error", "path", tmpPath, "error", rmErr)
		}
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	fsyncDir(filepath.Dir(cleanFinal))
	return nil
}

// SaveBytes writes raw bytes to path atomically, creating parent
// directories as needed. Directory permissions are 0755 for
// world-readable files, 0700 for private-only files.
func SaveBytes(path string, data []byte, perm os.FileMode, opts *Options) error {
	dir := filepath.Dir(path)
	dirPerm := os.FileMode(0o755)
	if perm&0o077 == 0 {
		dirPerm = 0o700
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return err
	}
	tmpName, err := writeTempFile(dir, filepath.Base(path), data, perm, opts)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	fsyncDir(dir)
	return nil
}

// SaveJSON marshals v to indented JSON and writes it atomically.
// mu must not be nil; it serializes concurrent writes to the same path.
func SaveJSON(path string, mu *sync.Mutex, v any, label string, perm os.FileMode, opts *Options) error {
	if mu == nil {
		return errors.New("atomicfile.SaveJSON: nil mutex")
	}
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		slog.Error("atomicfile.SaveJSON: marshal failed", "label", label, "error", err)
		return err
	}
	if err := SaveBytes(path, data, perm, opts); err != nil {
		slog.Error("atomicfile.SaveJSON: write failed", "label", label, "path", path, "error", err)
		return err
	}
	return nil
}

// ReadBounded opens a file, validates its size against maxBytes, and
// reads it with a LimitReader. Returns ErrFileTooLarge if the file
// exceeds the limit. Also detects if the file grew during the read.
func ReadBounded(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(cleanPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, fi.Size(), maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: file grew past %d byte limit during read", ErrFileTooLarge, maxBytes)
	}
	return data, nil
}

// fsyncDir best-effort fsyncs a directory for rename durability.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("atomicfile: parent dir open for fsync failed", "dir", dir, "error", err)
		return
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		slog.Debug("atomicfile: parent dir fsync failed", "dir", dir, "error", err)
	}
}

// writeTempFile creates a temp file in dir, writes data, fsyncs, closes,
// and chmods. Returns the temp file name on success.
func writeTempFile(dir, baseName string, data []byte, perm os.FileMode, opts *Options) (string, error) {
	pattern := baseName + ".tmp-*"
	if opts != nil && opts.TempPattern != "" {
		pattern = opts.TempPattern
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	if err = os.Chmod(name, perm); err != nil {
		return "", err
	}
	return name, nil
}

// isStaleTempName reports whether name matches temp file patterns.
func isStaleTempName(name string) bool {
	for _, tag := range [...]string{".tmp-", ".upload-", ".copy-"} {
		i := strings.LastIndex(name, tag)
		if i < 0 || i+len(tag) >= len(name) {
			continue
		}
		tail := name[i+len(tag):]
		if !strings.ContainsAny(tail, "./\\") {
			return true
		}
	}
	return false
}

// CleanupStaleTemps removes stale temp files in dir older than maxAge.
// Best-effort; errors are logged but not returned.
func CleanupStaleTemps(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("atomicfile.CleanupStaleTemps: readdir failed", "dir", dir, "error", err)
		}
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		name := e.Name()
		if !isStaleTempName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Debug("atomicfile.CleanupStaleTemps: stat failed", "name", name, "error", err)
			}
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(dir, name)
		if err := os.Remove(full); err != nil {
			slog.Warn("atomicfile.CleanupStaleTemps: remove failed", "path", full, "error", err)
			continue
		}
		slog.Info("atomicfile.CleanupStaleTemps: removed stale temp", "path", full, "age", time.Since(info.ModTime()))
	}
}
