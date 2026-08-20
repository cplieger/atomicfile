package atomicfile

import (
	"bytes"
	"fmt"
	"testing"
)

func BenchmarkWriteFile(b *testing.B) {
	sizes := []int{64, 4096, 64 * 1024, 1024 * 1024}
	for _, size := range sizes {
		data := bytes.Repeat([]byte("x"), size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			dir := b.TempDir()
			path := dir + "/target"
			ctx := b.Context()
			b.SetBytes(int64(size))
			b.ReportAllocs()
			// b.Loop excludes everything above it from the timing, so no
			// ResetTimer, and it keeps the call from being optimized away.
			for b.Loop() {
				if _, err := WriteFile(ctx, path, data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWriteReader(b *testing.B) {
	sizes := []int{4096, 1024 * 1024}
	for _, size := range sizes {
		data := bytes.Repeat([]byte("y"), size)
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			dir := b.TempDir()
			path := dir + "/target"
			ctx := b.Context()
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := WriteReader(ctx, path, bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPendingFileCommit(b *testing.B) {
	data := bytes.Repeat([]byte("z"), 4096)
	dir := b.TempDir()
	path := dir + "/target"
	ctx := b.Context()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		pf, err := NewPendingFile(ctx, path)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := pf.Write(data); err != nil {
			_ = pf.Cleanup()
			b.Fatal(err)
		}
		if _, err := pf.Commit(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMkdirChain separates the two WithMkdirMode paths, because only one of
// them pays for the per-level mode enforcement and parent fsync: a write into a
// directory that already exists creates nothing and costs what it always did,
// while a write that adds levels pays one directory fsync per level.
func BenchmarkMkdirChain(b *testing.B) {
	b.Run("existing_parent", func(b *testing.B) {
		dir := b.TempDir()
		ctx := b.Context()
		path := dir + "/target"
		b.ReportAllocs()
		for b.Loop() {
			if _, err := WriteFile(ctx, path, []byte("x"), WithMkdirMode(0o755)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("three_created_levels", func(b *testing.B) {
		base := b.TempDir()
		ctx := b.Context()
		n := 0
		b.ReportAllocs()
		for b.Loop() {
			n++
			// A fresh chain each iteration, so every one actually creates three
			// levels rather than measuring the already-exists path after the first.
			path := fmt.Sprintf("%s/r%d/a/b/target", base, n)
			if _, err := WriteFile(ctx, path, []byte("x"), WithMkdirMode(0o755)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
