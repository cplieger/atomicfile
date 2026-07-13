package atomicfile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// saturateAdd returns a + b clamped to math.MaxInt64 on overflow.
func saturateAdd(a, b int64) int64 {
	sum := a + b
	if sum < a {
		return math.MaxInt64
	}
	return sum
}

// ReadBounded opens path, validates its size against maxBytes, and reads it via
// an io.LimitReader. Returns ErrFileTooLarge if the file exceeds maxBytes
// (including if it grows past the limit during the read). ctx is checked before
// the open and before the read.
//
// Unlike the write primitives, ReadBounded does NOT refuse symlink targets:
// os.Open follows symlinks, so a symlink at path is resolved and its target is
// read. Callers reading from a directory writable by a less-trusted principal
// should confine the path themselves: open the file through an *os.Root and
// read it with ReadBoundedFile, which applies the same size and context bounds.
func ReadBounded(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	cleanPath, err := validateAbsClean(path)
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	f, err := os.Open(cleanPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadBoundedFile(ctx, f, maxBytes)
}

// ReadBoundedFile reads up to maxBytes from an already-open file using the same
// size validation as ReadBounded: it returns ErrFileTooLarge if the file
// exceeds maxBytes (including if it grows past the limit during the read), and
// checks ctx before the size stat and before the read. The caller owns f;
// ReadBoundedFile does not close it.
//
// This is the seam for callers that must open the file themselves before
// reading it — most importantly, opening through an *os.Root (Go 1.24+) to
// confine the path to a trusted directory, which ReadBounded cannot do because
// os.Open follows symlinks. Open the file via the root, then read it here to get
// the identical bounds and context handling.
func ReadBoundedFile(ctx context.Context, f *os.File, maxBytes int64) ([]byte, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	if f == nil {
		return nil, errors.New("atomicfile: nil file")
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrFileTooLarge, fi.Size(), maxBytes)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("atomicfile: %w", ctxErr)
	}
	data, err := io.ReadAll(io.LimitReader(f, saturateAdd(maxBytes, 1)))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: file grew past %d byte limit during read", ErrFileTooLarge, maxBytes)
	}
	return data, nil
}
