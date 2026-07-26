package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
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
// The caller owns root; ReadBoundedInRoot does not close it.
func ReadBoundedInRoot(ctx context.Context, root *os.Root, name string, maxBytes int64) ([]byte, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	if root == nil {
		return nil, errors.New("atomicfile: nil root")
	}
	clean, err := validateRootName(name)
	if err != nil {
		return nil, err
	}

	// O_NONBLOCK has no effect on a regular file, the only thing this reads, and it
	// is what makes the FIFO case a rejection rather than a hang.
	f, err := root.OpenFile(clean, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Stat the OPEN HANDLE, not the path: checking the path and then opening it
	// leaves a window in which the two refer to different files.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s (type %s)", ErrNotRegular, clean, fi.Mode().Type())
	}
	return ReadBoundedFile(ctx, f, maxBytes)
}
