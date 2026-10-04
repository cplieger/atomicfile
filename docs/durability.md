# Durability and errors

This page describes the steps every atomicfile write takes, what its `Result` reports and every error it can return, for a developer deciding how to handle each outcome.

## The write sequence

Every write, from `WriteFile` to `PendingFile.Commit`, follows the same steps:

1. Create a temp file in the target's own directory, on the same mount, so the rename is atomic.
2. Write the data.
3. Under `WithMode`, set the mode on the open file and read it back.
4. Sync the temp file.
5. Close it.
6. Rename it over the target.
7. Sync the parent directory.

After a crash the target holds the complete old content or the complete new content, never a partial write. The directory sync in step 7 makes the rename itself survive a power loss right after the call returns.

## What Result reports

```go
type Result struct {
	Path    string // the cleaned final path
	Durable bool   // true only when the file and every directory its path depends on were synced
}
```

`Path` is absolute for the package-level writes. For the `*InRoot` writes it is the root's directory joined with the cleaned relative name.

A nil error means the data reached its final path. The write either fully succeeded, or its rename completed while a directory sync failed, for example with an `EIO` from a failing disk. In that case the data is at the path, and the write logs the sync failure at `Warn`. `Result.Durable` tells the two outcomes apart, so a caller that cares about crash durability reads it rather than decoding error types.

A caller that needs strict durability treats `Durable == false` as a failure to act on, by retrying or alerting. A caller that only needs atomicity can ignore it. A non-nil error always means the data did not reach its final path.

`WithMkdirMode` is covered by the same guarantee. Each directory the write creates has its own parent synced as it is made. A directory entry that was never synced into its parent can vanish in a crash and take the whole subtree with it, including a file that was itself synced and renamed. A write that creates three directory levels therefore syncs four directories, and `Result.Durable` is false if any of those syncs failed.

## Concurrent writers

Each write stages its own temp file under a random name, created exclusively. Two writers on one path each publish a whole file, and the last rename wins. The package takes no lock, so a caller that must not lose an update coordinates its writers itself.

## Errors

Every error matches with `errors.Is` or `errors.As`.

| Error | Meaning |
| --- | --- |
| `ErrEmptyPath` | The path or name argument was empty |
| `ErrUnsafePath` | The path is not absolute, holds a NUL byte or names no entry (`.`, `..`), or a root-relative name is absolute. See [Confinement](confinement.md) |
| `ErrFileTooLarge` | A file exceeded the read limit, or content exceeded a `WithMaxBytes` cap. On a write it may arrive inside a `*WriteError` |
| `ErrSymlinkTarget` | The target of a write is a symlink, or `OpenRegular`, `OpenRegularInRootNoFollow` or `EnsurePrivateDir` refused one |
| `ErrRaced` | `OpenRegularInRootNoFollow` saw the name change identity between its symlink check and its open. A retry is the right response |
| `ErrNotRegular` | The name resolved to a directory, FIFO, device node or socket. Refused on read, on remove and on write |
| `ErrNotDirectory` | `EnsurePrivateDir` found a file, FIFO, device node or socket at the name |
| `ErrNotOwned` | `EnsurePrivateDir` found a directory not owned by the effective user, or could not tell |
| `ErrModeTooOpen` | `EnsurePrivateDir` found a pre-existing directory with group or other access, and no repair was asked for |
| `ErrModeNotStored` | `EnforceMode`, or a write under `WithMode` or `WithMkdirMode`, found the filesystem stored a different mode than asked |
| `ErrAborted` | `PendingFile.Commit` was called after `Cleanup` aborted the write |

`ErrNotRegular` arrives as a `*NotRegularError` carrying `Name` and `Mode`. Use `errors.As` to report what was there instead of parsing the message.

A failure inside the write itself is a `*WriteError{Err, Phase}`. `Phase` is one of `PhaseTempCreate`, `PhaseTempWrite`, `PhaseTempChmod`, `PhaseTempSync`, `PhaseTempClose` or `PhaseRename`. `PhaseTempCreate` covers opening the target's directory, so a missing parent without `WithMkdirMode` surfaces there, as well as creating the temp file. `PhaseTempChmod` occurs only under `WithMode`. A failed directory sync after the rename has no phase, because `Result.Durable` reports it.

Failures before the write starts keep their own types. Path and symlink failures use the values in the table. Context failures wrap `context.Canceled` or `context.DeadlineExceeded`. A `WithMkdirMode` directory creation failure wraps the underlying `os` error behind the `atomicfile:` prefix. A `*WriteError` always means the data did not reach its final path.
