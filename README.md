# atomicfile

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/atomicfile/v2.svg)](https://pkg.go.dev/github.com/cplieger/atomicfile/v2)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/atomicfile)](https://github.com/cplieger/atomicfile/blob/main/go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/atomicfile)](https://goreportcard.com/report/github.com/cplieger/atomicfile)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/coverage.json)](https://github.com/cplieger/atomicfile/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/mutation.json)](https://github.com/cplieger/atomicfile/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13198/badge)](https://www.bestpractices.dev/projects/13198)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/atomicfile/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/atomicfile)

> Crash-safe atomic file writes for Go

A standalone Go library providing atomic file writes (temp→fsync→rename→dir-fsync), path-traversal validation, bounded reads, and streaming writes. Standard-library only, no external runtime dependencies.

## Platform Support

**Linux only (including Docker/containers).** Windows is unsupported by design — `os.Rename` cannot guarantee atomicity on Windows ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). macOS/BSD may work but is untested.

## Install

`go get github.com/cplieger/atomicfile/v2@latest`

## Usage

```go
package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/cplieger/atomicfile/v2"
)

func main() {
	ctx := context.Background()

	// Atomic write with default mode (0644). The returned Result reports the
	// final path and whether the write is crash-durable.
	res, err := atomicfile.WriteFile(ctx, "/tmp/data.txt", []byte("hello"))
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (durable=%v)", res.Path, res.Durable)

	// Atomic write with custom mode.
	atomicfile.WriteFile(ctx, "/tmp/secret.txt", []byte("s3cr3t"),
		atomicfile.WithMode(0o600))

	// Streaming write from an io.Reader (mode via WithMode).
	atomicfile.WriteReader(ctx, "/tmp/stream.txt", strings.NewReader("streamed"),
		atomicfile.WithMode(0o644))

	// PendingFile for incremental writes (mirrors google/renameio).
	pf, _ := atomicfile.NewPendingFile(ctx, "/tmp/pending.txt")
	defer pf.Cleanup()
	pf.Write([]byte("incremental"))
	pf.Commit(ctx)

	// Preserve existing file permissions across replace.
	atomicfile.WriteFile(ctx, "/tmp/data.txt", []byte("updated"),
		atomicfile.WithPreserveMode())

	// Auto-create parent directories.
	atomicfile.WriteFile(ctx, "/tmp/nested/dir/file.txt", []byte("deep"),
		atomicfile.WithMkdirMode(0o755))

	// Skip fsync for speed (atomicity without durability).
	atomicfile.WriteFile(ctx, "/tmp/cache.txt", []byte("fast"),
		atomicfile.WithNoSync())

	// Confined I/O through an *os.Root (Go 1.24+): name is relative to the root,
	// and a symlink or ".." in it can never escape the root's tree.
	root, _ := os.OpenRoot("/tmp")
	defer root.Close()
	atomicfile.WriteFileInRoot(ctx, root, "rooted.txt", []byte("confined"))

	// Read it back through the same root: open via the root (so the path stays
	// confined), then bound the read with ReadBoundedFile.
	rf, _ := root.Open("rooted.txt")
	defer rf.Close()
	rooted, _ := atomicfile.ReadBoundedFile(ctx, rf, 1<<20)
	log.Printf("read %d confined bytes", len(rooted))

	// Bounded read.
	data, _ := atomicfile.ReadBounded(ctx, "/tmp/data.txt", 1<<20)
	log.Printf("read %d bytes", len(data))
}
```

## API

### Write Functions

All write primitives return `(Result, error)`. A nil error means the data reached its final path; inspect `Result.Durable` for crash durability.

- `WriteFile(ctx, path, data, opts ...Option) (Result, error)` — atomic write (default mode 0644)
- `WriteReader(ctx, path, r, opts ...Option) (Result, error)` — atomic write from `io.Reader` (uses the `io.WriterTo` fast path when available; mode via `WithMode`)
- `WriteFileInRoot(ctx, root, name, data, opts ...Option) (Result, error)` — atomic write of `data` to `name` relative to an `*os.Root`; every filesystem op runs through the root, so a symlink or `..` in `name` cannot escape its tree
- `WriteReaderInRoot(ctx, root, name, r, opts ...Option) (Result, error)` — same, streaming from an `io.Reader`

