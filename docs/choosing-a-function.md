# Choosing a function

This page maps each job to the atomicfile call that does it and lists every option, for a developer deciding which function to reach for. Signatures and full contracts are on [pkg.go.dev](https://pkg.go.dev/github.com/cplieger/atomicfile/v4).

## Which function to call

| Job | Call |
| --- | --- |
| Replace a file with bytes you hold | `WriteFile` |
| Replace a file from an `io.Reader` | `WriteReader` |
| Write a file piece by piece, then publish it | `NewPendingFile`, then `Commit` or `Cleanup` |
| Do any of the three inside one directory tree, so no name can leave it | `WriteFileInRoot`, `WriteReaderInRoot`, `NewPendingFileInRoot` |
| Read a whole file under a size cap | `ReadBounded`, or `ReadBoundedInRoot` inside a tree |
| Read a file you opened yourself, under a size cap | `ReadBoundedFile` |
| Open a regular file and keep its descriptor and `FileInfo` | `OpenRegularInRoot`, `OpenRegularInRootNoFollow` to refuse a symlink, `OpenRegular` for a full path |
| Walk a tree that other writers can modify | `WalkDirInRoot` |
| Remove a file in such a tree | `RemoveFileInRoot` |
| Act twice on one name and know it is the same file both times | `OpenParentInRoot` |
| Set a mode and prove the filesystem stored it | `EnforceMode` |
| Create a directory only your user can enter | `EnsurePrivateDir` |
| Check that a directory takes writes before relying on it | `ProbeWritable`, `ProbeWritableInRoot` |
| Reclaim temp files left by a crash | `CleanupStaleTemps`, `CleanupStaleTempsInRoot` |
| Name your own file so the sweeps reclaim it | `TempName`, `IsPackageTemp` |
| Tell whether a cached file changed on disk | `Identify`, then `FileIdentity.Changed` |
| Accept or refuse a configured path before writing to it | `ValidatePath` |

Every write returns `(Result, error)`. [Durability and errors](durability.md) explains both values.

## Streaming from a reader

`WriteReader` copies an `io.Reader` into the temp file. When the reader implements `io.WriterTo`, the copy uses it, so cancellation is checked per chunk or after a single copy. The context is still checked at the sync and rename, so a cancelled write leaves no partial target.

## Writing incrementally

`NewPendingFile` opens a temp file in the target's directory. `PendingFile` embeds `*os.File`, so `io.Writer`, `io.ReaderFrom` and `fmt.Fprintf` all work on it. `Name()` reports the temp's path, so another tool can inspect the staged file before it is published.

`Commit` runs the same sync and rename as `WriteFile`. Calling it again returns the first result. After `Cleanup` it returns `ErrAborted`. `Cleanup` closes and removes the temp, does nothing after `Commit`, and is safe to `defer` right after `NewPendingFile`.

A `WithMaxBytes` cap is enforced as you write. `BytesWritten()` reports the count, `Truncate` re-syncs it, and a call that would cross the cap is rejected whole. `Commit` checks the cap again against the staged file's real size. Bytes written outside that stream, through `WriteAt`, `Write` after `Seek` or a reopen of the temp by path, cannot publish an over-cap file either.

`NewPendingFileInRoot` runs the temp, the rename and the directory sync through your `*os.Root`, which you keep open until `Commit` or `Cleanup`. A `PendingFile` is not safe for concurrent use, so keep each one on one goroutine.

## Checking a path before writing

`ValidatePath` applies the path rule that `WriteFile`, `WriteReader`, `NewPendingFile` and `ReadBounded` apply. Use it to accept or refuse a path before the first write, such as a config value read at startup, a CLI flag or a request field.

It returns nil for an acceptable path. Otherwise it returns the error the write would return, `ErrEmptyPath`, or `ErrUnsafePath` for an embedded NUL byte or a path that is not absolute once cleaned. It calls the same validator the writes use, so its answer and a later write's answer cannot disagree.

`filepath.IsAbs` is a different rule. It accepts `"/tmp/a\x00b"`, which every write here refuses. `ValidatePath` reads nothing on the filesystem, so the path need not exist and nil does not promise a write will succeed. [`ProbeWritable`](temp-files.md#probing-a-directory) answers that by writing. An absolute path is not confined to any tree either, as [Confinement](confinement.md#the-path-check-is-not-containment) explains.

## Options

Every write, sweep and probe accepts options. Each call ignores the options that do not apply to it.

| Option | Applies to | Effect |
| --- | --- | --- |
| `WithMode(mode)` | writes, probes | Stages the file proven owner-only and proves the final file holds `mode`, or fails with `ErrModeNotStored`. The last call wins |
| `WithMkdirMode(mode)` | writes, probes | Creates missing parent directories at `mode`, proven on each, and syncs each one's parent. Without it a missing parent is an error |
| `WithMaxBytes(n)` | writes | Caps staged content at `n` bytes, checked while staging and before the rename. Over-cap writes match `ErrFileTooLarge`. `n <= 0` means no cap |
| `WithLogger(l)` | every call that logs | The `*slog.Logger` for diagnostics. Default `slog.Default()` |
| `WithRecursive(recursive)` | stale-temp sweeps | `true` descends into subdirectories. The default sweeps one directory |
| `WithRepairOwnedDir(repair)` | `EnsurePrivateDir` | `true` repairs a too-open pre-existing directory the effective user owns, instead of refusing it |

Without `WithMode`, the file is created like `os.Create`, at 0666 through the umask and inherited ACLs, and its mode is never checked. [File modes and private directories](file-modes.md) explains when to pass it.
