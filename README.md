# atomicfile

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/atomicfile/v4.svg)](https://pkg.go.dev/github.com/cplieger/atomicfile/v4) [![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/atomicfile)](https://github.com/cplieger/atomicfile/blob/main/go.mod) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/mutation.json)](https://github.com/cplieger/atomicfile/issues?q=label%3Agremlins-tracker)

atomicfile writes files from Go so that a crash or a power loss leaves the complete old content or the complete new content, never a partial file.

It replaces the temp file, fsync and rename sequence you would otherwise write around `os.WriteFile`. It runs on Linux, containers included, uses only the standard library at run time, needs Go 1.27 or later and is licensed under Apache-2.0. macOS and BSD may work but are untested.

## Why use it

atomicfile is built for Go services that keep state, configuration or credentials on disk and must not lose them to a crash.

- Every write syncs the file and its parent directory, and `Result.Durable` reports whether every sync succeeded.
- A nil error means the new data is at the path. Any other error leaves the old file in place, and a failure inside the write is a `*WriteError` naming the step that failed.
- `WithMode` proves the mode the filesystem stored, so a filesystem that widens a 0600 request fails the write instead of publishing a readable file.
- The `*InRoot` functions run through an `*os.Root`, so a symlink or `..` in a name cannot leave the directory tree.
- Writes refuse a symlink, FIFO, socket or device node at the target path.

Consider [google/renameio](https://github.com/google/renameio) if you also need to replace a symbolic link atomically.

## Install

```sh
go get github.com/cplieger/atomicfile/v4@latest
```

## Usage

```go
package main

import (
	"context"
	"log"
	"os"

	"github.com/cplieger/atomicfile/v4"
)

func main() {
	ctx := context.Background()

	// Replace a file. Result.Durable reports whether the file and its
	// directory were both synced to disk.
	res, err := atomicfile.WriteFile(ctx, "/var/lib/myapp/state.json", []byte(`{"ok":true}`))
	if err != nil {
		log.Fatal(err)
	}
	if !res.Durable {
		log.Printf("%s is written but not yet durable", res.Path)
	}

	// A credential: staged owner-only, published at exactly 0600 or not at all.
	_, err = atomicfile.WriteFile(ctx, "/var/lib/myapp/token", []byte("secret"),
		atomicfile.WithMode(0o600))
	if err != nil {
		log.Fatal(err)
	}

	// Confined to one tree: a symlink or ".." in the name cannot leave it.
	root, err := os.OpenRoot("/var/lib/myapp")
	if err != nil {
		log.Fatal(err)
	}
	defer root.Close()
	_, err = atomicfile.WriteFileInRoot(ctx, root, "cache/index.json", []byte("[]"),
		atomicfile.WithMkdirMode(0o750))
	if err != nil {
		log.Fatal(err)
	}

	// Read it back under a 1 MiB cap.
	data, err := atomicfile.ReadBoundedInRoot(ctx, root, "cache/index.json", 1<<20)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("read %d bytes", len(data))
}
```

To stream content, use `WriteReader`. To write a file piece by piece, call `NewPendingFile`, write to it, then `Commit` to publish it or `Cleanup` to abort. The 23 runnable examples on [pkg.go.dev](https://pkg.go.dev/github.com/cplieger/atomicfile/v4#pkg-examples) cover writes, reads, sweeps, probes and private directories, and `go test` runs them.

## API

- Writes: `WriteFile`, `WriteReader`, `NewPendingFile` and their `*InRoot` forms.
- Reads: `ReadBounded`, `ReadBoundedFile`, `ReadBoundedInRoot`, `OpenRegular`, `OpenRegularInRoot`, `OpenRegularInRootNoFollow`.
- Shared directory trees: `WalkDirInRoot`, `OpenParentInRoot`, `RemoveFileInRoot`.
- Modes: `EnforceMode`, `EnsurePrivateDir`.
- Temp files and checks: `CleanupStaleTemps`, `CleanupStaleTempsInRoot`, `TempName`, `IsPackageTemp`, `ProbeWritable`, `ProbeWritableInRoot`, `ValidatePath`, `Identify`.
- Options: `WithMode`, `WithMkdirMode`, `WithMaxBytes`, `WithLogger`, `WithRecursive`, `WithRepairOwnedDir`.
- Errors: sentinel values for `errors.Is`, and `*WriteError` and `*NotRegularError` for `errors.As`.

The full reference is on [pkg.go.dev](https://pkg.go.dev/github.com/cplieger/atomicfile/v4). [Choosing a function](docs/choosing-a-function.md) says which call fits which job.

## What a nil error means

Every write creates a temp file in the target's directory, writes and syncs it, renames it over the target, then syncs the directory. After a crash the path holds the complete old content or the complete new content.

A nil error means the data reached its final path. `Result.Durable` then says whether that content will survive a power loss. It is false when the directory sync failed after the rename, for example with an `EIO` from a failing disk. A crash could then still bring back the old file. The package logs a warning in that case, and a caller that needs crash durability retries the write or raises an alert. A non-nil error means the data did not reach the path, and the previous file is untouched.

Two writers on one path each publish a whole file, and the last rename wins. The package takes no lock.

A process killed before the rename leaves its temp file behind. Nothing in the package removes it on its own, so call `CleanupStaleTemps` at startup or on a schedule, as [Orphaned temp files](docs/temp-files.md) explains.

[Durability and errors](docs/durability.md) has the full sequence and every error value.

## File modes

Without `WithMode`, atomicfile sets no mode. The file is created the way `os.Create` creates one, at 0666, narrowed by the umask and widened or narrowed by any ACL the directory passes on. A write that replaces an existing file gives the new file those defaults, and the old file's mode is not kept. That suits a shared tree whose permissions an operator manages, such as a media library.

Pass `WithMode` for any file whose readers must be restricted, such as credentials, configuration and backups. The payload is staged in a file proven owner-only, the final file is proven to hold the mode, and a filesystem that stores anything else fails the write with `ErrModeNotStored`.

`WithMkdirMode` creates missing parent directories at a mode it also proves. `EnsurePrivateDir` sets up one directory level that only the effective user may enter. [File modes and private directories](docs/file-modes.md) explains both.

## Unsupported by design

These are deliberate non-goals, not missing features.

- Windows. `os.Rename` cannot guarantee an atomic replace there ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)), and the package does not build for it.
- `fs.FS` interop. `fs.FS` is a read-only interface, and atomic writes are outside its scope.
- Atomic symlink replacement. Use [google/renameio](https://github.com/google/renameio) for that.
- Detecting a cross-mount temp directory. Temp files are always created in the target's own directory, on the same mount, which is the only correct place for an atomic rename.

## Documentation

- [Choosing a function](docs/choosing-a-function.md) maps each job to a call and lists every option.
- [Durability and errors](docs/durability.md) covers the write sequence, `Result` and every error value.
- [File modes and private directories](docs/file-modes.md) covers `WithMode`, `EnforceMode` and `EnsurePrivateDir`.
- [Confinement, symlinks and special files](docs/confinement.md) covers `*os.Root` use and shared trees.
- [Orphaned temp files and the writability probe](docs/temp-files.md) covers sweeps and `ProbeWritable`.
- [Detecting a changed file](docs/reload-staleness.md) covers `FileIdentity` for caches.

## Credits

The incremental `PendingFile` API, a staged file that is either committed into place or cleaned up, follows [google/renameio](https://github.com/google/renameio).

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the invariants and how to run the checks.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).
