# atomicfile
> Crash-safe atomic file writes for Go

A standalone Go library providing atomic file writes (temp→fsync→rename→dir-fsync), path-traversal validation, bounded reads, and JSON save helpers. Standard-library only, no external dependencies.

## Install
<!-- TODO: registry/pull link -->
`go get github.com/cplieger/atomicfile@latest`

## Usage
```go
package main

import (
	"context"
	"github.com/cplieger/atomicfile"
)

func main() {
	ctx := context.Background()

	// Atomic write with default options
	atomicfile.WriteFile(ctx, "/tmp/data.txt", []byte("hello"))

	// With custom temp-file prefix
	opts := &atomicfile.Options{TempPattern: ".myapp-*.tmp"}
	atomicfile.WriteFileMode(ctx, "/tmp/data.txt", []byte("hello"), 0o600, opts)

	// Bounded read (rejects files over limit)
	data, _ := atomicfile.ReadBounded(ctx, "/tmp/data.txt", 1<<20)
	_ = data
}
```

## API
- `WriteFile(ctx, path, data)` — atomic write, mode 0644
- `WriteFileMode(ctx, path, data, mode, opts)` — atomic write with mode and options
- `Prepare(ctx, path, data, opts)` — create temp file ready for commit
- `Commit(tmpPath, finalPath)` — rename temp to final + dir fsync
- `SaveBytes(path, data, perm, opts)` — atomic write with auto-mkdir
- `SaveJSON(path, mu, v, label, perm, opts)` — marshal + atomic write
- `ReadBounded(ctx, path, maxBytes)` — size-checked read
- `CleanupStaleTemps(dir, maxAge)` — remove old temp files

## License
GPL-3.0 — see [LICENSE](LICENSE).
