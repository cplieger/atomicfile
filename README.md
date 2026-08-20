# atomicfile

[![Go Reference](https://pkg.go.dev/badge/github.com/cplieger/atomicfile/v3.svg)](https://pkg.go.dev/github.com/cplieger/atomicfile/v3)
[![Go version](https://img.shields.io/github/go-mod/go-version/cplieger/atomicfile)](https://github.com/cplieger/atomicfile/blob/main/go.mod)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/coverage.json)](https://github.com/cplieger/atomicfile/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/atomicfile/badges/mutation.json)](https://github.com/cplieger/atomicfile/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13198/badge)](https://www.bestpractices.dev/projects/13198)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/atomicfile/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/atomicfile)

> Crash-safe atomic file writes for Go

A standalone Go library providing atomic file writes (temp→fsync→rename→dir-fsync), path cleaning with `os.Root`-based containment, bounded reads, and streaming writes. Standard-library only, no external runtime dependencies.

Every write is `os.Root`-confined: the `*InRoot` APIs use the caller's root directly, and the absolute-path APIs open the target's parent directory as a root and write through it.

## Platform Support

**Linux only (including Docker/containers).** Windows is unsupported by design: `os.Rename` cannot guarantee atomicity on Windows ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). macOS/BSD may work but is untested.

## Install

`go get github.com/cplieger/atomicfile/v3@latest`

## Usage

```go
package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/cplieger/atomicfile/v3"
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

	// Auto-create parent directories.
	atomicfile.WriteFile(ctx, "/tmp/nested/dir/file.txt", []byte("deep"),
		atomicfile.WithMkdirMode(0o755))

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

- `WriteFile(ctx, path, data, opts ...Option) (Result, error)`: atomic write (default mode 0644)
- `WriteReader(ctx, path, r, opts ...Option) (Result, error)`: atomic write from `io.Reader` (uses the `io.WriterTo` fast path when available; mode via `WithMode`)
- `WriteFileInRoot(ctx, root, name, data, opts ...Option) (Result, error)`: atomic write of `data` to `name` relative to an `*os.Root`; every filesystem op runs through the root, so a symlink or `..` in `name` cannot escape its tree
- `WriteReaderInRoot(ctx, root, name, r, opts ...Option) (Result, error)`: same, streaming from an `io.Reader`

### Streaming Writer

- `NewPendingFile(ctx, path, opts ...Option) (*PendingFile, error)`: open a temp file for incremental writing (mode via `WithMode`)
- `NewPendingFileInRoot(ctx, root, name, opts ...Option) (*PendingFile, error)`: same, confined to an `*os.Root`: the temp, rename, and parent-dir fsync all run through the caller's root, which stays caller-owned (keep it open through Commit/Cleanup)
- `(*PendingFile).Commit(ctx) (Result, error)`: chmod + fsync + close + rename + dir-fsync (finalize). Idempotent: repeated calls return the first result. Returns `ErrAborted` if called after `Cleanup`.
- `(*PendingFile).Cleanup() error`: close + remove (abort; no-op after Commit, idempotent). Safe to `defer` immediately after `NewPendingFile`.

`PendingFile` embeds `*os.File`, so the full `io.Writer`/`io.ReaderFrom`/`fmt.Fprintf` surface is available, and `Name()` reports the staged temp's path for inspection before `Commit` publishes it. A `WithMaxBytes` cap is enforced as you write (`BytesWritten()` reports the count, `Truncate` re-syncs it, and a call that would cross the cap is rejected whole) and re-verified against the staged file's actual size at `Commit`, so bytes staged outside the tracked stream (`WriteAt`, `Write` after `Seek`, a reopen of the temp by path) cannot publish an over-cap file either.

### Read Functions

- `ReadBounded(ctx, path, maxBytes) ([]byte, error)`: size-checked read; returns `ErrFileTooLarge` past the limit
- `ReadBoundedFile(ctx, f, maxBytes) ([]byte, error)`: size-checked read from an already-open `*os.File` (the seam for reading a file opened through an `*os.Root`); the caller owns and closes `f`
- `OpenRegularInRoot(root, name) (*os.File, os.FileInfo, error)`: the open half of `ReadBoundedInRoot`, which is now literally this plus `ReadBoundedFile`. Opens `name` through the root read-only and non-blocking, stats the OPEN HANDLE rather than the pathname, and refuses a directory, FIFO, device node or socket with `ErrNotRegular`. The caller owns and closes the descriptor.
- `OpenRegular(path) (*os.File, os.FileInfo, error)`: the ambient-path sibling, for a full path into a directory the caller trusts. `O_NOFOLLOW` makes the **kernel** refuse a symlink under the name (`ErrSymlinkTarget`), which no check-then-open sequence can do without a race; the path itself goes through the `ValidatePath` rule. This does not change `ReadBounded`, which still follows symlinks by design.

Both openers exist because the **descriptor** is what a consumer cannot rebuild from bytes: a caller streaming the file through a decoder or decryptor never materializes it, and a caller caching the file needs the `FileInfo` the bytes came from to record a [`FileIdentity`](#reload-staleness). Binding the shape check, the identity and the read to ONE descriptor is the point — a second stat of the pathname lets a rename in between decode one generation's bytes while recording another's identity, and turns every non-regular-file rejection back into a check-then-open race. Neither opener pins the _ancestor_ components; `OpenParentInRoot` does that.

### Confined Traversal and Removal

An `*os.Root` confines a path but does not _pin_ it: a root deliberately follows a symlink component that stays inside its tree, so a multi-component name that is stat-ed and then operated on can address two different files if an ancestor directory is swapped for a symlink in between. Confinement still holds (nothing outside the root is reachable) and the operation lands somewhere inside the tree the caller never inspected. These two close that gap for callers working in a tree others can write to — a co-mounted Docker volume, a shared NFS export.

- `WalkDirInRoot(ctx, root, fn fs.WalkDirFunc) error`: stream a confined tree. Each directory is read in fixed 256-entry `ReadDir` batches instead of one materialized (and sorted) inventory, exactly ONE directory handle is open at a time (what the walk carries between directories is a queue of names), each directory is opened `O_DIRECTORY` so a named pipe swapped in for a directory is refused with `ENOTDIR` rather than blocking the caller's goroutine in `open(2)`, and a symlinked directory is never descended. The callback is an `fs.WalkDirFunc`, called exactly as `fs.WalkDir` calls one — the root first as `"."`, then each entry pre-order, a directory that cannot be opened or finished reported through the callback for its own path, `fs.SkipDir`/`fs.SkipAll` honoured. Two deliberate differences: entries arrive in **directory order** (sorted within a batch only, because a global sort is the materialization this avoids), and a directory's own entries are all visited before the walk descends into its subdirectories. The walk stops between batches once `ctx` is done; a caller wanting per-entry cancellation checks `ctx` in the callback. Walk a subtree by passing `root.OpenRoot(sub)`.
- `OpenParentInRoot(root, name) (parent *os.Root, base string, err error)`: open `name`'s parent directory as its own root, pinned. Every component is `Lstat`-ed (a symlink is refused, not followed), required to be a real directory, opened as a root, and confirmed with `os.SameFile` against the directory that was inspected, so a component replaced mid-open is refused too. Naming only `base` through `parent` afterwards removes every ancestor from the operation's path. The returned root is **always** a fresh handle the caller closes — including for a flat `name`, where it is a second handle on the root's own directory — so the close is unconditional and never closes the caller's root. A vanished component matches `fs.ErrNotExist` (the benign racing-deletion case), a refused one does not.
- `RemoveFileInRoot(root, name) error`: that descent applied to an unlink. Only a regular file is removed; a directory, symlink, device node, socket or FIFO wearing the name is refused with `ErrNotRegular` and left as found. A missing file is reported (`fs.ErrNotExist`) rather than swallowed — whether an already-gone name is success is the caller's judgement.

`CleanupStaleTempsInRoot` uses both: it enumerates through `WalkDirInRoot` (so a large or hostile directory is not materialized before the sweep sees its first entry) and unlinks every candidate through a pinned parent.

### Private Directories and Enforced Modes

- `EnforceMode(f *os.File, want os.FileMode) (os.FileMode, error)`: `fchmod` the **open handle** to `want`, then `fstat` the same handle and return the mode the filesystem actually **stored**; a mismatch is `ErrModeNotStored`, naming both modes. Works for a file or a directory, since both are an `*os.File`.
- `EnsurePrivateDir(dir string, opts ...Option) (PrivateDir, error)`: establish custody of **one** directory level only the effective uid may enter — `mkdir` 0700, open `O_DIRECTORY|O_NOFOLLOW`, `fstat` the handle, require the owner uid, repair the mode if we created it, refuse it if we did not.

```go
type PrivateDir struct {
	Mode     os.FileMode // the mode the filesystem stored, read back from the handle
	Created  bool        // this call created the directory
	Repaired bool        // the created directory's mode had to be corrected
}
```

A mode argument to `mkdir(2)` or `open(2)` is a **request**, not a result. `umask` narrows it, and a filesystem carrying an inheritable ACL can widen the outcome regardless of what was asked — measured on a ZFS `nfs4acl` dataset, an inheritable `group@:rwx` ACE stores **0770** for a `0o700` mkdir, and a child of an already-0700 parent comes back 0770 too, so tightening the parent does not cover it. `chmod(2)` is the only call that _sets_ a mode; a caller that stops there has issued a second request rather than confirmed a result. `EnforceMode` is the chmod **and** the re-stat, on one descriptor, because a chmod-then-stat by name can chmod one object and certify another if the name is swapped in between.

`EnsurePrivateDir` composes that into the sequence a process needs when its private directory lives inside a parent other users can write (a state directory under `/tmp`, an admin-socket directory):

- **`mkdir` 0700, remembering whether _this_ call created the level.** `os.MkdirAll` cannot substitute: it stats the path, _follows_ a symlink, finds a directory and returns nil, so it cannot tell "I created this" from "something was already here" — the distinction every decision below turns on.
- **Open `O_DIRECTORY|O_NOFOLLOW|O_RDONLY`.** The kernel refuses a planted symlink instead of following it (`ErrSymlinkTarget`), refuses anything that is not a directory (`ErrNotDirectory`), and cannot be stalled indefinitely by a planted FIFO.
- **`fstat` the handle, then require `os.Geteuid()` to own it** (`ErrNotOwned`). Ownership, not just mode: a perfectly-moded 0700 directory owned by another uid passes every other check and its owner can still replace it — with a symlink the caller then follows — _after_ the verdict returns.
- **If this call created it, `EnforceMode` to 0700.** That is the ACL repair, and it is safe precisely because we made the directory: no other writer has ever held that name. A **pre-existing** directory is refused instead, unless the caller passes `WithRepairOwnedDir(true)` — which repairs it, because the euid-ownership check has already proved the directory is the caller's own (see the option table). That option exists so a mode fix does not become an outage the first time an app meets a directory an earlier release of itself created.
- **If it pre-existed, it is never chmod'ed** and is refused when any group or other bit is set (`ErrModeTooOpen`). Repairing a directory another principal made would take over their name and hand them whatever gets written under it.

**One level per call, deliberately.** `dir`'s parent is not created, inspected, or vouched for; a multi-level path is a loop over the levels, outermost first, because only the caller knows which levels are its own.

> **The verdict is point-in-time.** It proves what was true of one inode while a
> handle was open on it. It does **not** pin the directory for the caller's later
> use: the handle is closed before the call returns, so a subsequent `ReadDir`,
> `Remove`, `Open` or `Mkdir` through the **pathname** re-resolves every component
> and re-opens the window the check closed. What would close that gap is acting
> through a retained handle rather than a name — the `openat(2)`-relative
> operations of an `*os.Root` — which is a different API shape: hold the directory
> open for its whole lifetime and name only leaves inside it.

Of the functional options, `WithRepairOwnedDir(true)` opts into repairing a too-open directory this same identity already owns, and `WithLogger` supplies the logger for the one `Warn` a mode repair emits (a filesystem that ignores mode requests is news for every other `mkdir` in the program too). The mode is not a parameter: a "private directory" with a caller-chosen mode is a different primitive.

### Utilities

- `CleanupStaleTemps(dir, maxAge, opts ...Option) (removed int, err error)`: remove orphaned temp files left by interrupted writes, returning how many were removed. Only files matching the package's exact temp shape (`.atomicfile-<digits>.tmp`) and older than `maxAge` are reclaimed; caller-owned files that merely share the prefix or suffix (`.atomicfile-notes.tmp`, `config.tmp-backup`) are never touched. Each candidate is re-checked immediately before removal, so a same-named fresh temp created during the scan is spared. See [Orphaned temps](#orphaned-temps) for what leaves one behind and how to pick `maxAge`.
- `Identify(info os.FileInfo) FileIdentity` + `Matches` / `Changed` / `Recorded` / `ModTime`: reload-staleness comparison for a reader caching a file another process publishes. See [Reload staleness](#reload-staleness).
- `TempName() string` and `IsPackageTemp(name string) bool`: the temp-name convention as a contract. `TempName` returns a fresh name of the exact shape the sweeps reclaim; `IsPackageTemp` reports whether a directory entry's name is one. Use them instead of rebuilding `.atomicfile-<digits>.tmp` by hand — `os.CreateTemp(dir, ".atomicfile-*.tmp")` only matches because Go happens to substitute decimal digits for `*`, which its documentation does not promise.
- `ValidatePath(path string) error`: the path gate the absolute-path functions apply (`WriteFile`, `WriteReader`, `NewPendingFile`, `ReadBounded`), for a caller that has to accept or refuse a path before it writes — a config value read at startup, a CLI flag, a request field. `nil` means acceptable; otherwise it returns the same error the write would: `ErrEmptyPath`, or `ErrUnsafePath` for an embedded NUL or a path that is not absolute once cleaned. It delegates to the one private validator the writes use, so an acceptance here and a rejection at write time cannot disagree. `filepath.IsAbs` is the usual stand-in and is a different rule: it accepts `"/tmp/a\x00b"`, which every write here refuses. Nothing on the filesystem is consulted — the path need not exist and its parent need not be a directory — so `nil` says the path is well formed, not that a write there will succeed ([`ProbeWritable`](#writability-probe) answers that, by writing). Being absolute is not containment: `filepath.Clean` normalizes `..`, so use the `*os.Root`-backed APIs to confine writes to a tree.

### Writability Probe

- `ProbeWritable(ctx, dir, opts ...Option) (ProbeResult, error)`: prove a directory is genuinely writable by doing what a write does — create a temp file, write and flush a byte, close it, unlink it — and report which stage failed. `dir` may be relative; `WithMkdirMode` creates it first.
- `ProbeWritableInRoot(ctx, root, name, opts ...Option) (ProbeResult, error)`: the same probe confined to an `*os.Root` (`name` is relative to it, `"."` for the root itself), for a caller that already holds a root over the directory it is about to write to. An escaping name is refused by the root.

```go
type ProbeResult struct {
	Err    error      // the failure from Stage, unwrapped to the filesystem error
	Dir    string     // directory probed
	Name   string     // probe file's base name; always satisfies IsPackageTemp
	Stage  ProbeStage // first stage that failed, or ProbeStageNone
	Leaked bool       // probe file still on disk
}
```

A stat of the mode bits is not an answer: an NFS or FUSE mount, a read-only bind mount, and a Docker volume owned by another UID all present a writable-looking directory that refuses the first write. The stages are the whole ladder (`ProbeStageMkdir`, `Create`, `Write`, `Sync`, `Close`, `Remove`), because a filesystem can accept the directory entry yet reject the first data write, surface a delayed failure only at sync or close, or accept everything and deny the unlink.

**Policy stays with the caller.** A stage failure is reported in the `ProbeResult`, never as the error return — `err != nil` means only that the probe could not be attempted (empty `dir`, cancelled context). `res.OK()` is the everything-passed answer; `res.Writable()` is the split for a caller that wants to warn about a failed cleanup and keep running, since a failure at `ProbeStageClose` or later still proves the directory took the bytes.

The probe file is named with `TempName()`, so a probe orphaned by a crash — or left behind by a directory that denies the unlink — is reclaimed by `CleanupStaleTemps` like any other temp, by construction rather than by a naming convention each caller reproduces.

`ctx` is checked once, before anything is created; the stages are single filesystem calls the OS does not make interruptible, and a probe that has begun always runs its own cleanup, so guard a possibly-wedged mount with your own timeout around the call.

## Reload staleness

A process that caches a file written by someone else needs to know, from a stat alone, whether its in-memory copy is still current. The correct test is **mtime equality AND `os.SameFile` identity**, and it is knowledge about this package's write barrier — which is why the primitive lives here:

```go
// after loading
id := atomicfile.Identify(info)

// on every poll
info, err := os.Stat(path)
if err != nil { /* caller's policy */ }
if id.Changed(info) {
	// reload, then re-Identify
}
```

Both legs are load-bearing, because two different write mechanisms change a file's content and each defeats one half of the naive check:

- An **in-place** writer (`os.WriteFile`, an editor, truncate-and-write) keeps the same inode and moves the mtime forward. An identity-only check calls that unchanged.
- A **publish-by-rename** writer — every write in this package — installs a _different_ inode. Normally its mtime differs too, but it need not: a backup restore, `rsync -t`, `tar -xp`, or any re-publication of an archived generation lands new content carrying the old timestamp. An mtime-only check calls that unchanged, and the stale copy is then served until something else happens to touch the file.

Size is deliberately not part of the comparison: a same-size replacement is exactly the case a size check misses, and a differing size is already caught by one of the two legs.

The zero `FileIdentity` records nothing and reports `Changed`, which is the fail direction a cache wants — a spurious reload costs one read, a missed reload serves stale data indefinitely. `FileIdentity` is a comparison primitive, not a policy: degradation states, stat-error handling, and re-stat cadence stay with the caller.

## Orphaned temps

A temp exists on disk between its creation and the rename that publishes it. Each write removes its own temp on every failure path, but a deferred removal only runs if the process lives to run it: **SIGKILL does not, and neither does Go's default SIGINT/SIGTERM handling**, which exits without unwinding. A power loss, an OOM kill, or a `docker stop` that escalates leaves a temp behind, and so does a `PendingFile` abandoned without `Commit` or `Cleanup`. Atomicity is unaffected — nothing was published and the previous file at the target is untouched — so an orphan costs disk, not correctness.

**Nothing in this library reclaims them on its own.** There is no background sweeper, no finalizer, and no cleanup on the next write. Reclamation is `CleanupStaleTemps` / `CleanupStaleTempsInRoot`, called by the consumer where it knows a sweep is safe: at startup, or on a schedule. A finalizer was considered and rejected — an unlink with no ordering guarantee against a live `Commit` would make abandonment silently work *sometimes*, which is worse than deterministically leaving a file a shape-matched sweep can identify. Publishing by `linkat` over `O_TMPFILE` would leave no name to orphan at all, and does not fit: `linkat` cannot replace an existing entry, so overwriting a previous generation still needs a named temp and a rename.

**Picking `maxAge`.** It must exceed the longest time any concurrent writer may hold a temp: a sweep cannot tell an orphan from a write in progress, because POSIX offers no way to ask. The gate is the temp's **mtime**, not its creation time, so a streaming write that keeps producing bytes keeps refreshing it and is safe at any duration; what is at risk is a temp nothing has written to for `maxAge`. The realistic case is a `PendingFile` staged, then held across slow work, then committed — measured, with the temp backdated past a one-hour `maxAge` the sweep unlinked it and the following `Commit` failed with `*WriteError{PhaseRename}` wrapping `ENOENT`. Nothing was lost from the target; that write was. A non-positive `maxAge` skips the sweep with a `Warn` instead of reaping everything, so a zero from an unset config cannot empty a directory.

## Result and Durability

```go
type Result struct {
	Path    string // cleaned final path (absolute for the package-level writers; root-relative for WriteFileInRoot/WriteReaderInRoot)
	Durable bool   // true only when the file and every directory its path depends on were fsynced
}
```

Every atomic write follows the sequence: create temp file → write data → fsync temp → close → rename to final path → fsync parent directory. After a crash the file contains either the complete old content or the complete new content, never a partial write. The directory fsync makes the rename durable even if the system loses power immediately after the call returns.

A nil error means the data reached its final path: the write either fully succeeded or, at worst, was renamed into place but a directory fsync failed (for example an `EIO` from a failing disk; the rename has already completed, and the write logs the fsync failure at `Warn`). `Result.Durable` distinguishes those two outcomes, so a caller that cares about crash durability inspects `Result.Durable` rather than decoding error types. A non-nil error always means the data did **not** reach its final path.

Callers that require strict durability treat `Durable == false` as actionable (retry or alert); callers that only need atomicity can ignore it.

`WithMkdirMode` is covered by the same guarantee. Each directory the write creates has its own parent fsynced as it is made, because a directory entry that was never fsynced into its parent can vanish in a crash and take the whole subtree with it — including a file that was itself fsynced and renamed. A three-level `WithMkdirMode` write therefore fsyncs four directories, and `Result.Durable` is false if any of them failed.

## Functional Options

All write functions accept variadic `Option` values. Omit options for defaults.

| Option                       | Description                                                                                                                                                                                                                     |
|------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `WithLogger(l)`              | Custom `*slog.Logger` for diagnostic output (default: `slog.Default()`)                                                                                                                                                         |
| `WithMode(mode)`             | File permission (default: `0o644`)                                                                                                                                                                                              |
| `WithMkdirMode(mode)`        | Create the parent directory (and missing ancestors) with this mode before writing. Without it, a missing parent is an error. The mode is enforced on each directory created (a request through `mkdir(2)` is narrowed by umask and can be widened by an ACL) and each one's parent is fsynced.                                                    |
| `WithRepairOwnedDir(repair)` | `EnsurePrivateDir` only: `true` repairs a **pre-existing** directory whose owner is already the effective uid, instead of refusing it; `false` keeps the default refusal. For adopting your own past output; ignored by writes. |
| `WithRecursive(recursive)`   | Stale-temp sweeps only: `true` makes `CleanupStaleTemps` / `CleanupStaleTempsInRoot` descend into subdirectories; `false` keeps the default one-directory sweep. Ignored by writes.                                             |
| `WithMaxBytes(n)`            | Cap staged content at `n` bytes, the write-side mirror of `ReadBounded`. Over-cap writes match `ErrFileTooLarge` and leave the previous target intact. `n <= 0` = no cap.                                                       |

## Errors

| Sentinel           | Meaning                                                                                        |
|--------------------|------------------------------------------------------------------------------------------------|
| `ErrEmptyPath`     | The path argument was empty                                                                    |
| `ErrUnsafePath`    | The path is not absolute, contains a null byte, or names no entry (`.`, `..`)                   |
| `ErrFileTooLarge`  | The file exceeded the `ReadBounded` size limit, or content exceeded a `WithMaxBytes` write cap |
| `ErrSymlinkTarget` | The target of a write is a symlink (always refused); or `OpenRegular` refused one              |
| `ErrNotRegular`    | Name resolved to a non-regular file (dir, FIFO, device, socket); the mode is named. Refused on read, unlink **and write** |
| `ErrNotDirectory`  | `EnsurePrivateDir`: the name is occupied by a file, FIFO, device node or socket                |
| `ErrNotOwned`      | `EnsurePrivateDir`: the directory's owner is not the effective uid (or could not be determined)|
| `ErrModeTooOpen`   | `EnsurePrivateDir`: a **pre-existing** dir grants group/other access, no repair opted in       |
| `ErrModeNotStored` | `EnforceMode`: the mode read back after the chmod is not the mode that was asked for           |
| `ErrAborted`       | `PendingFile.Commit` was called after `Cleanup` aborted the pending write                      |

The package-level path check is not a containment boundary. `filepath.Clean` normalizes any `..` in an absolute path rather than rejecting it (for an absolute path there is nothing to escape), so `ErrUnsafePath` only guards against a non-absolute or null-byte path. Callers that need to confine writes to a directory tree use the `*os.Root`-backed write APIs (`WriteFileInRoot` / `WriteReaderInRoot`). Callers that need to confine reads should open the file through an `*os.Root` and pass that already-confined handle to `ReadBoundedFile`, which then applies the same size and context bounds.

Failures in the write barrier (open destination / create temp, write, chmod, sync, close, rename) are reported as `*WriteError{Err, Phase}`, where `Phase` is one of `PhaseTempCreate`, `PhaseTempWrite`, `PhaseTempChmod`, `PhaseTempSync`, `PhaseTempClose`, or `PhaseRename`. `PhaseTempCreate` covers opening the destination for writing (a missing parent without `WithMkdirMode` surfaces here) as well as creating the temp file inside it. Pre-barrier failures keep their own error types: path-validation and symlink failures use the sentinels above, context failures wrap the standard-library context error (`context.Canceled` / `context.DeadlineExceeded`), and a `WithMkdirMode` parent-directory creation failure wraps the underlying os error behind the `atomicfile:` prefix. All are inspectable with `errors.Is` / `errors.As`, and a `*WriteError` always means the data did **not** reach its final path.

## Symlink Safety

All write functions refuse to write to a path that is currently a symlink, returning `ErrSymlinkTarget` — unconditionally; there is no opt-out. `os.Rename` replaces the symlink itself rather than the file it points to, which is rarely the caller's intent and can lead to data loss or security issues. Writing through a symlink is not supported; if the link's target is a path you already trust, resolve it with `filepath.EvalSymlinks` and write to the resolved path.

They also refuse a target already occupied by anything else that is not a **regular file**, returning `ErrNotRegular` — the same verdict `ReadBoundedInRoot` gives what it will read and `RemoveFileInRoot` gives what it will unlink. `rename(2)` overwrites any non-directory, so without the refusal a write whose target was a FIFO, socket or device node succeeded and reported `Durable`, silently destroying an object this package never created; a directory target failed only at `PhaseRename`, after a complete staged write and both fsyncs. An existing regular file is still overwritten, which is the point of the package. The check is a check-then-act and cannot be otherwise — `rename(2)` has no "only if the destination is a regular file" mode — so its value is refusing the states that are there to be found, not winning a race, and the race it can lose is bounded by rename never following a final-component symlink.

Reads behave differently: `ReadBounded` follows symlinks by design (`os.Open` resolves them), so it does NOT refuse a symlink at `path`. When reading from a directory writable by a less-trusted principal, confine the path yourself: open the file through an `*os.Root` (Go 1.24+) and read it with `ReadBoundedFile`, which applies the same size and context bounds without following symlinks out of the root. `OpenRegularInRoot` is that open, already written; `OpenRegular` is the ambient-path form for a caller holding a full path into a directory it trusts, and it is the one read entry point that does refuse a symlink under the name — `O_NOFOLLOW` has the kernel refuse the open, which a check-then-open sequence cannot do without a race.

An `*os.Root` is not the whole answer for a _multi-component_ name, because it follows a symlink component that stays inside its tree: two operations on one such path can land on two different files if an ancestor directory is swapped in between. `OpenParentInRoot` and `RemoveFileInRoot` pin the ancestors — see [Confined Traversal and Removal](#confined-traversal-and-removal).

## Unsupported by Design

| Feature                             | Rationale                                                                                                                                                                                                                 |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Windows rename-over semantics**   | Target platform is Linux. `os.Rename` is atomic on Linux. Windows cannot guarantee atomicity ([golang/go#22397](https://github.com/golang/go/issues/22397#issuecomment-498856679)). google/renameio also refuses Windows. |
| **`fs.FS` interop**                 | `fs.FS` is a read-only interface. Atomic writes are inherently outside its scope.                                                                                                                                         |
| **Atomic symlink replacement**      | Out of scope. Use google/renameio if needed.                                                                                                                                                                              |
| **Umask-aware permissions**         | The library uses `Chmod` for exact permissions (ignoring umask). This is the correct secure default for server/CLI tools. Equivalent to renameio's `WithStaticPermissions`.                                               |
| **`TempDir` cross-mount detection** | Temp files are always created in the target directory (same mount point), the only correct approach for atomic rename.                                                                                                    |

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).
