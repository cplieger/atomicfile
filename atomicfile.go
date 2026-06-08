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
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultTempPrefix is the default pattern used for temp files.
const DefaultTempPrefix = ".atomicfile-*.tmp"

// largeSaveJSONThreshold is the marshalled-payload size above which
// SaveJSON emits a Warn log (but still proceeds).
const largeSaveJSONThreshold = 16 << 20 // 16 MiB

// ErrEmptyPath is returned when a path argument is empty.
var ErrEmptyPath = errors.New("atomic write: empty path")

// ErrUnsafePath is returned when a path fails the local safety check.
var ErrUnsafePath = errors.New("atomic write: unsafe path")

// ErrFileTooLarge is returned when a file exceeds the size limit.
var ErrFileTooLarge = errors.New("file too large")

// ErrSymlinkTarget is returned when the target path is a symlink and
// AllowSymlinkTarget is not set. Atomic rename replaces the symlink
// itself, not the file it points to — which is rarely the caller's intent.
var ErrSymlinkTarget = errors.New("atomic write: target is a symlink")

// cfg holds resolved configuration for atomic write operations.
type cfg struct {
	logger             *slog.Logger
	tempPattern        string
	mode               os.FileMode
	mkdirMode          os.FileMode
	preserveMode       bool
	preserveOwnership  bool
	noSync             bool
	allowSymlinkTarget bool
}

// Option configures an atomic write operation.
type Option func(*cfg)

// WithLogger sets a custom logger. If not provided, slog.Default() is used.
func WithLogger(l *slog.Logger) Option {
	return func(c *cfg) { c.logger = l }
}

// WithTempPattern sets the os.CreateTemp pattern for temp files.
// Defaults to DefaultTempPrefix if not set.
func WithTempPattern(pattern string) Option {
	return func(c *cfg) { c.tempPattern = pattern }
}

// WithMode sets the file permission for the written file.
// Defaults to 0o644 if not set.
func WithMode(mode os.FileMode) Option {
	return func(c *cfg) { c.mode = mode }
}

// WithMkdirMode causes write functions to create parent directories
// with the specified permission before writing.
func WithMkdirMode(mode os.FileMode) Option {
	return func(c *cfg) { c.mkdirMode = mode }
}

// WithPreserveMode stats the target file before writing and applies
// its existing mode to the new file. Falls back to the explicit mode
// if the target does not exist.
func WithPreserveMode() Option {
	return func(c *cfg) { c.preserveMode = true }
}

// WithPreserveOwnership stats the target file before writing and
// chowns the temp file to match the target's uid/gid.
// Requires CAP_CHOWN or root. No-op if target doesn't exist.
// Best-effort: the chown is applied after the temp-file fsync, so unlike
// file content and mode it is not covered by the durability barrier; a
// crash immediately after a successful write may leave the file with the
// writing process's ownership.
func WithPreserveOwnership() Option {
	return func(c *cfg) { c.preserveOwnership = true }
}

// WithNoSync skips fsync on the temp file and parent directory.
// This provides atomicity without durability.
func WithNoSync() Option {
	return func(c *cfg) { c.noSync = true }
}

// WithAllowSymlinkTarget permits writing to a path that is currently
// a symlink. By default, symlink targets are refused with ErrSymlinkTarget.
//
// Note: the atomic rename REPLACES the symlink with a regular file; it
// does not follow the link to write through to its target. If you intend
// to update the file the symlink points to, resolve it yourself with
// filepath.EvalSymlinks before calling.
func WithAllowSymlinkTarget() Option {
	return func(c *cfg) { c.allowSymlinkTarget = true }
}

func buildCfg(opts []Option) *cfg {
	c := &cfg{mode: 0o644, tempPattern: DefaultTempPrefix}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	if c.tempPattern == "" {
		c.tempPattern = DefaultTempPrefix
	}
	return c
}

// checkSymlink returns ErrSymlinkTarget if target is a symlink and
// AllowSymlinkTarget is not set.
//
// checkSymlink is best-effort: the Lstat precedes the eventual
// os.Rename, so an attacker who can write the parent directory may
// create a symlink after the check. Because os.Rename does not follow
// the final component, the worst case is replacing the attacker's
// symlink rather than writing through it.
func checkSymlink(target string, c *cfg) error {
	if c.allowSymlinkTarget {
		return nil
	}
	fi, err := os.Lstat(target)
	if err != nil {
		return nil
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrSymlinkTarget, target)
	}
	return nil
}

