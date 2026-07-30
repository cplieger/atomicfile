package atomicfile

import (
	"context"
	"fmt"
	"os"
)

// ReadBoundedInRoot opens name inside root and reads it under maxBytes, confining
// the read to root's tree: a symlink or ".." component in name can never redirect
// the read outside it (Go 1.24+ *os.Root).
//
// It is the read-side counterpart to WriteFileInRoot. Without it, a caller that
// writes through a confined root has to hand-roll the read half — open through the
// root, require a regular file, avoid blocking on a FIFO, then delegate to
// ReadBoundedFile — which is exactly the sequence this package already owns on the
// write side. Every consumer that hand-rolls it re-derives the same three
// non-obvious details, and one of them getting it wrong is a confinement bypass.
//
// Only regular files are read. A named pipe, device node or socket planted in the
// tree is rejected rather than opened, and the open is non-blocking so it cannot
// stall a caller even momentarily: open(2) on a FIFO with no writer blocks
// indefinitely, which for a single-goroutine scanner means a permanent hang.
//
// The open half is OpenRegularInRoot and the read half is ReadBoundedFile: this
// function is exactly their composition, so a caller needing the descriptor
// itself — to stream the file, or to record its FileInfo as a FileIdentity from
// the same observation the bytes came from — takes the two apart instead of
// re-deriving either.
//
// The caller owns root; ReadBoundedInRoot does not close it.
func ReadBoundedInRoot(ctx context.Context, root *os.Root, name string, maxBytes int64) ([]byte, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	f, _, err := OpenRegularInRoot(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadBoundedFile(ctx, f, maxBytes)
}
