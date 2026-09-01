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

// ReadBounded opens path, validates its size against maxBytes, and reads it
// via an io.LimitReader. Returns ErrFileTooLarge if the file exceeds
// maxBytes, including if it grows past the limit during the read. ctx is
// checked before the open and before the read.
//
// Unlike the write primitives, ReadBounded does NOT refuse symlink targets:
// os.Open follows symlinks. Callers reading from a directory writable by a
// less-trusted principal should confine the path themselves: open through an
// *os.Root and read with ReadBoundedFile.
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

// ReadBoundedFile reads up to maxBytes from an already-open file, applying
// the same size validation as ReadBounded: returns ErrFileTooLarge if the
// file exceeds maxBytes, including if it grows past the limit during the
// read. ctx is checked before the size stat and before the read. The caller
// owns f; ReadBoundedFile does not close it.
//
// This is the seam for a caller that must open the file itself, most notably
// through an *os.Root to confine the path, which ReadBounded cannot do
// because os.Open follows symlinks.
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