// validateAbsClean checks that path is absolute, free of ".." traversal
// and null bytes, and returns the cleaned form.
func validateAbsClean(path string) (string, error) {
	if path == "" {
		return "", ErrEmptyPath
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: contains null byte", ErrUnsafePath)
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
	// PhaseTempCreate indicates failure creating the temp file.
	PhaseTempCreate WritePhase = iota + 1
	// PhaseTempWrite indicates failure writing to the temp file.
	PhaseTempWrite
	// PhaseTempChmod indicates failure setting permissions on the temp file.
	PhaseTempChmod
	// PhaseTempSync indicates failure syncing the temp file.
	PhaseTempSync
	// PhaseTempClose indicates failure closing the temp file.
	PhaseTempClose
	// PhaseRename indicates failure renaming temp to final path.
	PhaseRename
	// PhaseChown indicates failure changing ownership of the temp file.
	PhaseChown
	// PhaseDirSync indicates failure fsyncing the parent directory after a
	// successful rename. The new content is already at the final path, but
	// durability across a crash is not guaranteed until the directory entry
	// reaches stable storage.
	PhaseDirSync
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
	case PhaseChown:
		return "chown temp file"
	case PhaseDirSync:
		return "fsync parent directory"
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

// resolveMode determines the file mode to use, considering PreserveMode.
func resolveMode(target string, explicit os.FileMode, c *cfg) os.FileMode {
	if c.preserveMode {
		if fi, err := os.Stat(target); err == nil {
			return fi.Mode().Perm()
		}
	}
	return explicit
}

// applyOwnership chowns tmpName to match target's uid/gid if PreserveOwnership is set.
func applyOwnership(tmpName, target string, c *cfg) error {
	if !c.preserveOwnership {
		return nil
	}
	fi, err := os.Stat(target)
	if err != nil {
		return nil
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(tmpName, int(stat.Uid), int(stat.Gid)); err != nil {
		return &WriteError{Phase: PhaseChown, Err: err}
	}
	return nil
}

// ensureDir creates parent directories if MkdirMode is set.
func ensureDir(dir string, c *cfg) error {
	if c.mkdirMode != 0 {
		return os.MkdirAll(dir, c.mkdirMode)
	}
	return nil
}

// removeTemp deletes a temp file best-effort, logging at Debug when
// removal fails for a reason other than the file already being gone.
func removeTemp(tmpName string, logger *slog.Logger) {
	if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		logger.Debug("atomic write: temp file cleanup failed", "path", tmpName, "error", rmErr)
	}
}

// WriteFile writes data to path atomically. Mode defaults to 0o644;
// override with WithMode. Additional behavior configured via opts.
func WriteFile(ctx context.Context, path string, data []byte, opts ...Option) error {
	c := buildCfg(opts)
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("atomic write: %w", ctxErr)
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return symErr
	}
	dir := filepath.Dir(cleanPath)
	if mkErr := ensureDir(dir, c); mkErr != nil {
		return mkErr
	}
	mode := resolveMode(cleanPath, c.mode, c)
	tmp, err := os.CreateTemp(dir, c.tempPattern)
	if err != nil {
		return &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	tmpName := tmp.Name()
	cleanup := func() { removeTemp(tmpName, c.logger) }
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
	if !c.noSync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			cleanup()
			return &WriteError{Phase: PhaseTempSync, Err: err}
		}
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
	if err := applyOwnership(tmpName, cleanPath, c); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		cleanup()
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	if err := commitDirSync(dir, c.noSync); err != nil {
		return err
	}
	return nil
}

// WriteReader atomically writes the contents of r to path with the given mode.
// If r implements io.WriterTo, it is used for efficient copying.
func WriteReader(ctx context.Context, path string, r io.Reader, mode os.FileMode, opts ...Option) error {
	c := buildCfg(opts)
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("atomic write: %w", ctxErr)
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return symErr
	}
	dir := filepath.Dir(cleanPath)
	if mkErr := ensureDir(dir, c); mkErr != nil {
		return mkErr
	}
	mode = resolveMode(cleanPath, mode, c)
	tmp, err := os.CreateTemp(dir, c.tempPattern)
	if err != nil {
		return &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			removeTemp(tmpName, c.logger)
		}
	}()
	var writeErr error
	if wt, ok := r.(io.WriterTo); ok {
		_, writeErr = wt.WriteTo(tmp)
	} else {
		_, writeErr = io.Copy(tmp, r)
	}
	if writeErr != nil {
		tmp.Close()
		return &WriteError{Phase: PhaseTempWrite, Err: writeErr}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomic write: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if !c.noSync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return &WriteError{Phase: PhaseTempSync, Err: err}
		}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomic write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return &WriteError{Phase: PhaseTempClose, Err: err}
	}
	if err := applyOwnership(tmpName, cleanPath, c); err != nil {
		return err
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	committed = true
	if err := commitDirSync(dir, c.noSync); err != nil {
		return err
	}
	return nil
}