### Streaming Writer

- `NewPendingFile(ctx, path, opts ...Option) (*PendingFile, error)` — open a temp file for incremental writing (mode via `WithMode`)
- `(*PendingFile).Commit(ctx) (Result, error)` — chmod + fsync + close + rename + dir-fsync (finalize). Idempotent: repeated calls return the first result. Returns `ErrAborted` if called after `Cleanup`.
- `(*PendingFile).Cleanup() error` — close + remove (abort; no-op after Commit, idempotent). Safe to `defer` immediately after `NewPendingFile`.

`PendingFile` embeds `*os.File`, providing the full `io.Writer`/`io.ReaderFrom`/`fmt.Fprintf` surface.

### Read Functions

- `ReadBounded(ctx, path, maxBytes) ([]byte, error)` — size-checked read; returns `ErrFileTooLarge` past the limit
- `ReadBoundedFile(ctx, f, maxBytes) ([]byte, error)` — size-checked read from an already-open `*os.File` (the seam for reading a file opened through an `*os.Root`); the caller owns and closes `f`

### Utilities

- `CleanupStaleTemps(dir, maxAge, opts ...Option) (removed int, err error)` — remove orphaned temp files left by interrupted writes, returning how many were removed. Only files matching the package's exact temp shape — `.atomicfile-<digits>.tmp` — older than `maxAge` are reclaimed; caller-owned files that merely share the prefix or suffix (e.g. `.atomicfile-notes.tmp`, `config.tmp-backup`) are never touched.

## Result and Durability

```go
type Result struct {
	Path    string // cleaned final path (absolute for the package-level writers; root-relative for WriteFileInRoot/WriteReaderInRoot)
	Durable bool   // true only when file + parent-dir fsync both completed
}
```

A nil error means the data reached its final path: the write either fully succeeded, or — at worst — was renamed into place but the parent-directory fsync failed. `Result.Durable` distinguishes those two outcomes, so a caller that cares about crash durability inspects `Result.Durable` rather than decoding error types. A non-nil error always means the data did **not** reach its final path.

## Functional Options

All write functions accept variadic `Option` values. Omit options for defaults.

