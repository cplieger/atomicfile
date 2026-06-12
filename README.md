# atomicfile

[![CI](https://github.com/cplieger/atomicfile/actions/workflows/ci.yaml/badge.svg)](https://github.com/cplieger/atomicfile/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/atomicfile.svg)](https://pkg.go.dev/github.com/cplieger/atomicfile)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/atomicfile)](https://goreportcard.com/report/github.com/cplieger/atomicfile)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/atomicfile/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/atomicfile)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)

> Crash-safe atomic file writes for Go

A standalone Go library providing atomic file writes (temp→fsync→rename→dir-fsync), path-traversal validation, bounded reads, streaming writes, and JSON helpers. Standard-library only, no external dependencies.

## Platform Support

**Linux only (including Docker/containers).** Windows is unsupported by design — `os.Rename` cannot guarantee atomicity on Windows ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). macOS/BSD may work but is untested.

## Install

`go get github.com/cplieger/atomicfile@latest`

## Usage

```go
package main

import (
	"context"
	"strings"
	"sync"

	"github.com/cplieger/atomicfile"
)

func main() {
	ctx := context.Background()

	// Atomic write with default mode (0644)
	atomicfile.WriteFile(ctx, "/tmp/data.txt", []byte("hello"))

	// Atomic write with custom mode
	atomicfile.WriteFile(ctx, "/tmp/secret.txt", []byte("s3cr3t"),
		atomicfile.WithMode(0o600))

	// Streaming write from io.Reader
	atomicfile.WriteReader(ctx, "/tmp/stream.txt", strings.NewReader("streamed"), 0o644)

	// PendingFile for incremental writes (mirrors google/renameio)
	pf, _ := atomicfile.NewPendingFile("/tmp/pending.txt", 0o644)
	defer pf.Cleanup()
	pf.Write([]byte("incremental"))
	pf.CommitFile()

	// Preserve existing file permissions across replace
	atomicfile.WriteFile(ctx, "/tmp/data.txt", []byte("updated"),
		atomicfile.WithPreserveMode())

	// Auto-create parent directories
	atomicfile.WriteFile(ctx, "/tmp/nested/dir/file.txt", []byte("deep"),
		atomicfile.WithMkdirMode(0o755))

	// Skip fsync for speed (atomicity without durability)
	atomicfile.WriteFile(ctx, "/tmp/cache.txt", []byte("fast"),
		atomicfile.WithNoSync())

	// JSON round-trip
	var mu sync.Mutex
	atomicfile.SaveJSON("/tmp/config.json", &mu, map[string]int{"x": 1}, "app", 0o644)
	var cfg map[string]int
	atomicfile.LoadJSON(ctx, "/tmp/config.json", 1<<20, &cfg)
}
```

## API

### Write Functions

- `WriteFile(ctx, path, data, opts ...Option)` — atomic write (default mode 0644)
- `WriteReader(ctx, path, r, mode, opts ...Option)` — atomic write from `io.Reader` (uses `io.WriterTo` optimization when available)
- `Prepare(ctx, path, data, opts ...Option)` — create temp file ready for commit
- `Commit(tmpPath, finalPath, opts ...Option)` — rename temp to final + dir fsync
- `SaveBytes(path, data, perm, opts ...Option)` — atomic write with auto-mkdir
- `SaveJSON(path, mu, v, label, perm, opts ...Option)` — marshal + atomic write

### Streaming Writer

- `NewPendingFile(path, mode, opts ...Option)` — open a temp file for incremental writing
- `(*PendingFile).CommitFile()` — fsync + close + rename (finalize)
- `(*PendingFile).Cleanup()` — close + remove (abort; no-op after commit)

`PendingFile` embeds `*os.File`, providing full `io.Writer`/`io.ReaderFrom`/`fmt.Fprintf` support.

### Read Functions

- `ReadBounded(ctx, path, maxBytes)` — size-checked read
- `LoadJSON(ctx, path, maxBytes, v)` — bounded read + JSON unmarshal

### Utilities

- `CleanupStaleTemps(dir, maxAge, opts ...Option)` — remove stale temp files left by interrupted writes; recognizes both built-in temp schemes and, when a `WithTempPattern` option is passed, that custom pattern too (pass the same one you write with)

## Functional Options

All write functions accept variadic `Option` values to customize behavior. Omit options for defaults.

| Option | Description |
|--------|-------------|
| `WithLogger(l)` | Custom `*slog.Logger` for diagnostic output (default: `slog.Default()`) |
| `WithTempPattern(p)` | `os.CreateTemp` pattern for temp files (default: `".atomicfile-*.tmp"`) |
| `WithMode(mode)` | File permission (default: `0o644`). Used by `WriteFile` and `Prepare`. |
| `WithMkdirMode(mode)` | When set, create parent directories with this mode before writing |
| `WithPreserveMode()` | Stat target and reuse its mode (like `renameio.WithExistingPermissions`) |
| `WithPreserveOwnership()` | Stat target and chown temp to match uid/gid (requires CAP_CHOWN) |
| `WithNoSync()` | Skip fsync for speed (atomicity without durability, like google/renameio) |
| `WithAllowSymlinkTarget()` | Permit writing to a symlink path (default: refuse with `ErrSymlinkTarget`) |

## Durability Guarantees

Every atomic write follows the sequence: create temp file → write data → fsync temp → close → rename to final path → fsync parent directory. This ensures that after a crash, the file contains either the complete old content or the complete new content — never a partial write. The directory fsync makes the rename durable even if the system loses power immediately after the call returns.

If the parent-directory fsync fails (for example an `EIO` from a failing disk), the write returns a `*WriteError` with `Phase: PhaseDirSync`. The rename has already completed at that point, so the new content is present at the final path, but its durability across an immediate crash is not guaranteed. Callers that require strict durability should treat a `PhaseDirSync` error as actionable (retry or alert); callers that only need atomicity can ignore it or use `WithNoSync()` to skip the directory fsync entirely.

> **Note on auto-created directories.** When `WithMkdirMode` (or `SaveBytes`'s
> implicit `MkdirAll`) creates new parent directories, only the file's immediate
> parent is fsynced. The newly created intermediate directories are not fsynced
> into their own parents, so the durability guarantee above applies only when the
> full parent path already exists. If you need durability into a freshly created
> directory tree, create and fsync the directories before writing.

## Symlink Safety

By default, all write functions refuse to write to a path that is currently a symlink, returning `ErrSymlinkTarget`. This is because `os.Rename` replaces the symlink itself rather than the file it points to — which is rarely the caller's intent and can lead to data loss or security issues.

To opt in to writing through symlinks (replacing the symlink with a regular file), use `WithAllowSymlinkTarget()`.

## Durability vs Speed

By default, all writes are durable: temp files are fsynced before rename, and the parent directory is fsynced after rename. This ensures data survives power loss.

Use `WithNoSync()` for atomicity without durability (like google/renameio's design). The file will never be partially written, but may be lost entirely on power failure. Useful for caches, metrics, and other reconstructible data.

## Unsupported by Design

The following features are intentionally not implemented:

| Feature | Rationale |
|---------|-----------|
| **Windows rename-over semantics** | Target platform is Linux. `os.Rename` is atomic on Linux. Windows cannot guarantee atomicity ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). google/renameio also refuses Windows. |
| **`fs.FS` interop** | `fs.FS` is a read-only interface. Atomic writes are inherently outside its scope. |
| **Atomic symlink replacement** | Out of scope. Use google/renameio if needed. |
| **Umask-aware permissions** | The library uses `Chmod` for exact permissions (ignoring umask). This is the correct secure default for server/CLI tools. Equivalent to renameio's `WithStaticPermissions`. |
| **`TempDir` cross-mount detection** | Temp files are always created in the target directory (same mount point), the only correct approach for atomic rename. |
| **`os.Root` scoped operations** | Niche security feature not needed by target consumers. |
| **Context on `SaveBytes`/`SaveJSON`** | Convenience wrappers for small payloads. Use `WriteFile`/`WriteReader` for cancellable operations. |

## License

GPL-3.0 — see [LICENSE](LICENSE).
