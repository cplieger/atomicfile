# Contributing to atomicfile

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- A new write entry point joins `writeEntryPoints()` in `writeintent_test.go`, `TestOptions_NilElement` in `options_test.go` and, for an absolute-path writer, `TestSymlinkTarget`, `TestNullByte_AllEntryPoints` and `TestEmptyPath_AllEntryPoints` in `path_test.go`. Each checks only the writers it lists, so a writer left out goes untested.
- A new write entry point also gets a directory-sync failure case through `stubFsyncRootDir`, in `dirsync_test.go` or, for an `*os.Root` writer, `writeroot_test.go`. Without it, nothing catches a writer that errors or drops `Result.Durable` when the sync after the rename fails.
- A new exported function or `Option` gets a line in the `## API` list of `README.md` and a row in `docs/choosing-a-function.md`. Readers pick a call from those two pages, and no check compares them with the code.
- A change to path validation, temp-name matching or a bounded read comes with a fuzz target in `atomicfile_fuzz_test.go` or a seed for an existing one. `go test` replays every seed, so an input found once stays covered.
