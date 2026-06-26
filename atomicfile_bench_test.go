package atomicfile

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

func BenchmarkWriteFile(b *testing.B) {
	sizes := []int{64, 4096, 64 * 1024, 1024 * 1024}
	for _, size := range sizes {
		data := bytes.Repeat([]byte("x"), size)
		b.Run(fmt.Sprintf("size=%d/sync", size), func(b *testing.B) {
			dir := b.TempDir()
			path := dir + "/target"
			ctx := context.Background()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if _, err := WriteFile(ctx, path, data); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("size=%d/nosync", size), func(b *testing.B) {
			dir := b.TempDir()
			path := dir + "/target"
			ctx := context.Background()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				if _, err := WriteFile(ctx, path, data, WithNoSync()); err != nil {
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
			ctx := context.Background()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
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
	ctx := context.Background()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
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