| Option                     | Description                                                                                                                                        |
| -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WithLogger(l)`            | Custom `*slog.Logger` for diagnostic output (default: `slog.Default()`)                                                                            |
| `WithMode(mode)`           | File permission (default: `0o644`)                                                                                                                 |
| `WithMkdirMode(mode)`      | Create the parent directory (and missing ancestors) with this mode before writing. Without it, a missing parent is an error.                       |
| `WithPreserveMode()`       | Stat the target and reuse its mode (like `renameio.WithExistingPermissions`), falling back to `WithMode` if it does not exist or cannot be stat-ed |
| `WithPreserveOwnership()`  | Stat the target and chown the temp to match its uid/gid (requires CAP_CHOWN; no-op when the target is absent, cannot be stat-ed, or off Unix)      |
| `WithNoSync()`             | Skip fsync for speed (atomicity without durability). `Result.Durable` is then always false.                                                        |
| `WithAllowSymlinkTarget()` | Permit writing to a symlink path (default: refuse with `ErrSymlinkTarget`)                                                                         |

## Errors

| Sentinel           | Meaning                                                                         |
| ------------------ | ------------------------------------------------------------------------------- |
| `ErrEmptyPath`     | The path argument was empty                                                     |
| `ErrUnsafePath`    | The path failed the local safety check (relative, null byte, or `..` traversal) |
| `ErrFileTooLarge`  | The file exceeded the `ReadBounded` size limit                                  |
| `ErrSymlinkTarget` | The target is a symlink and `WithAllowSymlinkTarget` was not set                |
| `ErrAborted`       | `PendingFile.Commit` was called after `Cleanup` aborted the pending write       |

Failures in the write barrier (create temp, write, chmod, sync, close, rename) are reported as `*WriteError{Err, Phase}`, where `Phase` is one of `PhaseTempCreate`, `PhaseTempWrite`, `PhaseTempChmod`, `PhaseTempSync`, `PhaseTempClose`, or `PhaseRename`. Pre-barrier failures are returned as their own error types: path-validation and symlink failures use the sentinels above, context failures wrap the standard-library context error (`context.Canceled` / `context.DeadlineExceeded`), and a `WithMkdirMode` parent-directory creation failure wraps the underlying os error behind the `atomicfile:` prefix. All remain inspectable with `errors.Is` / `errors.As`. A `*WriteError` always means the data did **not** reach its final path; use `errors.As` to inspect it.

## Durability Guarantees

Every atomic write follows the sequence: create temp file → write data → fsync temp → close → rename to final path → fsync parent directory. This ensures that after a crash, the file contains either the complete old content or the complete new content — never a partial write. The directory fsync makes the rename durable even if the system loses power immediately after the call returns.

If the parent-directory fsync fails (for example an `EIO` from a failing disk), the rename has already completed, so the new content is present at the final path. The write therefore returns a **nil error** with `Result.Durable == false`, and logs the fsync failure at `Warn`. Callers that require strict durability treat `Durable == false` as actionable (retry or alert); callers that only need atomicity can ignore it or use `WithNoSync()` to skip the directory fsync entirely.

> **Note on auto-created directories.** When `WithMkdirMode` creates new parent
> directories, only the file's immediate parent is fsynced. The newly created
> intermediate directories are not fsynced into their own parents, so the
> durability guarantee above applies only when the full parent path already
> exists. If you need durability into a freshly created directory tree, create
> and fsync the directories before writing.

## Symlink Safety

By default, all write functions refuse to write to a path that is currently a symlink, returning `ErrSymlinkTarget`. This is because `os.Rename` replaces the symlink itself rather than the file it points to — which is rarely the caller's intent and can lead to data loss or security issues.

To opt in to writing through a symlink (replacing the symlink with a regular file), use `WithAllowSymlinkTarget()`. To write to the link's target instead, resolve it with `filepath.EvalSymlinks` first.

Reads behave differently: `ReadBounded` follows symlinks by design (`os.Open` resolves them), so it does NOT refuse a symlink at `path`. When reading from a directory writable by a less-trusted principal, confine the path yourself -- open the file through an `*os.Root` (Go 1.24+) and read it with `ReadBoundedFile`, which applies the same size and context bounds without following symlinks out of the root.

## Temp Files

Every temp file this package creates is named `.atomicfile-<digits>.tmp` (`os.CreateTemp` replaces the pattern's `*` with a decimal random string). `CleanupStaleTemps` reclaims orphaned temps of exactly that shape and nothing else, so it never deletes a caller-owned file.

## Unsupported by Design

| Feature                             | Rationale                                                                                                                                                                                                                 |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Windows rename-over semantics**   | Target platform is Linux. `os.Rename` is atomic on Linux. Windows cannot guarantee atomicity ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). google/renameio also refuses Windows. |
| **`fs.FS` interop**                 | `fs.FS` is a read-only interface. Atomic writes are inherently outside its scope.                                                                                                                                         |
| **Atomic symlink replacement**      | Out of scope. Use google/renameio if needed.                                                                                                                                                                              |
| **Umask-aware permissions**         | The library uses `Chmod` for exact permissions (ignoring umask). This is the correct secure default for server/CLI tools. Equivalent to renameio's `WithStaticPermissions`.                                               |
| **`TempDir` cross-mount detection** | Temp files are always created in the target directory (same mount point), the only correct approach for atomic rename.                                                                                                    |

## License

GPL-3.0 — see [LICENSE](LICENSE).
