# Contributing to atomicfile

Notes on the durability model, public API, and the test suite that guards
them. The crash-safety guarantees are the whole point of the library, so
most of this guide is about not breaking them when you add or change code.

## What the library guarantees

`atomicfile` is a standard-library-only Go package for crash-safe file
writes. Every durable write follows the same six-step sequence, and that
ordering is the load-bearing invariant of the package:

1. create a temp file in the **target directory** (same mount, so the
   rename is atomic),
2. write the data,
3. `Chmod` to the resolved mode,
4. `fsync` the temp file,
5. `close` then `os.Rename` to the final path,
6. `fsync` the parent directory.

After a crash the final path holds either the complete old content or the
complete new content — never a partial write. Step 6 is what makes the
rename itself survive power loss; skipping it (via `WithNoSync()`) keeps
atomicity but drops durability.

When you touch any write path (`WriteFile`, `WriteReader`,
`NewPendingFile` / `Commit`, `WriteFileInRoot` / `WriteReaderInRoot`),
preserve this ordering. In particular:

- The temp file is always created in `filepath.Dir(cleanPath)`, never in
  `os.TempDir()` — a cross-mount rename is not atomic.
- A hard failure returns a `*WriteError` tagged with the `WritePhase` that
  failed (`PhaseTempCreate`, `PhaseTempWrite`, `PhaseRename`, ...), and a
  `*WriteError` always means the data did NOT reach its final path. Keep new
  hard-failure points tagged. The parent-directory fsync (step 6) is the
  exception: once the rename succeeds the data is present, so a failed dir
  fsync is **not** a `*WriteError` — it returns a nil error with
  `Result.Durable == false` and a `Warn` log. The "written, but durability
  not guaranteed" state is now a `Result` flag, not a phase. Likewise a
  preserve-ownership chown failure is best-effort — it logs at `Warn` and lets
  the write proceed, so it carries no phase and is not a `*WriteError`.
- On any error before the rename, the temp file must be cleaned up
  (`removeTemp`). Don't leave orphans.
- Paths are validated before any filesystem work: `validateAbsClean`
  (absolute, no `..` traversal, no null bytes) for the package-level
  writers, and `validateRootName` (relative, no null bytes; an internal
  `..` that stays inside the tree is allowed, since the `*os.Root` itself
  refuses any escape) for the `*os.Root`-confined writers.

The platform target is **Linux only** — `os.Rename` is not guaranteed
atomic on Windows. Don't add Windows-specific rename code; see the
"Unsupported by Design" table in `README.md` for the full list of
deliberate non-features.

## The `fsyncDir`, `fsyncRootDir`, and `osChown` test seams

