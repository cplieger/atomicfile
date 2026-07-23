# atomicfile

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/atomicfile/v2.svg)](https://pkg.go.dev/github.com/cplieger/atomicfile/v2)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/atomicfile)](https://github.com/cplieger/atomicfile/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/coverage.json)](https://github.com/cplieger/atomicfile/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/mutation.json)](https://github.com/cplieger/atomicfile/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13198/badge)](https://www.bestpractices.dev/projects/13198)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/atomicfile/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/atomicfile)

> Crash-safe atomic file writes for Go

A standalone Go library providing atomic file writes (temp→fsync→rename→dir-fsync), path cleaning with `os.Root`-based containment, bounded reads, and streaming writes. Standard-library only, no external runtime dependencies.

Every write runs through one `os.Root`-confined engine: the `*InRoot` APIs use the caller's root directly, and the absolute-path APIs open the target's parent directory as a root and write through it. There is exactly one guard preamble, one temp-side barrier, and one commit-side barrier (rename + parent-dir fsync) in the package.

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

All write primitives return `(Result, error)`; inspect `Result.Durable` for crash durability (see [Result and Durability](#result-and-durability) for the nil-error contract).

- `WriteFile(ctx, path, data, opts ...Option) (Result, error)` — atomic write (default mode 0644)
- `WriteReader(ctx, path, r, opts ...Option) (Result, error)` — atomic write from `io.Reader` (uses the `io.WriterTo` fast path when available; mode via `WithMode`)
- `WriteFileInRoot(ctx, root, name, data, opts ...Option) (Result, error)` — atomic write of `data` to `name` relative to an `*os.Root`; every filesystem op runs through the root, so a symlink or `..` in `name` cannot escape its tree
- `WriteReaderInRoot(ctx, root, name, r, opts ...Option) (Result, error)` — same, streaming from an `io.Reader`

### Streaming Writer

- `NewPendingFile(ctx, path, opts ...Option) (*PendingFile, error)` — open a temp file for incremental writing (mode via `WithMode`)
- `NewPendingFileInRoot(ctx, root, name, opts ...Option) (*PendingFile, error)` — same, confined to an `*os.Root`: the temp, rename, and parent-dir fsync all run through the caller's root, which stays caller-owned (keep it open through Commit/Cleanup)
- `(*PendingFile).Commit(ctx) (Result, error)` — chmod + fsync + close + rename + dir-fsync (finalize). Idempotent: repeated calls return the first result. Returns `ErrAborted` if called after `Cleanup`.
- `(*PendingFile).Cleanup() error` — close + remove (abort; no-op after Commit, idempotent). Safe to `defer` immediately after `NewPendingFile`.

`PendingFile` embeds `*os.File`, providing the full `io.Writer`/`io.ReaderFrom`/`fmt.Fprintf` surface; its `Name()` reports the staged temp's path, so an external verifier can inspect the temp before `Commit` publishes it. It is written as an append-only stream: `Write`/`WriteString`/`ReadFrom` maintain a byte count (`BytesWritten()`), `Truncate` re-syncs it, and a `WithMaxBytes` cap is enforced on exactly that stream — the call that would cross the cap is rejected whole, so the staged temp never holds an over-cap prefix. `Commit` then re-verifies the staged file's actual size against the cap at the durability barrier, so bytes staged outside the stream (`WriteAt`, `Write` after `Seek`, a reopen of the temp by path) cannot publish an over-cap file either.

### Read Functions

- `ReadBounded(ctx, path, maxBytes) ([]byte, error)` — size-checked read; returns `ErrFileTooLarge` past the limit
- `ReadBoundedFile(ctx, f, maxBytes) ([]byte, error)` — size-checked read from an already-open `*os.File` (the seam for reading a file opened through an `*os.Root`); the caller owns and closes `f`

### Utilities

- `CleanupStaleTemps(dir, maxAge, opts ...Option) (removed int, err error)` — remove orphaned temp files left by interrupted writes, returning how many were removed. Only files matching the package's exact temp shape — `.atomicfile-<digits>.tmp`, with a crypto/rand decimal digit run in the middle — older than `maxAge` are reclaimed; caller-owned files that merely share the prefix or suffix (e.g. `.atomicfile-notes.tmp`, `config.tmp-backup`) are never touched. The reap decision is taken from a fresh `Lstat` immediately before each unlink, so a same-named fresh temp created between the directory scan and the removal is spared.

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

| Option                     | Description                                                                                                                                                                                                                                               |
|----------------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `WithLogger(l)`            | Custom `*slog.Logger` for diagnostic output (default: `slog.Default()`)                                                                                                                                                                                   |
| `WithMode(mode)`           | File permission (default: `0o644`)                                                                                                                                                                                                                        |
| `WithMkdirMode(mode)`      | Create the parent directory (and missing ancestors) with this mode before writing. Without it, a missing parent is an error.                                                                                                                              |
| `WithPreserveMode()`       | Stat the target and reuse its mode (like `renameio.WithExistingPermissions`), falling back to `WithMode` if it does not exist or cannot be stat-ed                                                                                                        |
| `WithPreserveOwnership()`  | Stat the target and chown the temp to match its uid/gid (requires CAP_CHOWN; no-op when the target is absent, cannot be stat-ed, or off Unix)                                                                                                             |
| `WithNoSync()`             | Skip fsync for speed (atomicity without durability). `Result.Durable` is then always false.                                                                                                                                                               |
| `WithMaxBytes(n)`          | Cap staged content at `n` bytes — the write-side mirror of `ReadBounded`, so a writer can refuse to persist what its own read path would refuse to load. Over-cap writes match `ErrFileTooLarge` and leave the previous target intact. `n <= 0` = no cap. |
| `WithAllowSymlinkTarget()` | Permit writing to a symlink path (default: refuse with `ErrSymlinkTarget`)                                                                                                                                                                                |

## Errors

| Sentinel           | Meaning                                                                                        |
|--------------------|------------------------------------------------------------------------------------------------|
| `ErrEmptyPath`     | The path argument was empty                                                                    |
| `ErrUnsafePath`    | The path is not absolute or contains a null byte                                               |
| `ErrFileTooLarge`  | The file exceeded the `ReadBounded` size limit, or content exceeded a `WithMaxBytes` write cap |
| `ErrSymlinkTarget` | The target is a symlink and `WithAllowSymlinkTarget` was not set                               |
| `ErrAborted`       | `PendingFile.Commit` was called after `Cleanup` aborted the pending write                      |

The package-level path check is not a containment boundary. `filepath.Clean` normalizes any `..` in an absolute path rather than rejecting it (for an absolute path there is nothing to escape), so `ErrUnsafePath` only guards against a non-absolute or null-byte path. Callers that need to confine writes to a directory tree use the `*os.Root`-backed write APIs (`WriteFileInRoot` / `WriteReaderInRoot`). Callers that need to confine reads should open the file through an `*os.Root` and pass that already-confined handle to `ReadBoundedFile`, which then applies the same size and context bounds.

Failures in the write barrier (open destination / create temp, write, chmod, sync, close, rename) are reported as `*WriteError{Err, Phase}`, where `Phase` is one of `PhaseTempCreate`, `PhaseTempWrite`, `PhaseTempChmod`, `PhaseTempSync`, `PhaseTempClose`, or `PhaseRename`. `PhaseTempCreate` covers opening the destination for writing — for the absolute-path entry points that includes opening the target's parent directory as an `*os.Root` (a missing parent without `WithMkdirMode` surfaces here) — as well as creating the temp file inside it. Pre-barrier failures are returned as their own error types: path-validation and symlink failures use the sentinels above, context failures wrap the standard-library context error (`context.Canceled` / `context.DeadlineExceeded`), and a `WithMkdirMode` parent-directory creation failure wraps the underlying os error behind the `atomicfile:` prefix. All remain inspectable with `errors.Is` / `errors.As`. A `*WriteError` always means the data did **not** reach its final path; use `errors.As` to inspect it.

## Durability Guarantees

Every atomic write follows the sequence: create temp file → write data → fsync temp → close → rename to final path → fsync parent directory. This ensures that after a crash, the file contains either the complete old content or the complete new content — never a partial write. The directory fsync makes the rename durable even if the system loses power immediately after the call returns.

If the parent-directory fsync fails (for example an `EIO` from a failing disk), the rename has already completed. This is the nil-error, `Result.Durable == false` outcome described under [Result and Durability](#result-and-durability); the write also logs the fsync failure at `Warn`. Callers that require strict durability treat `Durable == false` as actionable (retry or alert); callers that only need atomicity can ignore it or use `WithNoSync()` to skip the directory fsync entirely.

> **Note on auto-created directories.** When `WithMkdirMode` creates new parent
> directories, only the file's immediate parent is fsynced. The newly created
> intermediate directories are not fsynced into their own parents, so the
> durability guarantee above applies only when the full parent path already
> exists. If you need durability into a freshly created directory tree, create
> and fsync the directories before writing.

## Symlink Safety

By default, all write functions refuse to write to a path that is currently a symlink, returning `ErrSymlinkTarget`. This is because `os.Rename` replaces the symlink itself rather than the file it points to — which is rarely the caller's intent and can lead to data loss or security issues.

To opt in to writing through a symlink (replacing the symlink with a regular file), use `WithAllowSymlinkTarget()`. To write to the link's target instead, resolve it with `filepath.EvalSymlinks` first.

> **Metadata reads under `WithAllowSymlinkTarget` (v2.2 behavior note).** Since
> the absolute-path entry points adopted the root-confined engine, the
> symlink-following stat that `WithPreserveMode` / `WithPreserveOwnership`
> perform runs through an `os.Root` of the target's parent directory. A symlink
> target whose destination is absolute or escapes that directory is therefore no
> longer followed for the metadata read: preserve-mode falls back to the
> `WithMode` value and preserve-ownership becomes a no-op for such links (the
> write itself replaces the link as before). Links resolving within the parent
> directory behave unchanged. This tightens the previously documented
> metadata-influence window; no other option semantics changed.

Reads behave differently: `ReadBounded` follows symlinks by design (`os.Open` resolves them), so it does NOT refuse a symlink at `path`. When reading from a directory writable by a less-trusted principal, confine the path yourself -- open the file through an `*os.Root` (Go 1.24+) and read it with `ReadBoundedFile`, which applies the same size and context bounds without following symlinks out of the root.

## Unsupported by Design

| Feature                             | Rationale                                                                                                                                                                                                                 |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Windows rename-over semantics**   | Target platform is Linux. `os.Rename` is atomic on Linux. Windows cannot guarantee atomicity ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). google/renameio also refuses Windows. |
| **`fs.FS` interop**                 | `fs.FS` is a read-only interface. Atomic writes are inherently outside its scope.                                                                                                                                         |
| **Atomic symlink replacement**      | Out of scope. Use google/renameio if needed.                                                                                                                                                                              |
| **Umask-aware permissions**         | The library uses `Chmod` for exact permissions (ignoring umask). This is the correct secure default for server/CLI tools. Equivalent to renameio's `WithStaticPermissions`.                                               |
| **`TempDir` cross-mount detection** | Temp files are always created in the target directory (same mount point), the only correct approach for atomic rename.                                                                                                    |

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0 — see [LICENSE](LICENSE).
