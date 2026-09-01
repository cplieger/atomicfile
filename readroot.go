package atomicfile

import (
	"context"
	"fmt"
	"os"
)

// ReadBoundedInRoot opens name inside root and reads it under maxBytes,
// confining the read to root's tree: a symlink or ".." component in name
// can never redirect it outside (Go 1.24+ *os.Root). It is the read-side
// counterpart to WriteFileInRoot.
//
// Only regular files are read. A named pipe, device node or socket planted
// in the tree is rejected, and the open is non-blocking so a FIFO with no
// writer cannot stall the caller indefinitely.
//
// This is exactly the composition of OpenRegularInRoot and ReadBoundedFile;
// a caller needing the descriptor itself — to stream the file, or to record
// its FileInfo alongside the bytes — takes the two apart instead.
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