The parent-directory fsync (step 6) is impossible to fail on a healthy
filesystem, so `fsyncDir` is a package-level `var` that tests reassign to
inject an `EIO`-style failure (see `dirsync_test.go`'s `stubFsyncDir`). Its
`*os.Root`-confined analogue is `fsyncRootDir` (the dir-fsync used by
`WriteFileInRoot` / `WriteReaderInRoot`), stubbed the same way via
`stubFsyncRootDir` in `helpers_test.go` and exercised by `writeroot_test.go`.

A best-effort `osChown` (used by `WithPreserveOwnership`) is impractical to
fail from a same-owner test, so it is likewise a package-level `var` that
tests reassign to inject a chown failure (see `stubOsChown` in
`helpers_test.go`).

These seams follow the same rules:

- Production code must never reassign `fsyncDir`, `fsyncRootDir`, or
  `osChown`.
- Tests that stub any of them mutate package state, so they must **not**
  call `t.Parallel()` and must restore the original via `t.Cleanup`.

## Local development

The module targets the Go version pinned in `go.mod` (the code uses
`errors.AsType`, a 1.26 generic). Use that toolchain or newer.

```sh
go build ./...
go test ./...
go test -race ./...
```

Run a single benchmark set with `go test -bench=. -benchmem .`.

### Linting and formatting

Lint config lives in `.golangci.yaml` (golangci-lint v2). It enables
`gosec`, `gocritic`, `revive`, `gocyclo`, `sloglint`
(kv-only), and others. Formatting is `gofumpt` with `extra-rules` plus
`gci` import grouping (standard → third-party); `golangci-lint run` reports
unformatted files as issues, so format before pushing.

```sh
golangci-lint run
golangci-lint fmt
```

### Fuzzing

The package ships several fuzz targets across `fuzz_test.go` and
`fuzz_extended_test.go`. Run one at a time with a time budget:

```sh
go test -run='^$' -fuzz=FuzzWriteFile -fuzztime=30s .
```

Available targets:

- `FuzzWriteFile`, `FuzzWriteReader`, `FuzzReadBounded`,
  `FuzzValidateAbsClean`, `FuzzValidateRootName` (`fuzz_test.go`)
- `FuzzIsStaleTempName`, `FuzzPendingFileRoundTrip`,
  `FuzzCleanupStaleTemps`, `FuzzWriteFileInRoot` (`fuzz_extended_test.go`)

New parsing or path-handling logic should come with a fuzz target or an
added seed corpus entry.

### Mutation testing

`.gremlins.yaml` configures [Gremlins](https://gremlins.dev) mutation
testing (synced fleet-wide from `cplieger/ci`). The weekly central runner
tracks the efficacy score; you can run it locally to check that new tests
actually kill mutants:

```sh
gremlins unleash .
```

## Test suite conventions

Tests live beside the code (standard Go layout) but split by intent — match
the right file when adding cases:

- `atomicfile_test.go` — core table-driven unit tests.
- `atomicfile_prop_test.go` — property tests via `pgregory.net/rapid` (the
  one external dependency, test-only).
- `fuzz_test.go`, `fuzz_extended_test.go` — fuzz targets (see above).
- `adversarial_test.go`, `redteam_refactor_test.go`, `round3_test.go` —
  failure-injection and edge-case hardening (erroring readers, temp-cleanup
  races, symlink refusal).
- `convergence_test.go`, `niloption_test.go` — guards for variadic
  `Option` handling, including `nil` options interleaved with real ones
  (`buildCfg` skips nils, and the suite enforces it).
- `dirsync_test.go` — parent-dir fsync durability (`Result.Durable`
  propagation) through every durable entry point via the `fsyncDir` seam.
- `writeroot_test.go` — the `*os.Root`-confined writers (`WriteFileInRoot` /
  `WriteReaderInRoot`): confinement (symlink / `..` escape refusal),
  preserve-mode/ownership, and dir-fsync durability via the `fsyncRootDir`
  seam.
- `read_boundedfile_test.go` — `ReadBoundedFile` on an already-open handle,
  including the read-through-an-`*os.Root` seam and the caller-owns-the-handle
  contract.
- `helpers_test.go` — shared test helpers (`isWindows`, `assertNoTempLeak`,
  `stubFsyncDir`, `stubOsChown`, `plainReader`, capture-handler logging,
  `seqCancelCtx`, ...).
- `example_test.go`, `readme_example_test.go` — runnable `Example`
  functions and the README snippet, kept compiling.
- `benchmark_test.go` — allocation/throughput benchmarks.

When you add an `Option` or a new entry point, extend the convergence and
dir-sync suites so the nil-option guard and `Result.Durable` propagation stay
covered for the new surface — and, for a new `*os.Root`-confined entry point,
add the matching confinement and `fsyncRootDir` cases to `writeroot_test.go`.

## Commits and PRs

Branch from `main`, keep changes focused with tests, and open a PR. This
account uses [Conventional Commits](https://www.conventionalcommits.org/)
parsed by git-cliff (`cliff.toml`) to build release notes, so the commit
type drives the version bump: `feat:`, `fix:`, `sec:`, and
`chore:`/`docs:`/`refactor:`/`test:` (no release). Write the subject as the
changelog line a consumer would read.

## Conduct & security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md) —
never in a public issue.