// PendingFile is a pending temporary file, waiting to replace the
// destination path in a call to CommitFile. It embeds *os.File so
// callers get full io.Writer/io.ReaderFrom/fmt.Fprintf support.
type PendingFile struct {
	*os.File
	logger *slog.Logger
	cfg    *cfg
	path   string
	dir    string
	mode   os.FileMode
	done   bool
}

// NewPendingFile creates a temporary file destined to atomically replace
// the file at path. Write to it, then call CommitFile to finalize or
// Cleanup to abort.
func NewPendingFile(path string, mode os.FileMode, opts ...Option) (*PendingFile, error) {
	c := buildCfg(opts)
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return nil, err
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return nil, symErr
	}
	dir := filepath.Dir(cleanPath)
	if mkErr := ensureDir(dir, c); mkErr != nil {
		return nil, mkErr
	}
	mode = resolveMode(cleanPath, mode, c)
	tmp, err := os.CreateTemp(dir, c.tempPattern)
	if err != nil {
		return nil, &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	return &PendingFile{
		File:   tmp,
		path:   cleanPath,
		dir:    dir,
		mode:   mode,
		logger: c.logger,
		cfg:    c,
	}, nil
}

// CommitFile syncs, closes, and atomically renames the temp file to the
// destination path. After CommitFile, Cleanup is a no-op.
func (p *PendingFile) CommitFile() error {
	if p.done {
		return nil
	}
	p.done = true
	tmpName := p.Name()
	if err := p.Chmod(p.mode); err != nil {
		p.Close()
		removeTemp(tmpName, p.logger)
		return &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if !p.cfg.noSync {
		if err := p.Sync(); err != nil {
			p.Close()
			removeTemp(tmpName, p.logger)
			return &WriteError{Phase: PhaseTempSync, Err: err}
		}
	}
	if err := p.Close(); err != nil {
		removeTemp(tmpName, p.logger)
		return &WriteError{Phase: PhaseTempClose, Err: err}
	}
	if err := applyOwnership(tmpName, p.path, p.cfg); err != nil {
		removeTemp(tmpName, p.logger)
		return err
	}
	if err := os.Rename(tmpName, p.path); err != nil {
		removeTemp(tmpName, p.logger)
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	if err := commitDirSync(p.dir, p.cfg.noSync); err != nil {
		return err
	}
	return nil
}

// Cleanup closes and removes the temp file. It is a no-op if CommitFile
// has already been called. Safe to defer immediately after NewPendingFile.
func (p *PendingFile) Cleanup() error {
	if p.done {
		return nil
	}
	p.done = true
	tmpName := p.Name()
	p.Close()
	if err := os.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Prepare creates a temp file with data written, synced, and closed —
// ready for a final rename via Commit.
func Prepare(ctx context.Context, path string, data []byte, opts ...Option) (tmpPath string, cleanup func(), err error) {
	c := buildCfg(opts)
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return "", nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", nil, fmt.Errorf("atomic write: %w", ctxErr)
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return "", nil, symErr
	}
	dir := filepath.Dir(cleanPath)
	if mkErr := ensureDir(dir, c); mkErr != nil {
		return "", nil, mkErr
	}
	tmp, err := os.CreateTemp(dir, c.tempPattern)
	if err != nil {
		return "", nil, &WriteError{Phase: PhaseTempCreate, Err: err}
	}
	tmpName := tmp.Name()
	doCleanup := func() { removeTemp(tmpName, c.logger) }
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
	mode := resolveMode(cleanPath, c.mode, c)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		doCleanup()
		return "", nil, &WriteError{Phase: PhaseTempChmod, Err: err}
	}
	if !c.noSync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			doCleanup()
			return "", nil, &WriteError{Phase: PhaseTempSync, Err: err}
		}
	}
	if err := ctx.Err(); err != nil {
		tmp.Close()
		doCleanup()
		return "", nil, fmt.Errorf("atomic write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		doCleanup()
		return "", nil, &WriteError{Phase: PhaseTempClose, Err: err}
	}
	if ownErr := applyOwnership(tmpName, cleanPath, c); ownErr != nil {
		doCleanup()
		return "", nil, ownErr
	}
	return tmpName, doCleanup, nil
}

