package atomicfile

import (
	"context"
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
// should resolve and confine the path themselves (e.g. filepath.EvalSymlinks
// plus a root check) before calling.
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
