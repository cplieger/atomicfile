# Orphaned temp files and the writability probe

This page explains when an atomicfile write leaves a temp file behind, how to reclaim it, and how to check that a directory takes writes. It is for a developer wiring startup checks and cleanup into a service.

## When a temp file is left behind

A temp file exists on disk between its creation and the rename that publishes it. Each write removes its own temp file on every failure path, but a deferred removal only runs if the process lives to run it.

SIGKILL does not let it run, and neither does Go's default handling of SIGINT and SIGTERM, which exits without unwinding. A power loss, an out-of-memory kill or a `docker stop` that escalates leaves a temp file behind. So does a `PendingFile` abandoned without `Commit` or `Cleanup`. Atomicity is unaffected, because nothing was published and the previous file at the target is untouched. An orphan costs disk space, not correctness.

## Reclaiming them

Nothing in the library reclaims orphans on its own. There is no background sweeper, no finalizer and no cleanup on the next write. Call `CleanupStaleTemps` or `CleanupStaleTempsInRoot` where you know a sweep is safe, such as at startup or on a schedule.

A finalizer would unlink with no ordering against a live `Commit`, so abandonment would work only sometimes. An `O_TMPFILE` publish cannot replace an existing entry, so the named temp file stays.

A sweep reclaims only files of the package's exact temp shape, `.atomicfile-<digits>.tmp`, older than `maxAge`. A file that only shares the prefix or suffix, such as `.atomicfile-notes.tmp` or `config.tmp-backup`, is never touched. Each candidate is checked again right before removal, so a fresh temp file created during the sweep is spared. A missing directory is not an error. The sweep stops once `ctx` is done and returns `ctx.Err()` with the counts so far.

The result is a `SweepResult` with three counts. `Removed` counts reclaimed files. `Failed` counts temp files that were found but could not be removed, so orphans are piling up. `Unreadable` counts subdirectories the sweep could not enter, which may hide orphans nobody counted.

A sweep covers one directory unless you pass `WithRecursive(true)`. A temp file sits in the same directory as its target, so a write to `out/example.com/cert.pfx` leaves its orphan in `out/example.com`, where a flat sweep of `out` does not look.

`CleanupStaleTemps` builds paths with `filepath.Join`, which is safe for a directory you own outright. For a directory another writer can modify underneath you, use `CleanupStaleTempsInRoot`, which [stays inside the tree](confinement.md#walking-a-shared-tree).

## Choosing maxAge

`maxAge` must be longer than any concurrent writer may hold a temp file, because a sweep cannot tell an orphan from a write in progress. POSIX offers no way to ask.

The sweep checks the temp file's modification time, not its creation time. A streaming write that keeps producing bytes keeps refreshing it and is safe at any duration. What is at risk is a temp file nothing has written to for `maxAge`.

The realistic case is a `PendingFile` that is staged, held across slow work, then committed. Measured with the temp file backdated past a one-hour `maxAge`, the sweep removed it and the following `Commit` failed with `*WriteError{PhaseRename}` wrapping `ENOENT`. Nothing was lost from the target, but that write was.

A `maxAge` of zero or less skips the sweep with a `Warn` instead of removing everything, so a zero from an unset config value cannot empty a directory.

## Naming your own temp files

`TempName()` returns a fresh name of the exact shape the sweeps reclaim, and `IsPackageTemp(name)` reports whether a directory entry's name has that shape. Use them instead of building `.atomicfile-<digits>.tmp` by hand. `os.CreateTemp(dir, ".atomicfile-*.tmp")` matches only because Go happens to put decimal digits in place of `*`, which its documentation does not promise.

## Probing a directory

`ProbeWritable(ctx, dir)` proves a directory takes writes by doing what a write does. It creates a temp file, writes and syncs one byte, closes the file and removes it, then reports which stage failed. `dir` may be relative, and `WithMkdirMode` creates it first. Under `WithMode` the probe must also prove an owner-only staging file, as a write would.

`ProbeWritableInRoot(ctx, root, name)` runs the same probe through an `*os.Root` you already hold, with `"."` for the root itself. A name that escapes the root is refused.

```go
type ProbeResult struct {
	Err    error      // the failure from Stage, unwrapped to the filesystem error
	Dir    string     // directory probed
	Name   string     // probe file's base name; always satisfies IsPackageTemp
	Stage  ProbeStage // first stage that failed, or ProbeStageNone
	Leaked bool       // probe file still on disk
}
```

Reading the mode bits does not answer the question. An NFS or FUSE mount, a read-only bind mount and a Docker volume owned by another UID all show a directory that looks writable and refuses the first write. The stages are `ProbeStageMkdir`, `ProbeStageCreate`, `ProbeStageWrite`, `ProbeStageSync`, `ProbeStageClose` and `ProbeStageRemove`, because a filesystem can fail at any one of them.

The policy stays with you. A failed stage is reported in the `ProbeResult`, never as the error return. A non-nil error means only that the probe could not start, for an empty `dir` or a cancelled context. `res.OK()` reports that every stage passed. `res.Writable()` is true when only teardown failed, at `ProbeStageClose` or later, because the directory still took the bytes. A caller can then warn and keep running.

The probe file is named with `TempName()`, so a probe file orphaned by a crash, or left by a directory that refuses the unlink, is reclaimed by `CleanupStaleTemps` like any other temp file.

`ctx` is checked once, before anything is created. Each stage is a single filesystem call the OS does not make interruptible, and a probe that has begun always runs its own cleanup. To guard against a mount that hangs, wrap the call in your own timeout.