// Commit renames the prepared temp file to the final path and fsyncs
// the parent directory. Pass the SAME options (notably WithNoSync)
// that were given to Prepare: the durability barrier spans both calls,
// and a mismatch (e.g. WithNoSync on Commit but not Prepare) drops the
// parent-dir fsync that makes the rename durable.
func Commit(tmpPath, finalPath string, opts ...Option) error {
	c := buildCfg(opts)
	cleanFinal, err := validateAbsClean(finalPath)
	if err != nil {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			c.logger.Debug("atomic write: temp cleanup after path validation error", "path", tmpPath, "error", rmErr)
		}
		return err
	}
	if symErr := checkSymlink(cleanFinal, c); symErr != nil {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			c.logger.Debug("atomic write: temp cleanup after symlink refusal", "path", tmpPath, "error", rmErr)
		}
		return symErr
	}
	if err := os.Rename(tmpPath, cleanFinal); err != nil {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			c.logger.Debug("atomic write: temp cleanup after rename error", "path", tmpPath, "error", rmErr)
		}
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	if err := commitDirSync(filepath.Dir(cleanFinal), c.noSync); err != nil {
		return err
	}
	return nil
}

// SaveBytes writes raw bytes to path atomically, creating parent
// directories as needed. Directory permissions are 0755 for
// world-readable files, 0700 for private-only files.
func SaveBytes(path string, data []byte, perm os.FileMode, opts ...Option) error {
	c := buildCfg(opts)
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return err
	}
	if symErr := checkSymlink(cleanPath, c); symErr != nil {
		return symErr
	}
	dir := filepath.Dir(cleanPath)
	dirPerm := os.FileMode(0o755)
	if perm&0o077 == 0 {
		dirPerm = 0o700
	}
	if c.mkdirMode != 0 {
		dirPerm = c.mkdirMode
	}
	if mkErr := os.MkdirAll(dir, dirPerm); mkErr != nil {
		return mkErr
	}
	perm = resolveMode(cleanPath, perm, c)
	tmpName, err := writeTempFile(dir, filepath.Base(cleanPath), data, perm, c)
	if err != nil {
		return err
	}
	if err := applyOwnership(tmpName, cleanPath, c); err != nil {
		removeTemp(tmpName, c.logger)
		return err
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		removeTemp(tmpName, c.logger)
		return &WriteError{Phase: PhaseRename, Err: err}
	}
	if err := commitDirSync(dir, c.noSync); err != nil {
		return err
	}
	return nil
}

// SaveJSON marshals v to indented JSON and writes it atomically.
// mu must not be nil; it serializes concurrent writes to the same path.
// label identifies the caller in log output for diagnostics.
func SaveJSON(path string, mu *sync.Mutex, v any, label string, perm os.FileMode, opts ...Option) error {
	if mu == nil {
		return errors.New("atomicfile.SaveJSON: nil mutex")
	}
	mu.Lock()
	defer mu.Unlock()
	c := buildCfg(opts)
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		c.logger.Error("atomicfile.SaveJSON: marshal failed", "label", label, "error", err)
		return err
	}
	if len(data) > largeSaveJSONThreshold {
		c.logger.Warn("atomicfile.SaveJSON: large payload",
			"label", label, "bytes", len(data),
			"threshold", largeSaveJSONThreshold)
	}
	if err := SaveBytes(path, data, perm, opts...); err != nil {
		if we, ok := errors.AsType[*WriteError](err); ok && we.Phase == PhaseDirSync {
			c.logger.Warn("atomicfile.SaveJSON: written but parent-dir fsync failed (durability not guaranteed)",
				"label", label, "path", path, "error", err)
		} else {
			c.logger.Error("atomicfile.SaveJSON: write failed", "label", label, "path", path, "error", err)
		}
		return err
	}
	return nil
}

// LoadJSON reads a JSON file with size bounds and unmarshals into v.
// Symmetric with SaveJSON. maxBytes limits the file size to prevent
// unbounded memory allocation.
func LoadJSON(ctx context.Context, path string, maxBytes int64, v any) error {
	data, err := ReadBounded(ctx, path, maxBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ReadBounded opens a file, validates its size against maxBytes, and
// reads it with a LimitReader. Returns ErrFileTooLarge if the file
// exceeds the limit.
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, fi.Size(), maxBytes)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	data, err := io.ReadAll(io.LimitReader(f, saturateAdd(maxBytes, 1)))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: file grew past %d byte limit during read", ErrFileTooLarge, maxBytes)
	}
	return data, nil
}

