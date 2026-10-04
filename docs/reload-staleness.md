# Detecting a changed file

This page explains `FileIdentity`, which tells a reader that caches a file whether its copy is still current, for a developer who reloads a file another process publishes.

## The check

A process that caches a file written by someone else needs to know, from a stat alone, whether its copy is still current. For a file published by rename, the test is an equal modification time and the same file by `os.SameFile`. That is knowledge about how this package publishes a file, which is why the check lives here.

```go
// after loading
id := atomicfile.Identify(info)

// on every poll
info, err := os.Stat(path)
if err != nil { /* caller's policy */ }
if id.Changed(info) {
	// reload, then call Identify again
}
```

## Why both checks are needed

Two different ways of writing a file change its content, and each one defeats half of a single check.

- An in-place writer, such as `os.WriteFile`, an editor or truncate-and-write, keeps the same inode and usually moves the modification time forward. Inode times come from a coarse clock tick, so two writes inside one tick carry identical times. A check on identity alone calls that unchanged.
- A writer that publishes by rename, as every write in this package does, installs a different inode. Its modification time normally differs too, but it need not. A backup restore, `rsync -t`, `tar -xp` or any republication of an archived version lands new content with the old timestamp. A check on time alone calls that unchanged, and the stale copy is served until something else touches the file.

## Size is a third check

`Matches` does not compare size, and size is not redundant. Time and size together catch a replacement that `Matches` misses, and `Matches` catches one that time and size miss.

| Second write | Time | Size | Inode | Time and size | `Matches` (time and `SameFile`) |
| --- | --- | --- | --- | --- | --- |
| rename, same tick, equal length | same | same | new | misses | catches |
| rename, same tick, length changed | same | differs | new | catches | catches |
| in-place, same tick, equal length | same | same | same | misses | misses |
| in-place, same tick, length changed | same | differs | same | catches | misses |
| any write crossing a tick | differs | any | any | catches | catches |

So time, `SameFile` and size together cover every row either pair covers. A reader of a file only this package publishes needs `Changed` alone. A reader whose file may also be rewritten in place keeps its own `info.Size() != cached` check beside `Changed`. That is one comparison at the call site, which is why the package exports no size check. No check based on a stat can catch the third row.

## The zero value

A zero `FileIdentity` records nothing and reports `Changed`. That is the safe direction for a cache, because a needless reload costs one read while a missed reload serves stale data indefinitely. `Recorded()` reports whether an identity was captured, and `ModTime()` returns the captured time for diagnostics.

`FileIdentity` compares, and decides nothing else. What to do when a stat fails, how to degrade and how often to stat again stay with the caller.