// fsyncDir fsyncs a directory to make a prior rename durable across a crash.
// The parent-directory fsync is the step that guarantees the renamed entry
// survives power loss, so its failure is returned to the caller (wrapped as
// PhaseDirSync) rather than swallowed. The rename has already succeeded when
// this runs, so a non-nil return means "written but durability not
// guaranteed", not "write failed".
//
// It is a package var so tests can inject a directory-fsync failure; a real
// fsync(2) on a directory fd is impractical to force on a healthy filesystem.
// Production code never reassigns it.
var fsyncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// commitDirSync makes a completed rename durable by fsyncing its parent
// directory, unless noSync is set. A failure is wrapped as PhaseDirSync; the
// rename has already succeeded, so the new content is at the final path even
// when this returns an error.
func commitDirSync(dir string, noSync bool) error {
	if noSync {
		return nil
	}
	if err := fsyncDir(dir); err != nil {
		return &WriteError{Phase: PhaseDirSync, Err: err}
	}
	return nil
}

// writeTempFile creates a temp file in dir, writes data, fsyncs, closes,
// and chmods. Returns the temp file name on success.
func writeTempFile(dir, baseName string, data []byte, perm os.FileMode, c *cfg) (tmpName string, retErr error) {
	pattern := baseName + ".tmp-*"
	if c.tempPattern != DefaultTempPrefix {
		pattern = c.tempPattern
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if !c.noSync {
		if err = tmp.Sync(); err != nil {
			_ = tmp.Close()
			return "", err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return name, nil
}

// saturateAdd returns a + b clamped to math.MaxInt64 on overflow.
func saturateAdd(a, b int64) int64 {
	sum := a + b
	if sum < a {
		return math.MaxInt64
	}
	return sum
}

// isStaleTempName reports whether name matches either temp-naming
// convention this package emits: the SaveBytes/SaveJSON style
// "<base>.tmp-<random>", and the default CreateTemp pattern
// DefaultTempPrefix (".atomicfile-<random>.tmp") used by WriteFile,
// WriteReader, Prepare, and PendingFile.
func isStaleTempName(name string) bool {
	tag := ".tmp-"
	if i := strings.LastIndex(name, tag); i >= 0 && i+len(tag) < len(name) {
		tail := name[i+len(tag):]
		if !strings.ContainsAny(tail, "./\\") {
			return true
		}
	}
	if pre, suf, ok := strings.Cut(DefaultTempPrefix, "*"); ok &&
		len(name) > len(pre)+len(suf) &&
		strings.HasPrefix(name, pre) && strings.HasSuffix(name, suf) {
		return true
	}
	return false
}

// CleanupStaleTemps removes stale temp files in dir older than maxAge.
// It recognizes only the two built-in temp-naming conventions
// (DefaultTempPrefix ".atomicfile-*.tmp" and "<base>.tmp-<random>").
// Temp files created with a custom WithTempPattern are NOT reclaimed,
// even if the same option is passed here. Best-effort; errors are
// logged but not returned.
func CleanupStaleTemps(dir string, maxAge time.Duration, opts ...Option) {
	c := buildCfg(opts)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("atomicfile.CleanupStaleTemps: readdir failed", "dir", dir, "error", err)
		}
		return
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	failed := 0
	for _, e := range entries {
		name := e.Name()
		if !isStaleTempName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.logger.Debug("atomicfile.CleanupStaleTemps: stat failed", "name", name, "error", err)
			}
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		full := filepath.Join(dir, name)
		if err := os.Remove(full); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				c.logger.Debug("atomicfile.CleanupStaleTemps: remove failed", "path", full, "error", err)
				failed++
			}
			continue
		}
		c.logger.Debug("atomicfile.CleanupStaleTemps: removed stale temp", "path", full, "age", time.Since(info.ModTime()))
		removed++
	}
	if removed > 0 {
		c.logger.Info("atomicfile.CleanupStaleTemps: removed stale temps", "dir", dir, "count", removed)
	}
	if failed > 0 {
		c.logger.Warn("atomicfile.CleanupStaleTemps: some stale temps could not be removed", "dir", dir, "failed", failed)
	}
}
